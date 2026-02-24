// Package merger polls for pending-merge work items and merges their
// worktree branches into the main branch. On conflict, it spawns an
// agent to resolve. If resolution fails, changes are dropped.
package merger

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
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
	AgentCmd     string        // agent command for conflict resolution (e.g. "claude")
	AgentArgs    []string      // extra agent args
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
// On conflict, spawns an agent to resolve. If that fails, drops the changes.
func (m *Merger) mergeOne(item beadsclient.Issue) bool {
	branch := "loom/" + item.ID

	m.Logger.Printf("merging %s (branch %s) into main", item.ID, branch)

	// Ensure we're on main
	if err := m.gitRun("checkout", "main"); err != nil {
		// Transient git state issue — leave item in pending-merge for retry
		m.Logger.Printf("merger: checkout main failed: %v — will retry %s next poll", err, item.ID)
		return false
	}

	// Attempt merge
	mergeCmd := exec.Command("git", "merge", branch, "-m",
		fmt.Sprintf("loom: merge %s (%s)", item.ID, item.Title))
	mergeCmd.Dir = m.RepoPath
	out, mergeErr := mergeCmd.CombinedOutput()

	if mergeErr == nil {
		m.Logger.Printf("merged %s successfully", item.ID)
		m.markMerged(item.ID, branch)
		return true
	}

	// Merge conflicted — try agent resolution
	m.Logger.Printf("merger: conflict on %s, attempting agent resolution", item.ID)

	if m.resolveConflicts(item) {
		m.Logger.Printf("merger: agent resolved conflicts for %s", item.ID)
		m.markMerged(item.ID, branch)
		return true
	}

	// Agent couldn't resolve — drop changes
	m.Logger.Printf("merger: resolution failed for %s, dropping changes: %s",
		item.ID, strings.TrimSpace(string(out)))
	m.gitRun("merge", "--abort")
	m.drop(item.ID, branch)
	return false
}

// resolveConflicts spawns an agent to resolve merge conflicts in the working tree.
// Returns true if conflicts were resolved and committed.
func (m *Merger) resolveConflicts(item beadsclient.Issue) bool {
	if m.AgentCmd == "" {
		return false
	}

	// Get list of conflicted files
	conflicted := m.getConflictedFiles()
	if len(conflicted) == 0 {
		return false
	}

	prompt := fmt.Sprintf(
		"There are merge conflicts in these files that need to be resolved:\n\n%s\n\n"+
			"The task being merged was: %s\n\n"+
			"Resolve all conflict markers (<<<<<<< ======= >>>>>>>) in each file. "+
			"Keep the intent of both sides where possible. If unsure, prefer the incoming changes (the task's work). "+
			"Do NOT modify any files that don't have conflict markers. "+
			"Do NOT add new features or make other changes.",
		strings.Join(conflicted, "\n"),
		item.Title,
	)

	agentBin, args := buildMergeAgentArgs(m.AgentCmd, m.AgentArgs, prompt)

	cmd := exec.Command(agentBin, args...)
	cmd.Dir = m.RepoPath

	// Clean environment
	var cleanEnv []string
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "CLAUDECODE=") {
			cleanEnv = append(cleanEnv, e)
		}
	}
	cleanEnv = append(cleanEnv, "LOOM_MERGE_RESOLVE=1")
	cmd.Env = cleanEnv

	// Capture output for logging
	logPath := filepath.Join(m.WorktreeDir, "..", "merge-resolve.log")
	logFile, err := os.Create(logPath)
	if err == nil {
		cmd.Stdout = logFile
		cmd.Stderr = logFile
		defer logFile.Close()
	}

	if err := cmd.Run(); err != nil {
		m.Logger.Printf("merger: resolve agent failed: %v", err)
		return false
	}

	// Check if conflicts are actually resolved
	if remaining := m.getConflictedFiles(); len(remaining) > 0 {
		m.Logger.Printf("merger: agent finished but %d files still conflicted", len(remaining))
		return false
	}

	// Stage resolved files and complete the merge commit
	if err := m.gitRun("add", "-A"); err != nil {
		m.Logger.Printf("merger: git add after resolve failed: %v", err)
		return false
	}

	commitCmd := exec.Command("git", "commit", "--no-edit")
	commitCmd.Dir = m.RepoPath
	if out, err := commitCmd.CombinedOutput(); err != nil {
		m.Logger.Printf("merger: commit after resolve failed: %s (%v)",
			strings.TrimSpace(string(out)), err)
		return false
	}

	return true
}

// getConflictedFiles returns files with unresolved merge conflicts.
func (m *Merger) getConflictedFiles() []string {
	cmd := exec.Command("git", "diff", "--name-only", "--diff-filter=U")
	cmd.Dir = m.RepoPath
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "\n")
}

// buildMergeAgentArgs constructs the agent invocation for conflict resolution.
func buildMergeAgentArgs(agentCmd string, extraArgs []string, prompt string) (string, []string) {
	base := filepath.Base(agentCmd)
	base = strings.TrimSuffix(base, ".exe")
	base = strings.TrimSuffix(base, "-cli")

	var args []string
	switch base {
	case "claude":
		args = []string{"-p", "--dangerously-skip-permissions"}
		args = append(args, extraArgs...)
		args = append(args, prompt)
	case "kiro":
		args = []string{"chat", "--no-interactive", "--trust-all-tools"}
		args = append(args, extraArgs...)
		args = append(args, prompt)
	default:
		args = append(args, extraArgs...)
		args = append(args, prompt)
	}
	return agentCmd, args
}

// markMerged marks a work item as successfully merged and cleans up its branch.
func (m *Merger) markMerged(beadID, branch string) {
	if err := m.Beads.MarkMerged(beadID); err != nil {
		m.Logger.Printf("merger: mark merged %s: %v", beadID, err)
	}
	m.deleteBranch(branch)
}

// drop marks a work item as done (changes dropped) and cleans up its branch.
func (m *Merger) drop(beadID, branch string) {
	if err := m.Beads.CompleteWork(beadID, "merge conflict — changes dropped"); err != nil {
		m.Logger.Printf("merger: complete (drop) %s: %v", beadID, err)
	}
	m.deleteBranch(branch)
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
