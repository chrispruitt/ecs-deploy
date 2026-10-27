package internal

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	cwltypes "github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs/types"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	ecstypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
)

// Exit codes for a release command that did not succeed. The process exit code
// distinguishes "the command ran and failed" from "the command never ran" so a
// pipeline can treat an image-pull error differently from a failed migration.
const (
	// ExitCommandFailed: the container ran and exited non-zero.
	ExitCommandFailed = 1
	// ExitFailedToStart: ECS could not place or start the task, or the
	// container stopped without ever reporting an exit code.
	ExitFailedToStart = 2
	// ExitTimeout: the container was still running when --release-timeout
	// elapsed; the task has been stopped.
	ExitTimeout = 3
)

// ExitError carries the process exit code a failed release command maps to.
type ExitError struct {
	Code int
	Err  error
}

func (e *ExitError) Error() string { return e.Err.Error() }
func (e *ExitError) Unwrap() error { return e.Err }

// ExitCode returns the process exit code for an error: the release command's
// own code when it carries one, otherwise 1.
func ExitCode(err error) int {
	var ee *ExitError
	if errors.As(err, &ee) {
		return ee.Code
	}
	return 1
}

const (
	releaseStartedBy  = "ecs-deploy"
	releaseStopReason = "ecs-deploy: release command finished"
)

// Package-level so tests can shorten them.
var (
	releasePollInterval = 5 * time.Second
	logPollInterval     = 2 * time.Second
)

// releaseTask describes a one-off task to run from a task definition revision
// before the service rolls to it.
type releaseTask struct {
	Cluster           string
	Service           string
	TaskDefinitionARN string
	Container         string
	Command           []string
	Timeout           time.Duration
	Launch            launchConfig
	LogConfig         *ecstypes.LogConfiguration // the container's, for tailing
}

// launchConfig is what RunTask needs copied from the service so the one-off
// task lands on the same infrastructure and network as the service's own
// tasks.
type launchConfig struct {
	LaunchType               ecstypes.LaunchType
	CapacityProviderStrategy []ecstypes.CapacityProviderStrategyItem
	PlatformVersion          *string
	NetworkConfiguration     *ecstypes.NetworkConfiguration
	// Tags are the service's own tags, applied to the one-off task directly.
	// RunTask rejects propagateTags=SERVICE (that value is only valid for
	// tasks a service launches), so they are copied rather than propagated.
	Tags []ecstypes.Tag
}

// launchConfigFromService copies the launch settings a service uses. A service
// has either a launch type or a capacity provider strategy, never both.
func launchConfigFromService(svc ecstypes.Service) launchConfig {
	lc := launchConfig{
		PlatformVersion:      svc.PlatformVersion,
		NetworkConfiguration: svc.NetworkConfiguration,
		Tags:                 userTags(svc.Tags),
	}
	if len(svc.CapacityProviderStrategy) > 0 {
		lc.CapacityProviderStrategy = svc.CapacityProviderStrategy
	} else {
		lc.LaunchType = svc.LaunchType
	}
	return lc
}

// userTags drops tags with the reserved "aws:" prefix, which ECS manages
// itself and refuses in a RunTask request.
func userTags(tags []ecstypes.Tag) []ecstypes.Tag {
	var out []ecstypes.Tag
	for _, tag := range tags {
		if !strings.HasPrefix(strings.ToLower(aws.ToString(tag.Key)), "aws:") {
			out = append(out, tag)
		}
	}
	return out
}

// newClientToken returns a fresh RunTask idempotency token. The SDK fills
// ClientToken on the caller's input struct when it is nil, so a second RunTask
// sent with the same (modified) struct would reuse the first token and ECS
// answers ConflictException. Every attempt therefore gets its own token.
func newClientToken() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("crypto/rand: %v", err))
	}
	return hex.EncodeToString(b)
}

// runTaskInput builds a fresh RunTask request, with its own client token, each
// time it is called.
func (t releaseTask) runTaskInput() *ecs.RunTaskInput {
	in := &ecs.RunTaskInput{
		ClientToken:          aws.String(newClientToken()),
		Cluster:              aws.String(t.Cluster),
		TaskDefinition:       aws.String(t.TaskDefinitionARN),
		Count:                aws.Int32(1),
		StartedBy:            aws.String(releaseStartedBy),
		EnableExecuteCommand: false,
		PlatformVersion:      t.Launch.PlatformVersion,
		NetworkConfiguration: t.Launch.NetworkConfiguration,
		Overrides: &ecstypes.TaskOverride{
			ContainerOverrides: []ecstypes.ContainerOverride{{
				Name:    aws.String(t.Container),
				Command: t.Command,
			}},
		},
	}
	if len(t.Launch.CapacityProviderStrategy) > 0 {
		in.CapacityProviderStrategy = t.Launch.CapacityProviderStrategy
	} else {
		in.LaunchType = t.Launch.LaunchType
	}
	if len(t.Launch.Tags) > 0 {
		in.Tags = t.Launch.Tags
	}
	return in
}

