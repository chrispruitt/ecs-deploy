package internal

import (
	"context"
	"fmt"
	"os"
	"time"
)

// RunConfig holds the resolved flags for the standalone `run` subcommand.
type RunConfig struct {
	Cluster     string
	Service     string
	Container   string
	ImageTag    string   // empty means keep the current image
	SSMPrefixes []string // optional secrets sync for the new revision
	Command     []string
	Timeout     time.Duration
	AutoApprove bool
	NoRegister  bool // run from the service's current revision as-is
}

// RunOneOff runs a command as a one-off task on the service's infrastructure
// and returns the task definition revision it ran from. Unless NoRegister is
// set, a new revision is registered first with the image tag (and secrets)
// applied, exactly as a deploy would — but the service is never updated and
// no version parameter is written. The command itself is only ever a RunTask
// override; it is never written into the revision.
func RunOneOff(ctx context.Context, clients *Clients, cfg RunConfig) (string, error) {
	plan, err := planRevision(ctx, clients, cfg.Cluster, cfg.Service, cfg.Container, cfg.ImageTag, cfg.SSMPrefixes, true)
	if err != nil {
		return "", err
	}

	register := !cfg.NoRegister && plan.hasChanges()

	fmt.Printf("\nRun plan for container %q:\n", cfg.Container)
	fmt.Printf("  command: %s\n", quoteCommand(cfg.Command))
	fmt.Printf("  timeout: %s\n", cfg.Timeout)
	switch {
	case cfg.NoRegister:
		fmt.Printf("  revision: %s (current)\n", plan.taskDefARN)
	case !register:
		fmt.Printf("  revision: %s (current — nothing to change)\n", plan.taskDefARN)
	default:
		fmt.Printf("  revision: new, registered from %s\n", plan.taskDefARN)
		if plan.newImage != "" {
			fmt.Printf("  image:  %s\n          -> %s\n", plan.oldImage, plan.newImage)
		}
		printSecretChanges(plan.secretChanges, len(cfg.SSMPrefixes) > 0)
	}
	fmt.Println("  the service is not updated and no version parameter is written")
	fmt.Println()

	if !cfg.AutoApprove {
		ok, err := confirmChanges(ctx, os.Stdin)
		if err != nil {
			return "", err
		}
		if !ok {
			return "", fmt.Errorf("run cancelled by user")
		}
	}

	revisionARN := plan.taskDefARN
	if register {
		containers, err := plan.containers(cfg.Container, cfg.ImageTag, cfg.SSMPrefixes)
		if err != nil {
			return "", err
		}
		revisionARN, err = registerTaskDefinition(ctx, clients.ECS, plan.existing, containers, plan.tags)
		if err != nil {
			return "", err
		}
		fmt.Printf("Registered new task definition: %s\n", revisionARN)
	}

	task := plan.releaseTaskFor(cfg.Cluster, cfg.Service, cfg.Container, revisionARN, cfg.Command, cfg.Timeout)
	if err := runReleaseTask(ctx, clients, task); err != nil {
		return revisionARN, err
	}
	return revisionARN, nil
}
