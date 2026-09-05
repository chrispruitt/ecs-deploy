package internal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	ecstypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
)

// DeploymentConfig holds the resolved CLI flags passed from cmd/.
type DeploymentConfig struct {
	Cluster          string
	Service          string
	Container        string
	ImageTag         string // empty means skip image update
	SSMPrefixes      []string
	VersionParameter string // empty means do not record the deployed tag
	AutoApprove      bool
	NoWait           bool
}

const deploymentTimeout = 30 * time.Minute
const pollInterval = 10 * time.Second

// RunDeployment executes the full deployment sequence and returns the new task
// definition ARN. Returns ("", nil) when no changes are detected.
// On --no-wait, it returns after updating the service.
func RunDeployment(ctx context.Context, clients *Clients, cfg DeploymentConfig) (string, error) {
	taskDefARN, deplCfg, controller, err := describeService(ctx, clients.ECS, cfg.Cluster, cfg.Service)
	if err != nil {
		return "", err
	}

	logDeploymentType(controller)

	existing, tags, err := describeTaskDefinition(ctx, clients.ECS, taskDefARN)
	if err != nil {
		return "", err
	}

	// Compute what will change before touching anything.
	oldImage := containerImage(existing.ContainerDefinitions, cfg.Container)
	var newImage string
	if cfg.ImageTag != "" {
		newImage = replaceImageTag(oldImage, cfg.ImageTag)
	}

	var secretChanges []secretChange
	var ssmParams map[string]string
	if len(cfg.SSMPrefixes) > 0 {
		ssmParams, err = fetchSSMParameters(ctx, clients.SSM, cfg.SSMPrefixes)
		if err != nil {
			return "", err
		}
		secretChanges, err = computeSecretChanges(existing.ContainerDefinitions, cfg.Container, cfg.SSMPrefixes, ssmParams)
		if err != nil {
			return "", err
		}
	}

	if newImage == "" && len(secretChanges) == 0 {
		fmt.Println("No changes detected. Nothing to deploy.")
		return "", nil
	}

	// Read the version parameter before the plan is printed so the operator sees
	// the value it will move from, not just the value it will move to.
	var versionParam *versionParameter
	if cfg.VersionParameter != "" {
		versionParam, err = readVersionParameter(ctx, clients.SSM, cfg.VersionParameter, cfg.ImageTag)
		if err != nil {
			return "", err
		}
	}

	printDeploymentPlan(cfg.Container, oldImage, newImage, secretChanges, len(cfg.SSMPrefixes) > 0, versionParam)

	if !cfg.AutoApprove {
		ok, err := confirmChanges(ctx, os.Stdin)
		if err != nil {
			return "", err
		}
		if !ok {
			return "", fmt.Errorf("deployment cancelled by user")
		}
	}

	// Record the deployed tag BEFORE the service is updated. The parameter is
	// commonly injected into the container too, and ECS resolves that at task
	// start — writing it afterwards leaves the new task reporting the version it
	// replaced. Writing first also means a failure here aborts before anything
	// about the service has changed.
	if versionParam != nil && versionParam.changed() {
		if err := writeVersionParameter(ctx, clients.SSM, versionParam); err != nil {
			return "", err
		}
		fmt.Printf("Version parameter %s set to %s\n", versionParam.path, versionParam.to)
	}

	// Apply changes.
	containers := existing.ContainerDefinitions
	if cfg.ImageTag != "" {
		containers, err = updateContainerImage(containers, cfg.Container, cfg.ImageTag)
		if err != nil {
			return "", fmt.Errorf("%w (task definition: %s)", err, taskDefARN)
		}
	}
	if len(cfg.SSMPrefixes) > 0 {
		containers = applyChangesToContainers(containers, cfg.Container, cfg.SSMPrefixes, ssmParams)
	}

	// From here on the version parameter may already name a tag that is not
	// running. Every failure path has to say so rather than exit silently — a
	// parameter disagreeing with the service is the exact condition it exists to
	// prevent, and it is invisible until infrastructure-as-code acts on it.
	newARN, err := registerTaskDefinition(ctx, clients.ECS, existing, containers, tags)
	if err != nil {
		return "", withVersionWarning(err, versionParam)
	}
	fmt.Printf("Registered new task definition: %s\n", newARN)

	if err := updateService(ctx, clients.ECS, cfg.Cluster, cfg.Service, newARN); err != nil {
		return "", withVersionWarning(err, versionParam)
	}

	if cfg.NoWait {
		return newARN, nil
	}

	cbRollbackEnabled := deplCfg.DeploymentCircuitBreaker != nil &&
		deplCfg.DeploymentCircuitBreaker.Enable &&
		deplCfg.DeploymentCircuitBreaker.Rollback

	if err := pollDeployment(ctx, clients.ECS, cfg.Cluster, cfg.Service, newARN, cbRollbackEnabled); err != nil {
		return "", withVersionWarning(err, versionParam)
	}

	return newARN, nil
}

