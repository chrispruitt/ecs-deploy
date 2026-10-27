package internal

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	cwltypes "github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs/types"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	ecstypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
)

// fastPolling shortens every poll interval for the duration of a test.
func fastPolling(t *testing.T) {
	t.Helper()
	oldRelease, oldLog, oldDeploy := releasePollInterval, logPollInterval, pollInterval
	releasePollInterval, logPollInterval, pollInterval = time.Millisecond, time.Millisecond, time.Millisecond
	t.Cleanup(func() { releasePollInterval, logPollInterval, pollInterval = oldRelease, oldLog, oldDeploy })
}

func testReleaseTask(timeout time.Duration) releaseTask {
	return releaseTask{
		Cluster:           testCluster,
		Service:           testService,
		TaskDefinitionARN: testRegisterARN,
		Container:         testContainer,
		Command:           []string{"migrate", "up"},
		Timeout:           timeout,
		Launch:            launchConfigFromService(testFargateService()),
		LogConfig:         &ecstypes.LogConfiguration{LogDriver: ecstypes.LogDriverAwsfirelens},
	}
}

func exitCodeOf(t *testing.T, err error) int {
	t.Helper()
	var ee *ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("want *ExitError, got %T: %v", err, err)
	}
	return ee.Code
}

func TestReleaseOutcome(t *testing.T) {
	task := &ecstypes.Task{
		StopCode:      ecstypes.TaskStopCodeEssentialContainerExited,
		StoppedReason: aws.String("Essential container in task exited"),
	}

	t.Run("exit 0 is success", func(t *testing.T) {
		c := &ecstypes.Container{Name: aws.String(testContainer), ExitCode: aws.Int32(0)}
		if err := releaseOutcome(task, c, testTaskARN); err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("non-zero exit is a failed command naming the code, reason and task", func(t *testing.T) {
		c := &ecstypes.Container{Name: aws.String(testContainer), ExitCode: aws.Int32(7)}
		err := releaseOutcome(task, c, testTaskARN)
		if got := exitCodeOf(t, err); got != ExitCommandFailed {
			t.Errorf("exit code = %d, want %d", got, ExitCommandFailed)
		}
		for _, want := range []string{"code 7", "EssentialContainerExited", "Essential container in task exited", testTaskARN} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error missing %q: %v", want, err)
			}
		}
	})

	t.Run("no exit code means the container never ran", func(t *testing.T) {
		pullFailed := &ecstypes.Task{
			StopCode:      ecstypes.TaskStopCodeTaskFailedToStart,
			StoppedReason: aws.String("CannotPullContainerError: image not found"),
		}
		c := &ecstypes.Container{Name: aws.String(testContainer), Reason: aws.String("CannotPullContainerError: image not found")}
		err := releaseOutcome(pullFailed, c, testTaskARN)
		if got := exitCodeOf(t, err); got != ExitFailedToStart {
			t.Errorf("exit code = %d, want %d", got, ExitFailedToStart)
		}
		if !strings.Contains(err.Error(), "CannotPullContainerError") {
			t.Errorf("error should carry the reason: %v", err)
		}

		if got := exitCodeOf(t, releaseOutcome(pullFailed, nil, testTaskARN)); got != ExitFailedToStart {
			t.Errorf("missing container: exit code = %d, want %d", got, ExitFailedToStart)
		}
	})
}

