package cmd

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/chrispruitt/ecs-deploy/internal"
)

type config struct {
	Cluster      string
	Service      string
	Container    string
	ImageTag     string
	Role         string
	SSMPrefixes  []string
	VersionParam string
	AutoApprove  bool
	NoWait       bool
}

var cfg config

// version is set at build time via -ldflags="-X ecs-deploy/cmd.version=vX.Y.Z".
var version = "dev"

var rootCmd = &cobra.Command{
	Use:     "ecs-deploy",
	Short:   "Deploy a new container image tag to an ECS service",
	Version: version,
	RunE:    runDeploy,
}

// RootContext returns a base context for signal propagation from main.
func RootContext() context.Context {
	return context.Background()
}

// Execute runs the root command with the given context.
func Execute(ctx context.Context) error {
	rootCmd.SilenceErrors = true
	rootCmd.SilenceUsage = true
	return rootCmd.ExecuteContext(ctx)
}

func init() {
	rootCmd.Flags().StringVar(&cfg.Cluster, "cluster", "", "ECS cluster name")
	rootCmd.Flags().StringVar(&cfg.Service, "service", "", "ECS service name")
	rootCmd.Flags().StringVar(&cfg.Container, "container", "", "Name of the container to update")
	rootCmd.Flags().StringVar(&cfg.ImageTag, "image-tag", "", "New container image tag (required unless --secret-ssm-prefix is set)")
	rootCmd.Flags().StringVar(&cfg.Role, "role", "", "IAM role ARN to assume")
	rootCmd.Flags().StringArrayVar(&cfg.SSMPrefixes, "secret-ssm-prefix", nil, "SSM path prefix to sync as container secrets (repeatable)")
	rootCmd.Flags().StringVar(&cfg.VersionParam, "version-parameter", "", "SSM parameter path to write --image-tag to before deploying (e.g. /prod/my-service/VERSION)")
	rootCmd.Flags().BoolVar(&cfg.AutoApprove, "auto-approve", false, "Skip the secrets diff approval prompt")
	rootCmd.Flags().BoolVar(&cfg.NoWait, "no-wait", false, "Exit after registering the task definition without waiting for deployment")

	_ = rootCmd.MarkFlagRequired("cluster")
	_ = rootCmd.MarkFlagRequired("service")
	_ = rootCmd.MarkFlagRequired("container")
}

func runDeploy(cmd *cobra.Command, _ []string) error {
	if cfg.ImageTag == "" && len(cfg.SSMPrefixes) == 0 {
		return fmt.Errorf("at least one of --image-tag or --secret-ssm-prefix must be provided")
	}

	// There is nothing to record without a tag, and silently writing an empty
	// value would be worse than refusing.
	if cfg.VersionParam != "" && cfg.ImageTag == "" {
		return fmt.Errorf("--version-parameter requires --image-tag: there is no tag to record")
	}

	ctx := cmd.Context()

	clients, err := internal.NewClients(ctx, cfg.Role)
	if err != nil {
		return fmt.Errorf("failed to create AWS clients: %w", err)
	}

	newARN, err := internal.RunDeployment(ctx, clients, internal.DeploymentConfig{
		Cluster:          cfg.Cluster,
		Service:          cfg.Service,
		Container:        cfg.Container,
		ImageTag:         cfg.ImageTag,
		SSMPrefixes:      cfg.SSMPrefixes,
		VersionParameter: cfg.VersionParam,
		AutoApprove:      cfg.AutoApprove,
		NoWait:           cfg.NoWait,
	})
	if err != nil {
		return err
	}

	if newARN == "" {
		// No changes detected; message already printed by RunDeployment.
		return nil
	}

	if cfg.NoWait {
		fmt.Printf("Registered new task definition: %s\n", newARN)
		fmt.Println("Service updated. Exiting without waiting for deployment to complete.")
	} else {
		fmt.Println("Deployment succeeded.")
	}

	return nil
}
