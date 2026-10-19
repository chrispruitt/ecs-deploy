package cmd

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/chrispruitt/ecs-deploy/internal"
)

type runConfig struct {
	Cluster     string
	Service     string
	Container   string
	ImageTag    string
	Command     string
	Role        string
	SSMPrefixes []string
	Timeout     time.Duration
	AutoApprove bool
	NoRegister  bool
}

var runCfg runConfig

var runCmd = &cobra.Command{
	Use:   "run",
	Short: "Run a one-off command as a task on a service's infrastructure, without deploying",
	Long: `Run a command as a one-off ECS task using the service's launch type, network
and task role, then exit with the container's exit code.

By default a new task definition revision is registered from the service's
current one with --image-tag applied, so the command runs at the version about
to be deployed. With --no-register the service's current revision is used
as-is. The service itself is never updated and no version parameter is written.

Exit codes: 0 success, 1 the command exited non-zero, 2 the task failed to
start, 3 the command timed out (the task is stopped).`,
	RunE: runOneOff,
}

func init() {
	f := runCmd.Flags()
	f.StringVar(&runCfg.Cluster, "cluster", "", "ECS cluster name")
	f.StringVar(&runCfg.Service, "service", "", "ECS service whose task definition, network and roles the task uses")
	f.StringVar(&runCfg.Container, "container", "", "Name of the container to run the command in")
	f.StringVar(&runCfg.ImageTag, "image-tag", "", "Image tag to register a new revision with before running (required unless --no-register)")
	f.StringVar(&runCfg.Command, "command", "", "Command to run, split shell-style into the container command override")
	f.StringVar(&runCfg.Role, "role", "", "IAM role ARN to assume")
	f.StringArrayVar(&runCfg.SSMPrefixes, "secret-ssm-prefix", nil, "SSM path prefix to sync as container secrets on the new revision (repeatable)")
	f.DurationVar(&runCfg.Timeout, "timeout", 30*time.Minute, "How long to wait for the command before stopping the task (e.g. 45m, 4h)")
	f.BoolVar(&runCfg.AutoApprove, "auto-approve", false, "Skip the approval prompt")
	f.BoolVar(&runCfg.NoRegister, "no-register", false, "Run from the service's current task definition revision instead of registering a new one")

	_ = runCmd.MarkFlagRequired("cluster")
	_ = runCmd.MarkFlagRequired("service")
	_ = runCmd.MarkFlagRequired("container")
	_ = runCmd.MarkFlagRequired("command")

	rootCmd.AddCommand(runCmd)
}

func runOneOff(cmd *cobra.Command, _ []string) error {
	if runCfg.NoRegister && (runCfg.ImageTag != "" || len(runCfg.SSMPrefixes) > 0) {
		return fmt.Errorf("--no-register runs the current revision as-is and cannot be combined with --image-tag or --secret-ssm-prefix")
	}
	if !runCfg.NoRegister && runCfg.ImageTag == "" && len(runCfg.SSMPrefixes) == 0 {
		return fmt.Errorf("--image-tag is required (or pass --no-register to run the service's current revision)")
	}

	command, err := internal.SplitCommand(runCfg.Command)
	if err != nil {
		return fmt.Errorf("invalid --command: %w", err)
	}

	ctx := cmd.Context()

	clients, err := internal.NewClients(ctx, runCfg.Role)
	if err != nil {
		return fmt.Errorf("failed to create AWS clients: %w", err)
	}

	revisionARN, err := internal.RunOneOff(ctx, clients, internal.RunConfig{
		Cluster:     runCfg.Cluster,
		Service:     runCfg.Service,
		Container:   runCfg.Container,
		ImageTag:    runCfg.ImageTag,
		SSMPrefixes: runCfg.SSMPrefixes,
		Command:     command,
		Timeout:     runCfg.Timeout,
		AutoApprove: runCfg.AutoApprove,
		NoRegister:  runCfg.NoRegister,
	})
	if err != nil {
		return err
	}

	fmt.Printf("Command succeeded (task definition %s).\n", revisionARN)
	return nil
}

// ExitCode maps an error returned by Execute to a process exit code.
func ExitCode(err error) int {
	return internal.ExitCode(err)
}
