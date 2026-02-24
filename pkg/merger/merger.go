// Package merger polls for pending-merge work items and merges their
// worktree branches into a configurable target branch. On conflict, it
// spawns an agent to resolve. If resolution fails, changes are dropped.
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

// Merger polls for pending-merge items and merges their branches into a target branch.
type Merger struct {
	Beads        *beadsclient.Client
	RepoPath     string        // main repo path
	WorktreeDir  string        // where worktrees live
	PollInterval time.Duration
	AgentCmd     string        // agent command for conflict resolution (e.g. "claude")
	AgentArgs    []string      // extra agent args
	TargetBranch string        // branch to merge into ("main", "loom", etc.)
	Logger       *log.Logger
}

// targetBranch returns the configured target branch, defaulting to "main".
func (m *Merger) targetBranch() string {
	if m.TargetBranch == "" {
		return "main"
	}
	return m.TargetBranch
}

// Run starts the merger poll loop. It blocks until ctx is cancelled.
func (m *Merger) Run(ctx context.Context) error {
	m.Logger.Printf("merger started: repo=%s target=%s poll=%s", m.RepoPath, m.targetBranch(), m.PollInterval)

	// Ensure target branch exists (for non-main targets, create from main if missing)
	if m.targetBranch() != "main" {
		if err := m.ensureTargetBranch(); err != nil {
			return fmt.Errorf("ensure target branch: %w", err)
		}
	}

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

// mergeOne attempts to merge a task branch into the target branch.
// On conflict, spawns an agent to resolve. If that fails, drops the changes.
func (m *Merger) mergeOne(item beadsclient.Issue) bool {
	branch := "loom/" + item.ID
	m.Logger.Printf("merging %s (branch %s) into %s", item.ID, branch, m.targetBranch())

	// Phase 1: Pre-merge validation
	if !m.ensureCleanTarget(item.ID) {
		return false
	}
	if !m.validateBranch(item.ID, branch) {
		return false
	}

	// Phase 2: Attempt merge (dry-run then real)
	files, mergeErr := m.attemptMerge(item, branch)
	if mergeErr == nil {
		m.recordSuccess(item.ID, branch, files)
		return true
	}

	// Phase 3: Conflict resolution
	m.Logger.Printf("merger: conflict on %s, attempting agent resolution", item.ID)
	if m.resolveConflicts(item) {
		m.Logger.Printf("merger: agent resolved conflicts for %s", item.ID)
		files = m.getMergedFiles()
		m.recordSuccess(item.ID, branch, files)
		return true
	}

	// Phase 4: Drop
	conflicted := m.getConflictedFiles()
	m.gitRun("merge", "--abort")
	m.recordFailure(item.ID, branch, conflicted)
	return false
}

// validateBranch checks the branch exists, has commits ahead of the target, and isn't already merged.
func (m *Merger) validateBranch(beadID, branch string) bool {
	// Branch exists
	if err := m.gitRun("rev-parse", "--verify", branch); err != nil {
		m.Logger.Printf("merger: branch %s does not exist for %s", branch, beadID)
		m.Beads.FailWork(beadID, fmt.Sprintf("branch %s does not exist", branch))
		return false
	}

	// Has commits ahead of target
	out, err := m.gitOutput("log", m.targetBranch()+".."+branch, "--oneline")
	if err != nil {
		m.Logger.Printf("merger: check commits %s: %v", branch, err)
		return false
	}
	if out == "" {
		m.Logger.Printf("merger: branch %s has no commits ahead of %s, skipping %s", branch, m.targetBranch(), beadID)
		m.Beads.MarkMerged(beadID)
		m.deleteBranch(branch)
		return false
	}

	// Not already merged into target
	if err := m.gitRun("merge-base", "--is-ancestor", branch, m.targetBranch()); err == nil {
		m.Logger.Printf("merger: branch %s already merged into %s, skipping %s", branch, m.targetBranch(), beadID)
		m.Beads.MarkMerged(beadID)
		m.deleteBranch(branch)
		return false
	}

	return true
}

// ensureCleanTarget checks out the target branch and verifies a clean working tree.
func (m *Merger) ensureCleanTarget(beadID string) bool {
	if err := m.gitRun("checkout", m.targetBranch()); err != nil {
		m.Logger.Printf("merger: checkout %s failed: %v — will retry %s next poll", m.targetBranch(), err, beadID)
		return false
	}

	out, err := m.gitOutput("status", "--porcelain")
	if err != nil {
		m.Logger.Printf("merger: git status failed: %v", err)
		return false
	}
	if out != "" {
		m.Logger.Printf("merger: %s has uncommitted changes, will retry %s next poll:\n%s", m.targetBranch(), beadID, out)
		return false
	}

	return true
}

// ensureTargetBranch creates the target branch from main if it doesn't exist.
func (m *Merger) ensureTargetBranch() error {
	target := m.targetBranch()
	if err := m.gitRun("rev-parse", "--verify", target); err != nil {
		m.Logger.Printf("merger: creating branch %s from main", target)
		return m.gitRun("branch", target, "main")
	}
	return nil
}

// attemptMerge does a dry-run merge to detect conflicts, then commits if clean.
// On conflict, leaves the merge state in place for resolveConflicts and returns an error.
func (m *Merger) attemptMerge(item beadsclient.Issue, branch string) ([]string, error) {
	// Dry-run: merge without committing to detect conflicts
	dryCmd := exec.Command("git", "merge", "--no-commit", "--no-ff", branch)
	dryCmd.Dir = m.RepoPath
	_, dryErr := dryCmd.CombinedOutput()

	if dryErr != nil {
		// Conflict — leave merge state in place for resolution
		return nil, fmt.Errorf("merge conflict")
	}

	// Dry-run succeeded — capture file list, abort, then do real merge
	files := m.getStagedFiles()
	m.gitRun("merge", "--abort")

	// Real merge with enriched commit message
	commitMsg := fmt.Sprintf("loom: merge %s (%s)", item.ID, item.Title)
	if len(files) > 0 {
		commitMsg += "\n\nFiles: " + strings.Join(files, ", ")
	}

	mergeCmd := exec.Command("git", "merge", branch, "-m", commitMsg)
	mergeCmd.Dir = m.RepoPath
	out, mergeErr := mergeCmd.CombinedOutput()
	if mergeErr != nil {
		return nil, fmt.Errorf("merge failed: %s", strings.TrimSpace(string(out)))
	}

	return files, nil
}

// getStagedFiles returns files staged in the index (used during dry-run merge).
func (m *Merger) getStagedFiles() []string {
	out, err := m.gitOutput("diff", "--cached", "--name-only")
	if err != nil || out == "" {
		return nil
	}
	return strings.Split(out, "\n")
}

// getMergedFiles returns files changed in the most recent commit.
func (m *Merger) getMergedFiles() []string {
	out, err := m.gitOutput("diff", "HEAD~1", "--name-only")
	if err != nil || out == "" {
		return nil
	}
	return strings.Split(out, "\n")
}

// getDiffStats returns the total lines added and removed in the most recent commit.
func (m *Merger) getDiffStats() (added, removed int) {
	out, err := m.gitOutput("diff", "HEAD~1", "--numstat")
	if err != nil {
		return 0, 0
	}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 {
			var a, r int
			fmt.Sscanf(fields[0], "%d", &a)
			fmt.Sscanf(fields[1], "%d", &r)
			added += a
			removed += r
		}
	}
	return
}

