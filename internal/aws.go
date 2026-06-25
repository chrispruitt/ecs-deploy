package internal

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

// Clients holds AWS service clients sharing the same credential configuration.
type Clients struct {
	ECS *ecs.Client
	SSM *ssm.Client
}

// NewClients builds ECS and SSM clients from the default credential chain.
// If roleARN is non-empty, credentials are wrapped with STS AssumeRole.
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
		ECS: ecs.NewFromConfig(cfg),
		SSM: ssm.NewFromConfig(cfg),
	}, nil
}