// With an essential sidecar the task stays RUNNING after the application
// container exits, so the runner must finish on the container's state and stop
// the task itself.
func TestRunReleaseTask_SidecarKeepsTaskRunning(t *testing.T) {
	fastPolling(t)
	e := &fakeECS{describeTasks: []*ecs.DescribeTasksOutput{
		taskState("PROVISIONING", "PENDING", nil),
		taskState("RUNNING", "RUNNING", nil),
		taskState("RUNNING", "STOPPED", aws.Int32(0)),
	}}

	err := runReleaseTaskTo(context.Background(), newTestClients(e, &fakeSSM{}, nil), testReleaseTask(time.Minute), &bytes.Buffer{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(e.stops) != 1 {
		t.Fatalf("want the task stopped once so the sidecar goes too, got %d StopTask calls", len(e.stops))
	}
	stop := e.stops[0]
	if aws.ToString(stop.Task) != testTaskARN || aws.ToString(stop.Cluster) != testCluster || aws.ToString(stop.Reason) != releaseStopReason {
		t.Errorf("unexpected StopTask: task=%q cluster=%q reason=%q", aws.ToString(stop.Task), aws.ToString(stop.Cluster), aws.ToString(stop.Reason))
	}

	t.Run("non-zero exit in the sidecar case", func(t *testing.T) {
		e := &fakeECS{describeTasks: []*ecs.DescribeTasksOutput{taskState("RUNNING", "STOPPED", aws.Int32(2))}}
		err := runReleaseTaskTo(context.Background(), newTestClients(e, &fakeSSM{}, nil), testReleaseTask(time.Minute), &bytes.Buffer{})
		if got := exitCodeOf(t, err); got != ExitCommandFailed {
			t.Errorf("exit code = %d, want %d", got, ExitCommandFailed)
		}
		if len(e.stops) != 1 {
			t.Errorf("task must still be stopped after a failed command, got %d StopTask calls", len(e.stops))
		}
	})
}

// When the whole task stops first (no sidecars), the exit code is read from the
// stopped task and nothing needs stopping.
func TestRunReleaseTask_TaskStoppedFirst(t *testing.T) {
	fastPolling(t)
	e := &fakeECS{describeTasks: []*ecs.DescribeTasksOutput{taskState("STOPPED", "STOPPED", aws.Int32(1))}}

	err := runReleaseTaskTo(context.Background(), newTestClients(e, &fakeSSM{}, nil), testReleaseTask(time.Minute), &bytes.Buffer{})
	if got := exitCodeOf(t, err); got != ExitCommandFailed {
		t.Errorf("exit code = %d, want %d", got, ExitCommandFailed)
	}
	if len(e.stops) != 0 {
		t.Errorf("an already-stopped task must not be stopped again, got %d StopTask calls", len(e.stops))
	}
}

func TestRunReleaseTask_Timeout(t *testing.T) {
	fastPolling(t)
	e := &fakeECS{describeTasks: []*ecs.DescribeTasksOutput{taskState("RUNNING", "RUNNING", nil)}}

	err := runReleaseTaskTo(context.Background(), newTestClients(e, &fakeSSM{}, nil), testReleaseTask(10*time.Millisecond), &bytes.Buffer{})
	if got := exitCodeOf(t, err); got != ExitTimeout {
		t.Errorf("exit code = %d, want %d", got, ExitTimeout)
	}
	if len(e.stops) != 1 || !strings.Contains(aws.ToString(e.stops[0].Reason), "timed out") {
		t.Errorf("want the task stopped with a timeout reason, got %+v", e.stops)
	}
	if !strings.Contains(err.Error(), testTaskARN) {
		t.Errorf("error should name the task: %v", err)
	}
}

func TestRunReleaseTask_FailedToStart(t *testing.T) {
	fastPolling(t)

	t.Run("RunTask reports a failure", func(t *testing.T) {
		e := &fakeECS{runOut: &ecs.RunTaskOutput{Failures: []ecstypes.Failure{{
			Arn: aws.String("arn:aws:ecs:us-east-1:123456789012:container-instance/x"), Reason: aws.String("RESOURCE:MEMORY"),
		}}}}
		err := runReleaseTaskTo(context.Background(), newTestClients(e, &fakeSSM{}, nil), testReleaseTask(time.Minute), &bytes.Buffer{})
		if got := exitCodeOf(t, err); got != ExitFailedToStart {
			t.Errorf("exit code = %d, want %d", got, ExitFailedToStart)
		}
		if !strings.Contains(err.Error(), "RESOURCE:MEMORY") {
			t.Errorf("error should carry the failure reason: %v", err)
		}
		if len(e.stops) != 0 || e.describeTasksIdx != 0 {
			t.Errorf("nothing to poll or stop when no task started: stops=%d", len(e.stops))
		}
	})

	t.Run("RunTask errors", func(t *testing.T) {
		e := &fakeECS{runErr: errors.New("AccessDeniedException: iam:PassRole")}
		err := runReleaseTaskTo(context.Background(), newTestClients(e, &fakeSSM{}, nil), testReleaseTask(time.Minute), &bytes.Buffer{})
		if got := exitCodeOf(t, err); got != ExitFailedToStart {
			t.Errorf("exit code = %d, want %d", got, ExitFailedToStart)
		}
	})

	t.Run("task stops without the container ever reporting an exit code", func(t *testing.T) {
		stopped := taskState("STOPPED", "STOPPED", nil)
		stopped.Tasks[0].StopCode = ecstypes.TaskStopCodeTaskFailedToStart
		stopped.Tasks[0].StoppedReason = aws.String("CannotPullContainerError")
		e := &fakeECS{describeTasks: []*ecs.DescribeTasksOutput{stopped}}
		err := runReleaseTaskTo(context.Background(), newTestClients(e, &fakeSSM{}, nil), testReleaseTask(time.Minute), &bytes.Buffer{})
		if got := exitCodeOf(t, err); got != ExitFailedToStart {
			t.Errorf("exit code = %d, want %d", got, ExitFailedToStart)
		}
		if !strings.Contains(err.Error(), "CannotPullContainerError") {
			t.Errorf("error should carry the stopped reason: %v", err)
		}
	})
}

// The one-off task must land on the service's own infrastructure: same launch
// type or capacity providers, same subnets and security groups, same platform
// version — with the command applied only as an override.
func TestRunTaskInput_CopiesServiceLaunchConfig(t *testing.T) {
	t.Run("Fargate launch type", func(t *testing.T) {
		task := testReleaseTask(time.Minute)
		in := task.runTaskInput()

		if in.LaunchType != ecstypes.LaunchTypeFargate || len(in.CapacityProviderStrategy) != 0 {
			t.Errorf("launchType=%q strategy=%v, want FARGATE and no strategy", in.LaunchType, in.CapacityProviderStrategy)
		}
		if aws.ToString(in.PlatformVersion) != "1.4.0" {
			t.Errorf("platformVersion = %q, want 1.4.0", aws.ToString(in.PlatformVersion))
		}
		vpc := in.NetworkConfiguration.AwsvpcConfiguration
		if strings.Join(vpc.Subnets, ",") != "subnet-a,subnet-b" || strings.Join(vpc.SecurityGroups, ",") != "sg-1" || vpc.AssignPublicIp != ecstypes.AssignPublicIpDisabled {
			t.Errorf("network configuration not copied: %+v", vpc)
		}
		if aws.ToString(in.Cluster) != testCluster || aws.ToString(in.TaskDefinition) != testRegisterARN {
			t.Errorf("cluster=%q taskDefinition=%q", aws.ToString(in.Cluster), aws.ToString(in.TaskDefinition))
		}
		if aws.ToInt32(in.Count) != 1 || aws.ToString(in.StartedBy) != "ecs-deploy" || in.EnableExecuteCommand {
			t.Errorf("count=%d startedBy=%q enableExecuteCommand=%v", aws.ToInt32(in.Count), aws.ToString(in.StartedBy), in.EnableExecuteCommand)
		}
		if in.PropagateTags != "" {
			t.Errorf("RunTask rejects propagateTags=SERVICE; got %q", in.PropagateTags)
		}
		if len(in.Tags) != 1 || aws.ToString(in.Tags[0].Key) != "env" || aws.ToString(in.Tags[0].Value) != "prod" {
			t.Errorf("a tagged service's tags should be copied onto the task, got %+v", in.Tags)
		}
		if aws.ToString(in.ClientToken) == "" {
			t.Error("each RunTask request should carry its own client token")
		}
		ov := in.Overrides.ContainerOverrides
		if len(ov) != 1 || aws.ToString(ov[0].Name) != testContainer || strings.Join(ov[0].Command, " ") != "migrate up" {
			t.Errorf("unexpected container overrides: %+v", ov)
		}
	})

	t.Run("capacity provider strategy", func(t *testing.T) {
		svc := testFargateService()
		svc.LaunchType = ""
		svc.Tags = nil
		svc.CapacityProviderStrategy = []ecstypes.CapacityProviderStrategyItem{{CapacityProvider: aws.String("FARGATE_SPOT"), Weight: 1}}
		task := testReleaseTask(time.Minute)
		task.Launch = launchConfigFromService(svc)
		in := task.runTaskInput()

		if in.LaunchType != "" || len(in.CapacityProviderStrategy) != 1 || aws.ToString(in.CapacityProviderStrategy[0].CapacityProvider) != "FARGATE_SPOT" {
			t.Errorf("launchType=%q strategy=%+v, want only the strategy", in.LaunchType, in.CapacityProviderStrategy)
		}
		if in.PropagateTags != "" || len(in.Tags) != 0 {
			t.Errorf("an untagged service must not tag the task, got propagate=%q tags=%+v", in.PropagateTags, in.Tags)
		}
	})
}

func TestAwslogsStream(t *testing.T) {
	lc := &ecstypes.LogConfiguration{
		LogDriver: ecstypes.LogDriverAwslogs,
		Options:   map[string]string{"awslogs-group": "/ecs/api", "awslogs-stream-prefix": "ecs", "awslogs-region": "us-west-2"},
	}
	group, stream, region, ok := awslogsStream(lc, testContainer, testTaskARN)
	if !ok || group != "/ecs/api" || stream != "ecs/api/0123456789abcdef" || region != "us-west-2" {
		t.Errorf("got ok=%v group=%q stream=%q region=%q", ok, group, stream, region)
	}

	for _, other := range []*ecstypes.LogConfiguration{nil, {LogDriver: ecstypes.LogDriverAwsfirelens}, {LogDriver: ecstypes.LogDriverAwslogs}} {
		if _, _, _, ok := awslogsStream(other, testContainer, testTaskARN); ok {
			t.Errorf("%+v should not be tailable", other)
		}
	}
}

func TestTailLogs(t *testing.T) {
	fastPolling(t)
	event := func(msg string) cwltypes.OutputLogEvent {
		return cwltypes.OutputLogEvent{Timestamp: aws.Int64(1700000000000), Message: aws.String(msg)}
	}
	logs := &fakeLogs{pages: []*cloudwatchlogs.GetLogEventsOutput{
		{Events: []cwltypes.OutputLogEvent{event("starting migration")}, NextForwardToken: aws.String("f/1")},
		{Events: []cwltypes.OutputLogEvent{event("applied 3 migrations\n")}, NextForwardToken: aws.String("f/2")},
		{NextForwardToken: aws.String("f/2")}, // exhausted: token repeats
	}}

	var out bytes.Buffer
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		tailLogs(context.Background(), logs, "/ecs/api", "ecs/api/0123456789abcdef", stop, &out)
	}()
	time.Sleep(20 * time.Millisecond)
	close(stop)
	<-done

	got := out.String()
	for _, want := range []string{"starting migration", "applied 3 migrations"} {
		if strings.Count(got, want) != 1 {
			t.Errorf("want %q printed exactly once, got:\n%s", want, got)
		}
	}
	if logs.calls < 3 {
		t.Errorf("want the stream drained after stop, got %d GetLogEvents calls", logs.calls)
	}
}

// A container that does not log through awslogs gets a hint, not a tail.
func TestStartLogTail_NonAwslogsHint(t *testing.T) {
	var out bytes.Buffer
	logs := &fakeLogs{}
	stop := startLogTail(context.Background(), newTestClients(&fakeECS{}, &fakeSSM{}, logs), testReleaseTask(time.Minute), testTaskARN, &out)
	stop()
	if !strings.Contains(out.String(), "awsfirelens") || !strings.Contains(out.String(), `{ecs="api"}`) {
		t.Errorf("want a Loki hint naming the driver and service, got %q", out.String())
	}
	if logs.calls != 0 {
		t.Errorf("must not call GetLogEvents for a non-awslogs container, got %d calls", logs.calls)
	}
}

// Regression: v0.4.0 sent propagateTags=SERVICE (always rejected by RunTask)
// and then retried with the same input struct, which the SDK had already
// stamped with a client token — so ECS answered ConflictException and every
// release against a tagged service failed to start.
func TestRunReleaseTask_TaggedServiceStarts(t *testing.T) {
	e := &fakeECS{describeTasks: []*ecs.DescribeTasksOutput{taskState("STOPPED", "STOPPED", aws.Int32(0))}}
	err := runReleaseTaskTo(context.Background(), newTestClients(e, &fakeSSM{}, nil), testReleaseTask(time.Minute), &bytes.Buffer{})
	if err != nil {
		t.Fatalf("release against a tagged service should start: %v", err)
	}
	if len(e.runs) != 1 {
		t.Errorf("RunTask calls = %d, want 1", len(e.runs))
	}
}

// When the tagged RunTask is refused (e.g. no ecs:TagResource), the retry
// without tags must be a new request with a new client token.
func TestRunReleaseTask_TagRetryUsesNewClientToken(t *testing.T) {
	e := &fakeECS{
		runErrs:       []error{errors.New("AccessDeniedException: ecs:TagResource")},
		describeTasks: []*ecs.DescribeTasksOutput{taskState("STOPPED", "STOPPED", aws.Int32(0))},
	}
	err := runReleaseTaskTo(context.Background(), newTestClients(e, &fakeSSM{}, nil), testReleaseTask(time.Minute), &bytes.Buffer{})
	if err != nil {
		t.Fatalf("retry without tags should start the task: %v", err)
	}
	if len(e.runs) != 2 {
		t.Fatalf("RunTask calls = %d, want 2", len(e.runs))
	}
	if len(e.runs[1].Tags) != 0 {
		t.Errorf("retry should drop tags, got %+v", e.runs[1].Tags)
	}
	if aws.ToString(e.runs[0].ClientToken) == aws.ToString(e.runs[1].ClientToken) {
		t.Errorf("retry reused client token %q", aws.ToString(e.runs[0].ClientToken))
	}
}

func TestLaunchConfigFromService_DropsReservedTags(t *testing.T) {
	svc := testFargateService()
	svc.Tags = append(svc.Tags, ecstypes.Tag{Key: aws.String("aws:cloudformation:stack-name"), Value: aws.String("x")})
	lc := launchConfigFromService(svc)
	for _, tag := range lc.Tags {
		if strings.HasPrefix(aws.ToString(tag.Key), "aws:") {
			t.Errorf("reserved tag %q must not be sent to RunTask", aws.ToString(tag.Key))
		}
	}
}
