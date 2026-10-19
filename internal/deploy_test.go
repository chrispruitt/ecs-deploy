package internal

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	ecstypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
)

type deployFixture struct {
	log  *callLog
	ecs  *fakeECS
	ssm  *fakeSSM
	logs *fakeLogs
}

// newDeployFixture is a tagged Fargate service on revision 41 whose version
// parameter currently reads v1.0.0, with the release container exiting as
// given.
func newDeployFixture(releaseExit *int32) *deployFixture {
	log := &callLog{}
	f := &deployFixture{
		log: log,
		ecs: &fakeECS{
			log:         log,
			service:     testFargateService(),
			taskDef:     testTaskDef(),
			taskDefTags: []ecstypes.Tag{{Key: aws.String("team"), Value: aws.String("api")}},
			registerARN: testRegisterARN,
		},
		ssm:  &fakeSSM{log: log, value: aws.String("v1.0.0")},
		logs: &fakeLogs{},
	}
	if releaseExit != nil {
		f.ecs.describeTasks = []*ecs.DescribeTasksOutput{
			taskState("RUNNING", "RUNNING", nil),
			taskState("RUNNING", "STOPPED", releaseExit),
		}
	}
	return f
}

func (f *deployFixture) clients() *Clients { return newTestClients(f.ecs, f.ssm, f.logs) }

func (f *deployFixture) assertCalls(t *testing.T, want ...string) {
	t.Helper()
	if !reflect.DeepEqual(f.log.names, want) {
		t.Errorf("call sequence\n got: %s\nwant: %s", strings.Join(f.log.names, " "), strings.Join(want, " "))
	}
}

func baseDeployConfig() DeploymentConfig {
	return DeploymentConfig{
		Cluster:          testCluster,
		Service:          testService,
		Container:        testContainer,
		ImageTag:         "v1.1.0",
		VersionParameter: "/prod/api/VERSION",
		AutoApprove:      true,
		ReleaseTimeout:   time.Minute,
	}
}

// completedRollout makes the next DescribeServices poll report the new
// revision's deployment as COMPLETED.
func completedRollout(f *deployFixture) {
	f.ecs.service.Deployments = []ecstypes.Deployment{{
		TaskDefinition: aws.String(testRegisterARN),
		RolloutState:   ecstypes.DeploymentRolloutStateCompleted,
	}}
}

// Without --release-command a deploy must make exactly the v0.3.0 sequence of
// AWS calls, in the same order, with no task API involved — the binary is
// pinned by CI and v0.4.0 has to be a drop-in.
func TestRunDeployment_WithoutReleaseCommandIsUnchanged(t *testing.T) {
	fastPolling(t)

	t.Run("full deploy with rollout wait", func(t *testing.T) {
		f := newDeployFixture(nil)
		completedRollout(f)
		newARN, err := RunDeployment(context.Background(), f.clients(), baseDeployConfig())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if newARN != testRegisterARN {
			t.Errorf("returned ARN = %q, want %q", newARN, testRegisterARN)
		}
		f.assertCalls(t,
			"DescribeServices", "DescribeTaskDefinition", "GetParameter",
			"PutParameter", "RegisterTaskDefinition", "UpdateService",
			"DescribeServices",
		)
		if len(f.ecs.runs)+len(f.ecs.stops) != 0 {
			t.Errorf("no task should be run or stopped: runs=%d stops=%d", len(f.ecs.runs), len(f.ecs.stops))
		}
		for _, in := range f.ecs.describeServices {
			if len(in.Include) != 0 {
				t.Errorf("DescribeServices must not request extra fields on a plain deploy, got include=%v", in.Include)
			}
		}
		if aws.ToString(f.ssm.puts[0].Value) != "v1.1.0" {
			t.Errorf("version parameter written as %q, want v1.1.0", aws.ToString(f.ssm.puts[0].Value))
		}
		if aws.ToString(f.ecs.updated[0].TaskDefinition) != testRegisterARN {
			t.Errorf("service updated to %q, want %q", aws.ToString(f.ecs.updated[0].TaskDefinition), testRegisterARN)
		}
	})

	t.Run("--no-wait stops after UpdateService", func(t *testing.T) {
		f := newDeployFixture(nil)
		cfg := baseDeployConfig()
		cfg.NoWait = true
		if _, err := RunDeployment(context.Background(), f.clients(), cfg); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		f.assertCalls(t, "DescribeServices", "DescribeTaskDefinition", "GetParameter", "PutParameter", "RegisterTaskDefinition", "UpdateService")
	})

	t.Run("register failure still rolls the version parameter back", func(t *testing.T) {
		f := newDeployFixture(nil)
		f.ecs.registerErr = errors.New("ClientException")
		_, err := RunDeployment(context.Background(), f.clients(), baseDeployConfig())
		if err == nil {
			t.Fatal("want the register error")
		}
		f.assertCalls(t, "DescribeServices", "DescribeTaskDefinition", "GetParameter", "PutParameter", "RegisterTaskDefinition", "PutParameter")
		if aws.ToString(f.ssm.puts[1].Value) != "v1.0.0" {
			t.Errorf("rollback wrote %q, want the previous v1.0.0", aws.ToString(f.ssm.puts[1].Value))
		}
	})

	t.Run("same tag writes nothing", func(t *testing.T) {
		f := newDeployFixture(nil)
		cfg := baseDeployConfig()
		cfg.ImageTag = "v1.0.0"
		cfg.NoWait = true
		if _, err := RunDeployment(context.Background(), f.clients(), cfg); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		f.assertCalls(t, "DescribeServices", "DescribeTaskDefinition", "GetParameter", "RegisterTaskDefinition", "UpdateService")
	})
}