// runReleaseTask starts the task, tails the container's logs while it runs,
// and returns nil only when the named container exited 0. Any other outcome
// is an *ExitError. The task is always stopped before returning: essential
// sidecars (log routers, collectors) keep a task alive after the application
// container exits, so finishing is detected per container, not per task.
func runReleaseTask(ctx context.Context, clients *Clients, t releaseTask) error {
	return runReleaseTaskTo(ctx, clients, t, os.Stdout)
}

func runReleaseTaskTo(ctx context.Context, clients *Clients, t releaseTask, out io.Writer) error {
	fmt.Fprintf(out, "Running release command in container %q from %s:\n  %s\n", t.Container, t.TaskDefinitionARN, quoteCommand(t.Command))

	in := t.runTaskInput()
	resp, err := clients.ECS.RunTask(ctx, in)
	if err != nil && len(in.Tags) > 0 {
		// Tags are best-effort: retry once without them rather than fail a
		// release over a tagging permission. The retry is a new request with
		// a new client token; reusing the first one is a ConflictException.
		fmt.Fprintf(os.Stderr, "warning: RunTask with tags failed, retrying without tags: %v\n", err)
		retry := t.runTaskInput()
		retry.Tags = nil
		resp, err = clients.ECS.RunTask(ctx, retry)
	}
	if err != nil {
		return &ExitError{Code: ExitFailedToStart, Err: fmt.Errorf("failed to start release task: %w", err)}
	}
	if len(resp.Failures) > 0 {
		reasons := make([]string, 0, len(resp.Failures))
		for _, f := range resp.Failures {
			reasons = append(reasons, strings.TrimSpace(aws.ToString(f.Reason)+" "+aws.ToString(f.Detail)))
		}
		return &ExitError{Code: ExitFailedToStart, Err: fmt.Errorf("release task failed to start: %s", strings.Join(reasons, "; "))}
	}
	if len(resp.Tasks) == 0 {
		return &ExitError{Code: ExitFailedToStart, Err: errors.New("release task failed to start: RunTask returned no task")}
	}

	taskARN := aws.ToString(resp.Tasks[0].TaskArn)
	fmt.Fprintf(out, "Started task: %s\n", taskARN)

	stopTail := startLogTail(ctx, clients, t, taskARN, out)
	defer stopTail()

	deadline := time.Now().Add(t.Timeout)
	ticker := time.NewTicker(releasePollInterval)
	defer ticker.Stop()

	start := time.Now()
	lastStatus := ""

	for {
		select {
		case <-ctx.Done():
			// Ctrl-C: the task was started on the operator's behalf, so do not
			// leave it running unattended.
			stopTask(context.WithoutCancel(ctx), clients.ECS, t.Cluster, taskARN, "ecs-deploy: interrupted", out)
			return ctx.Err()

		case <-ticker.C:
			if time.Now().After(deadline) {
				stopTask(ctx, clients.ECS, t.Cluster, taskARN, "ecs-deploy: release command timed out", out)
				return &ExitError{Code: ExitTimeout, Err: fmt.Errorf("release command timed out after %s (task %s stopped)", t.Timeout, taskARN)}
			}

			task, err := describeTask(ctx, clients.ECS, t.Cluster, taskARN)
			if err != nil {
				return err
			}

			taskStatus := aws.ToString(task.LastStatus)
			container := findTaskContainer(task.Containers, t.Container)
			containerStatus := ""
			if container != nil {
				containerStatus = aws.ToString(container.LastStatus)
			}

			status := fmt.Sprintf("task=%s container=%s", taskStatus, containerStatus)
			if status != lastStatus {
				fmt.Fprintf(out, "[%s] %s\n", time.Since(start).Round(time.Second), status)
				lastStatus = status
			}

			containerDone := containerStatus == "STOPPED"
			taskDone := taskStatus == "STOPPED"
			if !containerDone && !taskDone {
				continue
			}

			if !taskDone {
				// The application container is finished but sidecars keep the
				// task up; stop it so they go too.
				stopTask(ctx, clients.ECS, t.Cluster, taskARN, releaseStopReason, out)
			}

			stopTail() // drain whatever the container wrote last
			return releaseOutcome(task, container, taskARN)
		}
	}
}

// releaseOutcome maps the final state of the task and the named container to
// an exit code: 0 is success, non-zero is a failed command, and no exit code
// at all means the container never ran (image pull failure, missing secret,
// task placement failure).
func releaseOutcome(task *ecstypes.Task, container *ecstypes.Container, taskARN string) error {
	stoppedReason := aws.ToString(task.StoppedReason)
	stopCode := string(task.StopCode)

	if container == nil || container.ExitCode == nil {
		detail := stoppedReason
		if container != nil && container.Reason != nil {
			detail = aws.ToString(container.Reason)
		}
		if detail == "" {
			detail = "no reason reported"
		}
		return &ExitError{
			Code: ExitFailedToStart,
			Err:  fmt.Errorf("release container did not start: %s (stopCode=%s, task=%s)", detail, stopCode, taskARN),
		}
	}

	if code := aws.ToInt32(container.ExitCode); code != 0 {
		return &ExitError{
			Code: ExitCommandFailed,
			Err: fmt.Errorf("release command exited with code %d (stopCode=%s, stoppedReason=%q, task=%s)",
				code, stopCode, stoppedReason, taskARN),
		}
	}

	return nil
}