// withVersionWarning prints the rollback guidance for an already-written version
// parameter and returns the original error unchanged, so callers can wrap a
// failure path without altering what the command reports.
func withVersionWarning(err error, v *versionParameter) error {
	if msg := describeVersionRollback(v); msg != "" {
		fmt.Fprintf(os.Stderr, "\n%s", msg)
	}
	return err
}

// containerImage returns the current image string for the named container.
func containerImage(containers []ecstypes.ContainerDefinition, name string) string {
	for _, c := range containers {
		if aws.ToString(c.Name) == name {
			return aws.ToString(c.Image)
		}
	}
	return ""
}

func printDeploymentPlan(containerName, oldImage, newImage string, secretChanges []secretChange, ssmRequested bool, versionParam *versionParameter) {
	fmt.Printf("\nDeployment plan for container %q:\n", containerName)

	if newImage != "" {
		fmt.Printf("  image:  %s\n          -> %s\n", oldImage, newImage)
	}

	if versionParam != nil {
		switch {
		case !versionParam.existed:
			fmt.Printf("  version: %s\n           (does not exist) -> %s\n", versionParam.path, versionParam.to)
		case versionParam.changed():
			fmt.Printf("  version: %s\n           %s -> %s\n", versionParam.path, versionParam.from, versionParam.to)
		default:
			fmt.Printf("  version: %s already %s\n", versionParam.path, versionParam.to)
		}
	}

	if ssmRequested {
		if len(secretChanges) == 0 {
			fmt.Println("  secrets: no changes")
		} else {
			fmt.Println("  secrets:")
			for _, c := range secretChanges {
				switch c.action() {
				case "add":
					fmt.Printf("    + %-30s %s\n", c.name, c.to)
				case "remove":
					fmt.Printf("    - %-30s %s\n", c.name, c.from)
				case "update":
					fmt.Printf("    ~ %-30s %s\n      %-30s %s\n", c.name, c.from, "->", c.to)
				}
			}
		}
	}

	fmt.Println()
}

func describeService(ctx context.Context, client *ecs.Client, cluster, service string) (
	taskDefARN string,
	deplCfg ecstypes.DeploymentConfiguration,
	controller ecstypes.DeploymentController,
	err error,
) {
	resp, err := client.DescribeServices(ctx, &ecs.DescribeServicesInput{
		Cluster:  aws.String(cluster),
		Services: []string{service},
	})
	if err != nil {
		return "", deplCfg, controller, fmt.Errorf("failed to describe ECS service %q in cluster %q: %w", service, cluster, err)
	}

	for _, f := range resp.Failures {
		if aws.ToString(f.Reason) == "MISSING" {
			return "", deplCfg, controller, fmt.Errorf("ECS service %q not found in cluster %q — verify the cluster and service names are correct", service, cluster)
		}
	}

	if len(resp.Services) == 0 {
		return "", deplCfg, controller, fmt.Errorf("ECS service %q not found in cluster %q — verify the cluster and service names are correct", service, cluster)
	}

	svc := resp.Services[0]
	if svc.DeploymentConfiguration != nil {
		deplCfg = *svc.DeploymentConfiguration
	}
	if svc.DeploymentController != nil {
		controller = *svc.DeploymentController
	}

	return aws.ToString(svc.TaskDefinition), deplCfg, controller, nil
}

func describeTaskDefinition(ctx context.Context, client *ecs.Client, taskDefARN string) (*ecstypes.TaskDefinition, []ecstypes.Tag, error) {
	resp, err := client.DescribeTaskDefinition(ctx, &ecs.DescribeTaskDefinitionInput{
		TaskDefinition: aws.String(taskDefARN),
		Include:        []ecstypes.TaskDefinitionField{ecstypes.TaskDefinitionFieldTags},
	})
	if err != nil {
		return nil, nil, fmt.Errorf("failed to describe task definition %q: %w", taskDefARN, err)
	}
	return resp.TaskDefinition, resp.Tags, nil
}

func updateContainerImage(containers []ecstypes.ContainerDefinition, name, newTag string) ([]ecstypes.ContainerDefinition, error) {
	result := make([]ecstypes.ContainerDefinition, len(containers))
	copy(result, containers)

	found := false
	for i, c := range result {
		if aws.ToString(c.Name) == name {
			updated := replaceImageTag(aws.ToString(c.Image), newTag)
			result[i].Image = aws.String(updated)
			found = true
			break
		}
	}

	if !found {
		names := make([]string, 0, len(containers))
		for _, c := range containers {
			names = append(names, aws.ToString(c.Name))
		}
		return nil, fmt.Errorf("container %q not found in task definition — available containers: %s", name, strings.Join(names, ", "))
	}

	return result, nil
}

// replaceImageTag replaces the tag portion of a Docker image reference.
// Handles registry:port/repo:tag and digest (@sha256:...) forms.
func replaceImageTag(image, newTag string) string {
	if i := strings.LastIndex(image, "@"); i != -1 {
		image = image[:i]
	}
	slashIdx := strings.LastIndex(image, "/")
	colonIdx := strings.LastIndex(image, ":")
	if colonIdx > slashIdx {
		image = image[:colonIdx]
	}
	return image + ":" + newTag
}

