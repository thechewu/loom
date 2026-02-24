// Package merger polls for pending-merge work items and merges their
// worktree branches into the main branch. If a merge conflicts, the
// changes are dropped and the item is marked done anyway — we favor
// throughput over preserving every change.
package merger

import (
	"context"
	"fmt"
	"log"
	"os/exec"
	"strings"
	"time"

	"github.com/thechewu/loom/pkg/beadsclient"
)

// Merger polls for pending-merge items and merges their branches into main.
type Merger struct {
	Beads        *beadsclient.Client
	RepoPath     string        // main repo path
	WorktreeDir  string        // where worktrees live
	PollInterval time.Duration
	Logger       *log.Logger
}

// Run starts the merger poll loop. It blocks until ctx is cancelled.
func (m *Merger) Run(ctx context.Context) error {
	m.Logger.Printf("merger started: repo=%s poll=%s", m.RepoPath, m.PollInterval)

	ticker := time.NewTicker(m.PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			m.Logger.Println("merger shutting down...")
			return ctx.Err()
		case <-ticker.C:
			m.poll()
		}
	}
}

// MergeAll processes all pending-merge items once (for manual trigger).
func (m *Merger) MergeAll() (merged, dropped int, err error) {
	items, err := m.Beads.ListPendingMerge()
	if err != nil {
		return 0, 0, fmt.Errorf("list pending-merge: %w", err)
	}

	for _, item := range items {
		ok := m.mergeOne(item)
		if ok {
			merged++
		} else {
			dropped++
		}
	}
	return merged, dropped, nil
}

func (m *Merger) poll() {
	items, err := m.Beads.ListPendingMerge()
	if err != nil {
		m.Logger.Printf("merger: list pending-merge: %v", err)
		return
	}

	for _, item := range items {
		m.mergeOne(item)
	}
}

// mergeOne attempts to merge a task branch into main.
// Branch is named loom/<bead-id> (not loom/<worker-name>) so it survives
// worker reassignment. Always marks done and cleans up the branch.
func (m *Merger) mergeOne(item beadsclient.Issue) bool {
	branch := "loom/" + item.ID

	m.Logger.Printf("merging %s (branch %s) into main", item.ID, branch)

	// Ensure we're on main
	if err := m.gitRun("checkout", "main"); err != nil {
		m.Logger.Printf("merger: checkout main failed: %v — dropping %s", err, item.ID)
		m.finish(item.ID, branch)
		return false
	}

	// Attempt merge
	mergeCmd := exec.Command("git", "merge", branch, "-m",
		fmt.Sprintf("loom: merge %s (%s)", item.ID, item.Title))
	mergeCmd.Dir = m.RepoPath
	out, mergeErr := mergeCmd.CombinedOutput()

	if mergeErr != nil {
		m.Logger.Printf("merger: conflict on %s, dropping changes: %s",
			item.ID, strings.TrimSpace(string(out)))
		m.gitRun("merge", "--abort")
		m.finish(item.ID, branch)
		return false
	}

	m.Logger.Printf("merged %s successfully", item.ID)
	m.finish(item.ID, branch)
	return true
}

// finish marks a work item done and cleans up its branch.
// Worktree directories are managed by the worker pool, not the merger.
func (m *Merger) finish(beadID, branch string) {
	if err := m.Beads.MarkMerged(beadID); err != nil {
		m.Logger.Printf("merger: mark merged %s: %v", beadID, err)
	}
	if branch != "" {
		m.deleteBranch(branch)
	}
}

func (m *Merger) deleteBranch(branch string) {
	cmd := exec.Command("git", "branch", "-D", branch)
	cmd.Dir = m.RepoPath
	cmd.Run() // best-effort
}

func (m *Merger) gitRun(args ...string) error {
	cmd := exec.Command("git", args...)
	cmd.Dir = m.RepoPath
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git %s: %s (%w)", strings.Join(args, " "), strings.TrimSpace(string(out)), err)
	}
	return nil
}