// recordSuccess marks merged, enriches the commit message, deletes branch, and adds audit comment.
func (m *Merger) recordSuccess(beadID, branch string, files []string) {
	added, removed := m.getDiffStats()

	// Enrich merge commit message with file list if not already present
	if len(files) > 0 {
		msg, err := m.gitOutput("log", "-1", "--format=%B")
		if err == nil && !strings.Contains(msg, "Files:") {
			enriched := strings.TrimSpace(msg) + "\n\nFiles: " + strings.Join(files, ", ")
			m.gitRun("commit", "--amend", "-m", enriched)
		}
	}

	// Add audit comment to the bead
	comment := fmt.Sprintf("Merged: %d files changed (+%d, -%d)", len(files), added, removed)
	if len(files) > 0 {
		comment += ": " + strings.Join(files, ", ")
	}
	m.Logger.Printf("merger: %s %s", beadID, comment)
	if err := m.Beads.AddComment(beadID, comment); err != nil {
		m.Logger.Printf("merger: add comment %s: %v", beadID, err)
	}

	if err := m.Beads.MarkMerged(beadID); err != nil {
		m.Logger.Printf("merger: mark merged %s: %v", beadID, err)
	}
	m.deleteBranch(branch)
}

// recordFailure marks failed, deletes branch, and adds audit comment with conflict details.
func (m *Merger) recordFailure(beadID, branch string, conflicted []string) {
	reason := "merge conflict — changes dropped"
	if len(conflicted) > 0 {
		reason = fmt.Sprintf("merge conflict in %s — changes dropped", strings.Join(conflicted, ", "))
	}
	m.Logger.Printf("merger: %s failed: %s", beadID, reason)
	if err := m.Beads.FailWork(beadID, reason); err != nil {
		m.Logger.Printf("merger: fail (drop) %s: %v", beadID, err)
	}
	m.deleteBranch(branch)
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

	commitMsg := fmt.Sprintf("loom: merge %s (%s)", item.ID, item.Title)
	commitCmd := exec.Command("git", "commit", "-m", commitMsg)
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

// gitOutput runs a git command and returns its trimmed stdout.
func (m *Merger) gitOutput(args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = m.RepoPath
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %s (%w)", strings.Join(args, " "), strings.TrimSpace(string(out)), err)
	}
	return strings.TrimSpace(string(out)), nil
}
