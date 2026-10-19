package internal

import (
	"context"
	"errors"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	ecstypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
)

// callLog records the order of AWS calls across every fake sharing it, so a
// test can assert the exact sequence a flow makes. A nil log is a no-op.
type callLog struct{ names []string }

func (l *callLog) add(name string) {
	if l != nil {
		l.names = append(l.names, name)
	}
}

// fakeECS serves one service and one task definition and records every write.
// describeTasks is served in order, the last entry repeating once exhausted.
type fakeECS struct {
	log *callLog

	service     ecstypes.Service
	taskDef     *ecstypes.TaskDefinition
	taskDefTags []ecstypes.Tag
	registerARN string
	registerErr error
	updateErr   error

	runOut *ecs.RunTaskOutput
	runErr error

	describeTasks    []*ecs.DescribeTasksOutput
	describeTasksIdx int

	describeServices []*ecs.DescribeServicesInput
	registered       []*ecs.RegisterTaskDefinitionInput
	updated          []*ecs.UpdateServiceInput
	runs             []*ecs.RunTaskInput
	stops            []*ecs.StopTaskInput
}

func (f *fakeECS) DescribeServices(_ context.Context, in *ecs.DescribeServicesInput, _ ...func(*ecs.Options)) (*ecs.DescribeServicesOutput, error) {
	f.log.add("DescribeServices")
	f.describeServices = append(f.describeServices, in)
	return &ecs.DescribeServicesOutput{Services: []ecstypes.Service{f.service}}, nil
}

func (f *fakeECS) DescribeTaskDefinition(_ context.Context, in *ecs.DescribeTaskDefinitionInput, _ ...func(*ecs.Options)) (*ecs.DescribeTaskDefinitionOutput, error) {
	f.log.add("DescribeTaskDefinition")
	if f.taskDef == nil || aws.ToString(in.TaskDefinition) != aws.ToString(f.taskDef.TaskDefinitionArn) {
		return nil, fmt.Errorf("unexpected task definition %q", aws.ToString(in.TaskDefinition))
	}
	return &ecs.DescribeTaskDefinitionOutput{TaskDefinition: f.taskDef, Tags: f.taskDefTags}, nil
}

func (f *fakeECS) RegisterTaskDefinition(_ context.Context, in *ecs.RegisterTaskDefinitionInput, _ ...func(*ecs.Options)) (*ecs.RegisterTaskDefinitionOutput, error) {
	f.log.add("RegisterTaskDefinition")
	f.registered = append(f.registered, in)
	if f.registerErr != nil {
		return nil, f.registerErr
	}
	return &ecs.RegisterTaskDefinitionOutput{TaskDefinition: &ecstypes.TaskDefinition{TaskDefinitionArn: aws.String(f.registerARN)}}, nil
}

func (f *fakeECS) UpdateService(_ context.Context, in *ecs.UpdateServiceInput, _ ...func(*ecs.Options)) (*ecs.UpdateServiceOutput, error) {
	f.log.add("UpdateService")
	f.updated = append(f.updated, in)
	if f.updateErr != nil {
		return nil, f.updateErr
	}
	return &ecs.UpdateServiceOutput{}, nil
}

func (f *fakeECS) RunTask(_ context.Context, in *ecs.RunTaskInput, _ ...func(*ecs.Options)) (*ecs.RunTaskOutput, error) {
	f.log.add("RunTask")
	f.runs = append(f.runs, in)
	if f.runErr != nil {
		return nil, f.runErr
	}
	if f.runOut != nil {
		return f.runOut, nil
	}
	return &ecs.RunTaskOutput{Tasks: []ecstypes.Task{{TaskArn: aws.String(testTaskARN)}}}, nil
}

func (f *fakeECS) DescribeTasks(_ context.Context, _ *ecs.DescribeTasksInput, _ ...func(*ecs.Options)) (*ecs.DescribeTasksOutput, error) {
	f.log.add("DescribeTasks")
	if len(f.describeTasks) == 0 {
		return nil, errors.New("DescribeTasks not expected")
	}
	out := f.describeTasks[f.describeTasksIdx]
	if f.describeTasksIdx < len(f.describeTasks)-1 {
		f.describeTasksIdx++
	}
	return out, nil
}