func registerTaskDefinition(
	ctx context.Context,
	client *ecs.Client,
	existing *ecstypes.TaskDefinition,
	updatedContainers []ecstypes.ContainerDefinition,
	tags []ecstypes.Tag,
) (string, error) {
	// JSON round-trip: marshal the full TaskDefinition and unmarshal into
	// RegisterTaskDefinitionInput. Fields that only exist on the output type
	// (ARN, revision, status, timestamps, etc.) are naturally dropped because
	// RegisterTaskDefinitionInput has no matching fields for them.
	// This means any new writable fields AWS adds to the API are picked up
	// automatically after a SDK version bump — no manual field listing needed.
	b, err := json.Marshal(existing)
	if err != nil {
		return "", fmt.Errorf("failed to marshal task definition: %w", err)
	}

	var input ecs.RegisterTaskDefinitionInput
	if err := json.Unmarshal(b, &input); err != nil {
		return "", fmt.Errorf("failed to unmarshal task definition: %w", err)
	}

	input.ContainerDefinitions = updatedContainers
	input.Tags = tags

	resp, err := client.RegisterTaskDefinition(ctx, &input)
	if err != nil {
		return "", fmt.Errorf("failed to register new task definition: %w", err)
	}
	return aws.ToString(resp.TaskDefinition.TaskDefinitionArn), nil
}

func updateService(ctx context.Context, client *ecs.Client, cluster, service, taskDefARN string) error {
	_, err := client.UpdateService(ctx, &ecs.UpdateServiceInput{
		Cluster:        aws.String(cluster),
		Service:        aws.String(service),
		TaskDefinition: aws.String(taskDefARN),
	})
	if err != nil {
		return fmt.Errorf("failed to update ECS service %q in cluster %q: %w", service, cluster, err)
	}
	fmt.Printf("Updated service %q to use task definition %s\n", service, taskDefARN)
	return nil
}

func pollDeployment(
	ctx context.Context,
	client *ecs.Client,
	cluster, service, targetTaskDefARN string,
	cbRollbackEnabled bool,
) error {
	deadline := time.Now().Add(deploymentTimeout)
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	start := time.Now()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case <-ticker.C:
			if time.Now().After(deadline) {
				return errors.New("deployment timed out after 30 minutes — the service may still be deploying; check the AWS console")
			}

			resp, err := client.DescribeServices(ctx, &ecs.DescribeServicesInput{
				Cluster:  aws.String(cluster),
				Services: []string{service},
			})
			if err != nil {
				return fmt.Errorf("failed to describe ECS service %q in cluster %q: %w", service, cluster, err)
			}

			if len(resp.Services) == 0 {
				return fmt.Errorf("ECS service %q not found in cluster %q — verify the cluster and service names are correct", service, cluster)
			}

			svc := resp.Services[0]
			d := findDeployment(svc.Deployments, targetTaskDefARN)
			if d == nil {
				continue
			}

			elapsed := time.Since(start).Round(time.Second)
			switch d.RolloutState {
			case ecstypes.DeploymentRolloutStateCompleted:
				return nil

			case ecstypes.DeploymentRolloutStateFailed:
				reason := aws.ToString(d.RolloutStateReason)
				if strings.Contains(strings.ToLower(reason), "circuit breaker") {
					if cbRollbackEnabled {
						return fmt.Errorf("deployment FAILED: ECS circuit breaker triggered auto-rollback — %s", reason)
					}
					return fmt.Errorf("deployment FAILED: ECS circuit breaker triggered (rollback disabled) — %s", reason)
				}
				return fmt.Errorf("deployment FAILED: rollout state is FAILED — %s", reason)

			case ecstypes.DeploymentRolloutStateInProgress:
				fmt.Printf("[%s] Deployment in progress: running=%d/%d pending=%d failed=%d\n",
					elapsed,
					d.RunningCount,
					d.DesiredCount,
					d.PendingCount,
					d.FailedTasks,
				)
			}
		}
	}
}

func logDeploymentType(controller ecstypes.DeploymentController) {
	switch controller.Type {
	case ecstypes.DeploymentControllerTypeCodeDeploy:
		fmt.Println("Deployment type: Blue/green (CodeDeploy)")
		fmt.Println("Warning: this tool uses UpdateService which has no effect on CodeDeploy-managed services. Use the CodeDeploy console or API to deploy.")
	case ecstypes.DeploymentControllerTypeExternal:
		fmt.Println("Deployment type: External")
	default:
		fmt.Println("Deployment type: Rolling update (ECS)")
	}
}

func findDeployment(deployments []ecstypes.Deployment, targetTaskDefARN string) *ecstypes.Deployment {
	for i := range deployments {
		if aws.ToString(deployments[i].TaskDefinition) == targetTaskDefARN {
			return &deployments[i]
		}
	}
	return nil
}
