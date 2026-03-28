// Package queue defines River job types for the Mimir review pipeline.
// It isolates River-specific plumbing from the domain packages.
package queue

import (
	"github.com/riverqueue/river"
)

// ReviewJobArgs are the arguments for a PR review job enqueued by the
// webhook handler and processed by ReviewWorker.
type ReviewJobArgs struct {
	RepoFullName string `json:"repo_full_name"`
	PRNumber     int    `json:"pr_number"`
	GitHubPRID   int64  `json:"github_pr_id"`
	HeadSHA      string `json:"head_sha"`
	BaseSHA      string `json:"base_sha"`
	Author       string `json:"author"`
}

func (ReviewJobArgs) Kind() string { return "review" }

// InsertOpts returns default River insert options for review jobs.
// ADR-0003: 3 retries with backoff for infrastructure failures.
func (ReviewJobArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		MaxAttempts: 4, // 1 initial + 3 retries
		Queue:       "review",
	}
}
