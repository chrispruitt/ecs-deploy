package internal

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

// ecsAPI is the subset of the ECS client this tool uses, so the deploy and
// release flows can be driven by a fake in tests.
type ecsAPI interface {
	DescribeServices(ctx context.Context, params *ecs.DescribeServicesInput, optFns ...func(*ecs.Options)) (*ecs.DescribeServicesOutput, error)
	DescribeTaskDefinition(ctx context.Context, params *ecs.DescribeTaskDefinitionInput, optFns ...func(*ecs.Options)) (*ecs.DescribeTaskDefinitionOutput, error)
	RegisterTaskDefinition(ctx context.Context, params *ecs.RegisterTaskDefinitionInput, optFns ...func(*ecs.Options)) (*ecs.RegisterTaskDefinitionOutput, error)
	UpdateService(ctx context.Context, params *ecs.UpdateServiceInput, optFns ...func(*ecs.Options)) (*ecs.UpdateServiceOutput, error)
	RunTask(ctx context.Context, params *ecs.RunTaskInput, optFns ...func(*ecs.Options)) (*ecs.RunTaskOutput, error)
	DescribeTasks(ctx context.Context, params *ecs.DescribeTasksInput, optFns ...func(*ecs.Options)) (*ecs.DescribeTasksOutput, error)
	StopTask(ctx context.Context, params *ecs.StopTaskInput, optFns ...func(*ecs.Options)) (*ecs.StopTaskOutput, error)
}

// ssmAPI is the subset of the SSM client this tool uses.
type ssmAPI interface {
	versionParameterAPI
	GetParametersByPath(ctx context.Context, params *ssm.GetParametersByPathInput, optFns ...func(*ssm.Options)) (*ssm.GetParametersByPathOutput, error)
}

// logsAPI is the subset of the CloudWatch Logs client used to tail a release
// task's output.
type logsAPI interface {
	GetLogEvents(ctx context.Context, params *cloudwatchlogs.GetLogEventsInput, optFns ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.GetLogEventsOutput, error)
}

// Clients holds AWS service clients sharing the same credential configuration.
type Clients struct {
	ECS  ecsAPI
	SSM  ssmAPI
	Logs logsAPI

	// logsFor builds a CloudWatch Logs client for a specific region, used when a
	// container's awslogs configuration points at a log group in a region other
	// than the one the clients were built for. Nil in tests, where Logs is used
	// as-is.
	logsFor func(region string) logsAPI
}

// NewClients builds ECS, SSM and CloudWatch Logs clients from the default
// credential chain. If roleARN is non-empty, credentials are wrapped with STS
// AssumeRole.
func NewClients(ctx context.Context, roleARN string) (*Clients, error) {
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, err
	}

	if roleARN != "" {
		stsClient := sts.NewFromConfig(cfg)
		provider := stscreds.NewAssumeRoleProvider(stsClient, roleARN)
		cfg.Credentials = aws.NewCredentialsCache(provider)
	}

	return &Clients{
		ECS:  ecs.NewFromConfig(cfg),
		SSM:  ssm.NewFromConfig(cfg),
		Logs: cloudwatchlogs.NewFromConfig(cfg),
		logsFor: func(region string) logsAPI {
			return cloudwatchlogs.NewFromConfig(cfg, func(o *cloudwatchlogs.Options) {
				o.Region = region
			})
		},
	}, nil
}

// logsClient returns the Logs client to use for a log group in the given
// region. An empty region, or no per-region factory, means the default client.
func (c *Clients) logsClient(region string) logsAPI {
	if region == "" || c.logsFor == nil {
		return c.Logs
	}
	return c.logsFor(region)
}