// A failed release command must leave the deploy exactly where it started:
// no version parameter written, no UpdateService. The only trace is the
// registered (unused) revision.
func TestRunDeployment_ReleaseFailureAbortsBeforeAnyWrite(t *testing.T) {
	fastPolling(t)
	f := newDeployFixture(aws.Int32(1))
	cfg := baseDeployConfig()
	cfg.ReleaseCommand = []string{"migrate", "up"}

	_, err := RunDeployment(context.Background(), f.clients(), cfg)
	if err == nil {
		t.Fatal("want the release failure")
	}
	if got := ExitCode(err); got != ExitCommandFailed {
		t.Errorf("exit code = %d, want %d", got, ExitCommandFailed)
	}
	if !strings.Contains(err.Error(), "service not updated") {
		t.Errorf("error should say the service was not touched: %v", err)
	}
	f.assertCalls(t,
		"DescribeServices", "DescribeTaskDefinition", "GetParameter",
		"RegisterTaskDefinition", "RunTask", "DescribeTasks", "DescribeTasks", "StopTask",
	)
	if len(f.ssm.puts) != 0 || len(f.ssm.deletes) != 0 {
		t.Errorf("version parameter must not be written when the release fails: puts=%d deletes=%d", len(f.ssm.puts), len(f.ssm.deletes))
	}
	if len(f.ecs.updated) != 0 {
		t.Errorf("service must not be updated when the release fails")
	}
}