func describeTask(ctx context.Context, client ecsAPI, cluster, taskARN string) (*ecstypes.Task, error) {
	resp, err := client.DescribeTasks(ctx, &ecs.DescribeTasksInput{
		Cluster: aws.String(cluster),
		Tasks:   []string{taskARN},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to describe release task %s: %w", taskARN, err)
	}
	for _, f := range resp.Failures {
		return nil, fmt.Errorf("failed to describe release task %s: %s", taskARN, aws.ToString(f.Reason))
	}
	if len(resp.Tasks) == 0 {
		return nil, fmt.Errorf("release task %s not found", taskARN)
	}
	return &resp.Tasks[0], nil
}

func stopTask(ctx context.Context, client ecsAPI, cluster, taskARN, reason string, out io.Writer) {
	_, err := client.StopTask(ctx, &ecs.StopTaskInput{
		Cluster: aws.String(cluster),
		Task:    aws.String(taskARN),
		Reason:  aws.String(reason),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to stop task %s: %v\n", taskARN, err)
		return
	}
	fmt.Fprintf(out, "Stopped task %s (%s)\n", taskARN, reason)
}

func findTaskContainer(containers []ecstypes.Container, name string) *ecstypes.Container {
	for i := range containers {
		if aws.ToString(containers[i].Name) == name {
			return &containers[i]
		}
	}
	return nil
}

// taskID is the last path segment of a task ARN, which is what awslogs uses in
// the stream name.
func taskID(taskARN string) string {
	return taskARN[strings.LastIndex(taskARN, "/")+1:]
}

// awslogsStream returns the log group, stream and region for a container that
// logs through the awslogs driver, or ok=false for any other driver.
func awslogsStream(lc *ecstypes.LogConfiguration, container, taskARN string) (group, stream, region string, ok bool) {
	if lc == nil || lc.LogDriver != ecstypes.LogDriverAwslogs {
		return "", "", "", false
	}
	group = lc.Options["awslogs-group"]
	prefix := lc.Options["awslogs-stream-prefix"]
	if group == "" || prefix == "" {
		return "", "", "", false
	}
	return group, prefix + "/" + container + "/" + taskID(taskARN), lc.Options["awslogs-region"], true
}

// startLogTail tails the container's awslogs stream in the background and
// returns a function that drains it one last time and waits for it to finish.
// Calling the returned function more than once is safe. Containers that log
// through any other driver get a hint instead of a tail.
func startLogTail(ctx context.Context, clients *Clients, t releaseTask, taskARN string, out io.Writer) func() {
	group, stream, region, ok := awslogsStream(t.LogConfig, t.Container, taskARN)
	if !ok {
		driver := "none"
		if t.LogConfig != nil {
			driver = string(t.LogConfig.LogDriver)
		}
		fmt.Fprintf(out, "logs: container uses %s log driver; query Loki for {ecs=%q} around %s\n",
			driver, t.Service, time.Now().UTC().Format(time.RFC3339))
		return func() {}
	}

	fmt.Fprintf(out, "logs: tailing %s %s\n", group, stream)

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		tailLogs(ctx, clients.logsClient(region), group, stream, stop, out)
	}()

	var stopped bool
	return func() {
		if !stopped {
			stopped = true
			close(stop)
		}
		<-done
	}
}

// tailLogs follows a log stream, printing events as they arrive, until stop is
// closed — then fetches once more so the last lines are not lost. A stream that
// does not exist yet (the container has not started) is not an error.
func tailLogs(ctx context.Context, client logsAPI, group, stream string, stop <-chan struct{}, out io.Writer) {
	var token *string

	fetch := func() bool {
		for {
			resp, err := client.GetLogEvents(ctx, &cloudwatchlogs.GetLogEventsInput{
				LogGroupName:  aws.String(group),
				LogStreamName: aws.String(stream),
				StartFromHead: aws.Bool(true),
				NextToken:     token,
			})
			if err != nil {
				var notFound *cwltypes.ResourceNotFoundException
				if errors.As(err, &notFound) || ctx.Err() != nil {
					return true
				}
				fmt.Fprintf(os.Stderr, "logs: %v (log tailing stopped)\n", err)
				return false
			}
			for _, e := range resp.Events {
				ts := time.UnixMilli(aws.ToInt64(e.Timestamp)).UTC().Format("15:04:05")
				fmt.Fprintf(out, "  %s | %s\n", ts, strings.TrimRight(aws.ToString(e.Message), "\n"))
			}
			// The forward token stops changing once the stream is exhausted.
			if resp.NextForwardToken == nil || aws.ToString(resp.NextForwardToken) == aws.ToString(token) {
				return true
			}
			token = resp.NextForwardToken
		}
	}

	for {
		if !fetch() {
			return
		}
		select {
		case <-stop:
			fetch()
			return
		case <-ctx.Done():
			return
		case <-time.After(logPollInterval):
		}
	}
}
