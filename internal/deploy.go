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

	// ReleaseCommand, when non-empty, is run as a one-off task from the new
	// revision before the service is updated; a non-zero exit aborts the deploy.
	ReleaseCommand []string
	ReleaseTimeout time.Duration
}

// revisionPlan is everything known about a task definition change before any
// write is made: the service, its current revision, and what the new revision
// will differ by. Shared by the deploy and run flows.
type revisionPlan struct {
	service       ecstypes.Service
	taskDefARN    string
	existing      *ecstypes.TaskDefinition
	tags          []ecstypes.Tag
	oldImage      string
	newImage      string // empty when the image is not changing
	secretChanges []secretChange
	ssmParams     map[string]string
}

func (p *revisionPlan) hasChanges() bool {
	return p.newImage != "" || len(p.secretChanges) > 0
}

// planRevision reads the service and its task definition and computes what a
// new revision would change, without touching anything.
func planRevision(ctx context.Context, clients *Clients, cluster, service, container, imageTag string, ssmPrefixes []string, forRelease bool) (*revisionPlan, error) {
	svc, err := describeService(ctx, clients.ECS, cluster, service, forRelease)
	if err != nil {
		return nil, err
	}
	p := &revisionPlan{service: svc, taskDefARN: aws.ToString(svc.TaskDefinition)}

	p.existing, p.tags, err = describeTaskDefinition(ctx, clients.ECS, p.taskDefARN)
	if err != nil {
		return nil, err
	}

	if findContainerByName(p.existing.ContainerDefinitions, container) == nil {
		names := make([]string, 0, len(p.existing.ContainerDefinitions))
		for _, c := range p.existing.ContainerDefinitions {
			names = append(names, aws.ToString(c.Name))
		}
		return nil, fmt.Errorf("container %q not found in task definition %s — available containers: %s", container, p.taskDefARN, strings.Join(names, ", "))
	}

	p.oldImage = containerImage(p.existing.ContainerDefinitions, container)
	if imageTag != "" {
		p.newImage = replaceImageTag(p.oldImage, imageTag)
	}

	if len(ssmPrefixes) > 0 {
		p.ssmParams, err = fetchSSMParameters(ctx, clients.SSM, ssmPrefixes)
		if err != nil {
			return nil, err
		}
		p.secretChanges, err = computeSecretChanges(p.existing.ContainerDefinitions, container, ssmPrefixes, p.ssmParams)
		if err != nil {
			return nil, err
		}
	}
	return p, nil
}

// containers returns the container definitions for the new revision: the
// existing ones with only the image tag and managed secrets changed.
func (p *revisionPlan) containers(container, imageTag string, ssmPrefixes []string) ([]ecstypes.ContainerDefinition, error) {
	containers := p.existing.ContainerDefinitions
	var err error
	if imageTag != "" {
		containers, err = updateContainerImage(containers, container, imageTag)
		if err != nil {
			return nil, fmt.Errorf("%w (task definition: %s)", err, p.taskDefARN)
		}
	}
	if len(ssmPrefixes) > 0 {
		containers = applyChangesToContainers(containers, container, ssmPrefixes, p.ssmParams)
	}
	return containers, nil
}

// releaseTaskFor builds the one-off task to run from a revision: the command is
// applied only as a RunTask container override, never to the revision itself.
func (p *revisionPlan) releaseTaskFor(cluster, service, container, revisionARN string, command []string, timeout time.Duration) releaseTask {
	var logConfig *ecstypes.LogConfiguration
	if c := findContainerByName(p.existing.ContainerDefinitions, container); c != nil {
		logConfig = c.LogConfiguration
	}
	return releaseTask{
		Cluster:           cluster,
		Service:           service,
		TaskDefinitionARN: revisionARN,
		Container:         container,
		Command:           command,
		Timeout:           timeout,
		Launch:            launchConfigFromService(p.service),
		LogConfig:         logConfig,
	}
}

const deploymentTimeout = 30 * time.Minute

var pollInterval = 10 * time.Second // var so tests can shorten it