func (f *fakeECS) StopTask(_ context.Context, in *ecs.StopTaskInput, _ ...func(*ecs.Options)) (*ecs.StopTaskOutput, error) {
	f.log.add("StopTask")
	f.stops = append(f.stops, in)
	return &ecs.StopTaskOutput{}, nil
}

// fakeLogs serves GetLogEvents pages in order, the last repeating.
type fakeLogs struct {
	pages []*cloudwatchlogs.GetLogEventsOutput
	idx   int
	calls int
}

func (f *fakeLogs) GetLogEvents(_ context.Context, _ *cloudwatchlogs.GetLogEventsInput, _ ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.GetLogEventsOutput, error) {
	f.calls++
	if len(f.pages) == 0 {
		return &cloudwatchlogs.GetLogEventsOutput{}, nil
	}
	out := f.pages[f.idx]
	if f.idx < len(f.pages)-1 {
		f.idx++
	}
	return out, nil
}

const (
	testCluster     = "prod"
	testService     = "api"
	testContainer   = "api"
	testTaskDefARN  = "arn:aws:ecs:us-east-1:123456789012:task-definition/api:41"
	testRegisterARN = "arn:aws:ecs:us-east-1:123456789012:task-definition/api:42"
	testTaskARN     = "arn:aws:ecs:us-east-1:123456789012:task/prod/0123456789abcdef"
)

// testTaskDef is a two-container task definition: the application plus an
// essential log-router sidecar, the shape that keeps a task alive after the
// application container exits.
func testTaskDef() *ecstypes.TaskDefinition {
	return &ecstypes.TaskDefinition{
		TaskDefinitionArn: aws.String(testTaskDefARN),
		Family:            aws.String("api"),
		Revision:          41,
		Cpu:               aws.String("512"),
		Memory:            aws.String("1024"),
		ContainerDefinitions: []ecstypes.ContainerDefinition{
			{
				Name:       aws.String(testContainer),
				Image:      aws.String("123456789012.dkr.ecr.us-east-1.amazonaws.com/api:v1.0.0"),
				Command:    []string{"node", "server.js"},
				EntryPoint: []string{"/docker-entrypoint.sh"},
				Essential:  aws.Bool(true),
				Environment: []ecstypes.KeyValuePair{
					{Name: aws.String("NODE_ENV"), Value: aws.String("production")},
				},
				LogConfiguration: &ecstypes.LogConfiguration{
					LogDriver: ecstypes.LogDriverAwsfirelens,
				},
			},
			{
				Name:      aws.String("log-router"),
				Image:     aws.String("public.ecr.aws/aws-observability/aws-for-fluent-bit:stable"),
				Essential: aws.Bool(true),
			},
		},
	}
}

func testFargateService() ecstypes.Service {
	return ecstypes.Service{
		ServiceName:     aws.String(testService),
		TaskDefinition:  aws.String(testTaskDefARN),
		LaunchType:      ecstypes.LaunchTypeFargate,
		PlatformVersion: aws.String("1.4.0"),
		NetworkConfiguration: &ecstypes.NetworkConfiguration{
			AwsvpcConfiguration: &ecstypes.AwsVpcConfiguration{
				Subnets:        []string{"subnet-a", "subnet-b"},
				SecurityGroups: []string{"sg-1"},
				AssignPublicIp: ecstypes.AssignPublicIpDisabled,
			},
		},
		Tags: []ecstypes.Tag{{Key: aws.String("env"), Value: aws.String("prod")}},
	}
}

// taskState builds a DescribeTasks response for the one task under test.
func taskState(taskStatus, containerStatus string, exitCode *int32) *ecs.DescribeTasksOutput {
	return &ecs.DescribeTasksOutput{Tasks: []ecstypes.Task{{
		TaskArn:       aws.String(testTaskARN),
		LastStatus:    aws.String(taskStatus),
		StopCode:      ecstypes.TaskStopCodeEssentialContainerExited,
		StoppedReason: aws.String("Essential container in task exited"),
		Containers: []ecstypes.Container{
			{Name: aws.String(testContainer), LastStatus: aws.String(containerStatus), ExitCode: exitCode},
			{Name: aws.String("log-router"), LastStatus: aws.String("RUNNING")},
		},
	}}}
}

func newTestClients(ecsFake *fakeECS, ssmFake *fakeSSM, logs *fakeLogs) *Clients {
	if logs == nil {
		logs = &fakeLogs{}
	}
	return &Clients{ECS: ecsFake, SSM: ssmFake, Logs: logs}
}
