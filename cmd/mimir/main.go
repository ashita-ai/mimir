package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
	"go.uber.org/zap"
)

func main() {
	log, err := zap.NewProduction()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to initialize logger: %v\n", err)
		os.Exit(1)
	}
	defer log.Sync() //nolint:errcheck

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := newRootCmd(log).ExecuteContext(ctx); err != nil {
		os.Exit(1)
	}
}

func newRootCmd(log *zap.Logger) *cobra.Command {
	root := &cobra.Command{
		Use:   "mimir",
		Short: "AI-powered PR review harness",
		Long: `Mimir is an open-source AI PR review harness that builds semantic context
slices per changed function and routes them through configurable model and
static analysis pipelines to produce high-signal, low-noise code review findings.`,
		SilenceUsage: true,
	}

	root.AddCommand(newServeCmd(log))
	root.AddCommand(newReviewCmd(log))

	return root
}

func newServeCmd(log *zap.Logger) *cobra.Command {
	var (
		addr    string
		workers int
	)

	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Start the webhook receiver and pipeline workers",
		Long: `serve starts a chi HTTP server to receive GitHub webhook events and
one or more river worker pools to process review jobs. Both run in the same
process. Scale horizontally by running multiple instances against the same DB.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			log.Info("serve not yet implemented",
				zap.String("addr", addr),
				zap.Int("workers", workers),
			)
			// TODO(M1): wire chi server + river workers
			return nil
		},
	}

	cmd.Flags().StringVar(&addr, "addr", ":8080", "HTTP listen address")
	cmd.Flags().IntVar(&workers, "workers", 4, "number of river worker goroutines (0 = no workers)")

	return cmd
}

func newReviewCmd(log *zap.Logger) *cobra.Command {
	var (
		repo     string
		prNumber int
	)

	cmd := &cobra.Command{
		Use:   "review",
		Short: "Run a one-shot review of a single PR",
		Long: `review fetches a PR, runs the full analysis pipeline, and prints findings
to stdout. Does not require a running server. Useful for local development and CI.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if repo == "" {
				return fmt.Errorf("--repo is required (e.g. ashita-ai/mimir)")
			}
			if prNumber <= 0 {
				return fmt.Errorf("--pr must be a positive integer")
			}
			log.Info("review not yet implemented",
				zap.String("repo", repo),
				zap.Int("pr", prNumber),
			)
			// TODO(M1): wire one-shot review pipeline
			return nil
		},
	}

	cmd.Flags().StringVar(&repo, "repo", "", "repository in owner/name format")
	cmd.Flags().IntVar(&prNumber, "pr", 0, "pull request number")

	return cmd
}
