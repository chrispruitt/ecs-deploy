package main

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/chrispruitt/ecs-deploy/cmd"
)

func main() {
	ctx, stop := signal.NotifyContext(cmd.RootContext(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := cmd.Execute(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}