// With --release-command the order is: register, run the release task, THEN
// the deploy as before (version parameter, UpdateService, rollout).
func TestRunDeployment_ReleaseSuccessThenDeploy(t *testing.T) {
	fastPolling(t)
	f := newDeployFixture(aws.Int32(0))
	completedRollout(f)
	cfg := baseDeployConfig()
	cfg.ReleaseCommand = []string{"migrate", "up"}

	newARN, err := RunDeployment(context.Background(), f.clients(), cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if newARN != testRegisterARN {
		t.Errorf("returned ARN = %q, want %q", newARN, testRegisterARN)
	}
	f.assertCalls(t,
		"DescribeServices", "DescribeTaskDefinition", "GetParameter",
		"RegisterTaskDefinition", "RunTask", "DescribeTasks", "DescribeTasks", "StopTask",
		"PutParameter", "UpdateService", "DescribeServices",
	)
	if aws.ToString(f.ecs.runs[0].TaskDefinition) != testRegisterARN {
		t.Errorf("release task ran from %q, want the new revision %q", aws.ToString(f.ecs.runs[0].TaskDefinition), testRegisterARN)
	}
	if len(f.ecs.describeServices[0].Include) == 0 {
		t.Errorf("a release deploy needs the service's tags to decide propagateTags")
	}

	t.Run("--no-wait still waits for the release task", func(t *testing.T) {
		f := newDeployFixture(aws.Int32(0))
		cfg := baseDeployConfig()
		cfg.ReleaseCommand = []string{"migrate", "up"}
		cfg.NoWait = true
		if _, err := RunDeployment(context.Background(), f.clients(), cfg); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		f.assertCalls(t,
			"DescribeServices", "DescribeTaskDefinition", "GetParameter",
			"RegisterTaskDefinition", "RunTask", "DescribeTasks", "DescribeTasks", "StopTask",
			"PutParameter", "UpdateService",
		)
	})
}

// Terraform owns the task definition. The release command must appear only as
// a RunTask container override — the registered revision with --release-command
// must be byte-identical to the one without it.
func TestRunDeployment_ReleaseCommandNeverTouchesTheRevision(t *testing.T) {
	fastPolling(t)

	plain := newDeployFixture(nil)
	plainCfg := baseDeployConfig()
	plainCfg.NoWait = true
	if _, err := RunDeployment(context.Background(), plain.clients(), plainCfg); err != nil {
		t.Fatalf("plain deploy: %v", err)
	}

	release := newDeployFixture(aws.Int32(0))
	releaseCfg := plainCfg
	releaseCfg.ReleaseCommand = []string{"migrate", "up"}
	if _, err := RunDeployment(context.Background(), release.clients(), releaseCfg); err != nil {
		t.Fatalf("release deploy: %v", err)
	}

	plainJSON, _ := json.Marshal(plain.ecs.registered[0])
	releaseJSON, _ := json.Marshal(release.ecs.registered[0])
	if string(plainJSON) != string(releaseJSON) {
		t.Errorf("RegisterTaskDefinition input differs with --release-command:\n plain:   %s\n release: %s", plainJSON, releaseJSON)
	}

	// And the revision differs from the current one only by the image tag.
	original := testTaskDef()
	registered := release.ecs.registered[0]
	for i, c := range registered.ContainerDefinitions {
		o := original.ContainerDefinitions[i]
		if i == 0 && aws.ToString(c.Image) != "123456789012.dkr.ecr.us-east-1.amazonaws.com/api:v1.1.0" {
			t.Errorf("image = %q, want the new tag", aws.ToString(c.Image))
		}
		c.Image = o.Image
		if !reflect.DeepEqual(c, o) {
			t.Errorf("container %q changed beyond the image:\n got: %+v\nwant: %+v", aws.ToString(c.Name), c, o)
		}
	}
	if aws.ToString(registered.Cpu) != "512" || aws.ToString(registered.Memory) != "1024" || len(registered.Tags) != 1 {
		t.Errorf("task-level fields changed: cpu=%q memory=%q tags=%d", aws.ToString(registered.Cpu), aws.ToString(registered.Memory), len(registered.Tags))
	}

	// The command lives only in the RunTask overrides.
	ov := release.ecs.runs[0].Overrides.ContainerOverrides
	if len(ov) != 1 || aws.ToString(ov[0].Name) != testContainer || !reflect.DeepEqual(ov[0].Command, []string{"migrate", "up"}) {
		t.Errorf("RunTask overrides = %+v, want the command on %q only", ov, testContainer)
	}
}

// run registers and runs but never deploys: no UpdateService, no version
// parameter. With --no-register it does not even register.
func TestRunOneOff(t *testing.T) {
	fastPolling(t)

	t.Run("registers a revision and runs from it", func(t *testing.T) {
		f := newDeployFixture(aws.Int32(0))
		arn, err := RunOneOff(context.Background(), f.clients(), RunConfig{
			Cluster: testCluster, Service: testService, Container: testContainer,
			ImageTag: "v1.1.0", Command: []string{"migrate", "status"}, Timeout: time.Minute, AutoApprove: true,
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if arn != testRegisterARN {
			t.Errorf("ran from %q, want the new revision %q", arn, testRegisterARN)
		}
		f.assertCalls(t, "DescribeServices", "DescribeTaskDefinition", "RegisterTaskDefinition", "RunTask", "DescribeTasks", "DescribeTasks", "StopTask")
		if len(f.ecs.updated) != 0 || len(f.ssm.puts) != 0 || len(f.ssm.deletes) != 0 {
			t.Errorf("run must never update the service or write a parameter: updates=%d puts=%d deletes=%d", len(f.ecs.updated), len(f.ssm.puts), len(f.ssm.deletes))
		}
		reg := f.ecs.registered[0].ContainerDefinitions[0]
		if !reflect.DeepEqual(reg.Command, []string{"node", "server.js"}) {
			t.Errorf("registered revision's command changed to %q", reg.Command)
		}
		if got := f.ecs.runs[0].Overrides.ContainerOverrides[0].Command; !reflect.DeepEqual(got, []string{"migrate", "status"}) {
			t.Errorf("RunTask override command = %q", got)
		}
	})

	t.Run("--no-register runs the current revision", func(t *testing.T) {
		f := newDeployFixture(aws.Int32(0))
		arn, err := RunOneOff(context.Background(), f.clients(), RunConfig{
			Cluster: testCluster, Service: testService, Container: testContainer,
			Command: []string{"migrate", "status"}, Timeout: time.Minute, AutoApprove: true, NoRegister: true,
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if arn != testTaskDefARN {
			t.Errorf("ran from %q, want the current revision %q", arn, testTaskDefARN)
		}
		f.assertCalls(t, "DescribeServices", "DescribeTaskDefinition", "RunTask", "DescribeTasks", "DescribeTasks", "StopTask")
		if aws.ToString(f.ecs.runs[0].TaskDefinition) != testTaskDefARN {
			t.Errorf("RunTask used %q, want %q", aws.ToString(f.ecs.runs[0].TaskDefinition), testTaskDefARN)
		}
	})

	t.Run("a failed command surfaces its exit code", func(t *testing.T) {
		f := newDeployFixture(aws.Int32(4))
		_, err := RunOneOff(context.Background(), f.clients(), RunConfig{
			Cluster: testCluster, Service: testService, Container: testContainer,
			Command: []string{"false"}, Timeout: time.Minute, AutoApprove: true, NoRegister: true,
		})
		if got := ExitCode(err); got != ExitCommandFailed {
			t.Errorf("exit code = %d, want %d (%v)", got, ExitCommandFailed, err)
		}
		if len(f.ecs.updated) != 0 {
			t.Errorf("run must never update the service")
		}
	})
}