// errRolloutFailed marks a deployment ECS itself reported as FAILED, as opposed
// to one whose outcome is unknown (timeout, interrupt, API error while polling).
// Only the former is certain not to end up running the new task definition.
var errRolloutFailed = errors.New("deployment FAILED")

// RunDeployment executes the full deployment sequence and returns the new task
// definition ARN. Returns ("", nil) when no changes are detected.
// On --no-wait, it returns after updating the service.
func RunDeployment(ctx context.Context, clients *Clients, cfg DeploymentConfig) (string, error) {
	plan, err := planRevision(ctx, clients, cfg.Cluster, cfg.Service, cfg.Container, cfg.ImageTag, cfg.SSMPrefixes, len(cfg.ReleaseCommand) > 0)
	if err != nil {
		return "", err
	}

	var deplCfg ecstypes.DeploymentConfiguration
	if plan.service.DeploymentConfiguration != nil {
		deplCfg = *plan.service.DeploymentConfiguration
	}
	var controller ecstypes.DeploymentController
	if plan.service.DeploymentController != nil {
		controller = *plan.service.DeploymentController
	}
	logDeploymentType(controller)

	if !plan.hasChanges() {
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

	printDeploymentPlan(cfg.Container, plan.oldImage, plan.newImage, plan.secretChanges, len(cfg.SSMPrefixes) > 0, versionParam, cfg.ReleaseCommand, cfg.ReleaseTimeout)

	if !cfg.AutoApprove {
		ok, err := confirmChanges(ctx, os.Stdin)
		if err != nil {
			return "", err
		}
		if !ok {
			return "", fmt.Errorf("deployment cancelled by user")
		}
	}

	containers, err := plan.containers(cfg.Container, cfg.ImageTag, cfg.SSMPrefixes)
	if err != nil {
		return "", err
	}

	var newARN string
	if len(cfg.ReleaseCommand) > 0 {
		// The release command gates everything that follows: the revision is
		// registered first (registering changes nothing about the service), the
		// command runs from it, and only on success does the deploy proper —
		// version parameter, service update — begin. A failed release therefore
		// leaves no version parameter to roll back and the service untouched.
		newARN, err = registerTaskDefinition(ctx, clients.ECS, plan.existing, containers, plan.tags)
		if err != nil {
			return "", err
		}
		fmt.Printf("Registered new task definition: %s\n", newARN)

		task := plan.releaseTaskFor(cfg.Cluster, cfg.Service, cfg.Container, newARN, cfg.ReleaseCommand, cfg.ReleaseTimeout)
		if err := runReleaseTask(ctx, clients, task); err != nil {
			return "", fmt.Errorf("release command failed, service not updated: %w", err)
		}
		fmt.Println("Release command succeeded.")

		if versionParam != nil && versionParam.changed() {
			if err := writeVersionParameter(ctx, clients.SSM, versionParam); err != nil {
				return "", err
			}
			fmt.Printf("Version parameter %s set to %s\n", versionParam.path, versionParam.to)
		}
	} else {
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

		// From here on the version parameter may already name a tag that is not
		// running — the exact condition it exists to prevent, and invisible until
		// infrastructure-as-code acts on it. Where the new tag is certain not to be
		// running, the parameter is rolled back; where the outcome is unknown, it is
		// left alone and the failure says so rather than exiting silently.
		newARN, err = registerTaskDefinition(ctx, clients.ECS, plan.existing, containers, plan.tags)
		if err != nil {
			return "", withVersionRollback(ctx, clients.SSM, err, versionParam)
		}
		fmt.Printf("Registered new task definition: %s\n", newARN)
	}

	if err := updateService(ctx, clients.ECS, cfg.Cluster, cfg.Service, newARN); err != nil {
		return "", withVersionRollback(ctx, clients.SSM, err, versionParam)
	}

	if cfg.NoWait {
		return newARN, nil
	}

	cbRollbackEnabled := deplCfg.DeploymentCircuitBreaker != nil &&
		deplCfg.DeploymentCircuitBreaker.Enable &&
		deplCfg.DeploymentCircuitBreaker.Rollback

	if err := pollDeployment(ctx, clients.ECS, cfg.Cluster, cfg.Service, newARN, cbRollbackEnabled); err != nil {
		// A timeout or interrupt leaves the rollout running and it may still
		// complete; restoring the old tag then would create the disagreement
		// instead of fixing it.
		if errors.Is(err, errRolloutFailed) {
			return "", withVersionRollback(ctx, clients.SSM, err, versionParam)
		}
		return "", withVersionWarning(err, versionParam)
	}

	return newARN, nil
}

// withVersionRollback restores an already-written version parameter after a
// deployment that is certain not to be running the new tag, and returns the
// original error unchanged. If the restore itself fails, it falls back to the
// manual guidance from withVersionWarning.
func withVersionRollback(ctx context.Context, client versionParameterAPI, err error, v *versionParameter) error {
	// Restore even if the deploy was interrupted at the same moment.
	if restoreErr := restoreVersionParameter(context.WithoutCancel(ctx), client, v); restoreErr != nil {
		fmt.Fprintf(os.Stderr, "\n%v\n", restoreErr)
		return withVersionWarning(err, v)
	}
	if msg := describeVersionRestore(v); msg != "" {
		fmt.Fprintf(os.Stderr, "\n%s", msg)
	}
	return err
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

func printDeploymentPlan(containerName, oldImage, newImage string, secretChanges []secretChange, ssmRequested bool, versionParam *versionParameter, releaseCommand []string, releaseTimeout time.Duration) {
	fmt.Printf("\nDeployment plan for container %q:\n", containerName)

	if len(releaseCommand) > 0 {
		fmt.Printf("  release: run `%s` in container %q from the new revision (timeout %s)\n", quoteCommand(releaseCommand), containerName, releaseTimeout)
		fmt.Println("           the service is updated only if it exits 0")
	}

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

	printSecretChanges(secretChanges, ssmRequested)

	fmt.Println()
}

func printSecretChanges(secretChanges []secretChange, ssmRequested bool) {
	if !ssmRequested {
		return
	}
	if len(secretChanges) == 0 {
		fmt.Println("  secrets: no changes")
		return
	}
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

// describeService returns the service. Tags are only requested when a release
// task will be run from it (they decide whether RunTask propagates them), so a
// plain deploy makes exactly the same call it always has.
func describeService(ctx context.Context, client ecsAPI, cluster, service string, includeTags bool) (ecstypes.Service, error) {
	in := &ecs.DescribeServicesInput{
		Cluster:  aws.String(cluster),
		Services: []string{service},
	}
	if includeTags {
		in.Include = []ecstypes.ServiceField{ecstypes.ServiceFieldTags}
	}
	resp, err := client.DescribeServices(ctx, in)
	if err != nil {
		return ecstypes.Service{}, fmt.Errorf("failed to describe ECS service %q in cluster %q: %w", service, cluster, err)
	}

	for _, f := range resp.Failures {
		if aws.ToString(f.Reason) == "MISSING" {
			return ecstypes.Service{}, fmt.Errorf("ECS service %q not found in cluster %q — verify the cluster and service names are correct", service, cluster)
		}
	}

	if len(resp.Services) == 0 {
		return ecstypes.Service{}, fmt.Errorf("ECS service %q not found in cluster %q — verify the cluster and service names are correct", service, cluster)
	}

	return resp.Services[0], nil
}

func describeTaskDefinition(ctx context.Context, client ecsAPI, taskDefARN string) (*ecstypes.TaskDefinition, []ecstypes.Tag, error) {
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
	client ecsAPI,
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

func updateService(ctx context.Context, client ecsAPI, cluster, service, taskDefARN string) error {
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
	client ecsAPI,
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
						return fmt.Errorf("%w: ECS circuit breaker triggered auto-rollback — %s", errRolloutFailed, reason)
					}
					return fmt.Errorf("%w: ECS circuit breaker triggered (rollback disabled) — %s", errRolloutFailed, reason)
				}
				return fmt.Errorf("%w: rollout state is FAILED — %s", errRolloutFailed, reason)

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
