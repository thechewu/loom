package worker

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/thechewu/loom/pkg/beadsclient"
)

// Pool manages a set of worker agent processes, each in its own git worktree.
type Pool struct {
	MaxWorkers  int
	MaxRetries  int
	MaxDepth    int      // max recursion depth for subtask decomposition
	AgentCmd    string   // e.g. "claude"
	AgentArgs   []string // e.g. ["--print"]
	RepoPath    string   // base git repo
	WorktreeDir string   // directory to create worktrees in
	Beads       *beadsclient.Client

	mu      sync.Mutex
	workers map[string]*runningWorker
}

type runningWorker struct {
	Name           string
	Cmd            *exec.Cmd
	Cancel         context.CancelFunc
	WorkDir        string
	BeadID         string
	ResultPath     string
	StartedAt      time.Time
	LastOutputSize int64
	LastOutputAt   time.Time
	exited         bool // set by waitForCompletion; health check skips these
}

// NewPool creates a worker pool.
func NewPool(beads *beadsclient.Client, repoPath, worktreeDir, agentCmd string, agentArgs []string, maxWorkers, maxRetries, maxDepth int) *Pool {
	return &Pool{
		MaxWorkers:  maxWorkers,
		MaxRetries:  maxRetries,
		MaxDepth:    maxDepth,
		AgentCmd:    agentCmd,
		AgentArgs:   agentArgs,
		RepoPath:    repoPath,
		WorktreeDir: worktreeDir,
		Beads:       beads,
		workers:     make(map[string]*runningWorker),
	}
}

// Available returns how many more workers can be spawned.
func (p *Pool) Available() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := p.MaxWorkers - len(p.workers)
	if n < 0 {
		return 0
	}
	return n
}

// ActiveCount returns the number of currently active workers.
func (p *Pool) ActiveCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.workers)
}

// Spawn creates a git worktree and starts an agent process to work on the given item.
func (p *Pool) Spawn(name string, item *beadsclient.Issue) error {
	p.mu.Lock()
	if _, exists := p.workers[name]; exists {
		p.mu.Unlock()
		return fmt.Errorf("worker %s already exists", name)
	}
	p.mu.Unlock()

	// Create git worktree — branch is named per-task (not per-worker)
	// so the merger can find it even if the worker is reassigned.
	worktreePath := filepath.Join(p.WorktreeDir, name)
	branch := "loom/" + item.ID
	if err := createWorktree(p.RepoPath, worktreePath, branch); err != nil {
		return fmt.Errorf("create worktree: %w", err)
	}

	// Write prompt and result files under .loom-agent/ so they stay out of git
	// (Must NOT be named ".loom" — findLoomDir() would match it and
	//  agents running `loom queue add` would find the wrong workspace.)
	loomSubdir := filepath.Join(worktreePath, ".loom-agent")
	if err := os.MkdirAll(loomSubdir, 0o755); err != nil {
		removeWorktree(p.RepoPath, worktreePath)
		return fmt.Errorf("create .loom dir: %w", err)
	}

	promptPath := filepath.Join(loomSubdir, "prompt.md")
	prompt := fmt.Sprintf("# Task: %s\n\nBead ID: %s\n\n%s\n", item.Title, item.ID, item.Description)
	if err := os.WriteFile(promptPath, []byte(prompt), 0o644); err != nil {
		removeWorktree(p.RepoPath, worktreePath)
		return fmt.Errorf("write prompt: %w", err)
	}

	// Build agent command based on the agent type.
	ctx, cancel := context.WithCancel(context.Background())
	agentBin, args := buildAgentArgs(p.AgentCmd, p.AgentArgs, prompt)

	cmd := exec.CommandContext(ctx, agentBin, args...)
	cmd.Dir = worktreePath

	// Build clean environment
	var cleanEnv []string
	for _, e := range os.Environ() {
		// Strip session env vars that cause nested-agent detection
		if strings.HasPrefix(e, "CLAUDECODE=") {
			continue
		}
		// Prepend loom's own directory to PATH so workers can run `loom queue add`
		if strings.HasPrefix(e, "PATH=") {
			if self, err := os.Executable(); err == nil {
				e = "PATH=" + filepath.Dir(self) + string(os.PathListSeparator) + e[5:]
			}
		}
		cleanEnv = append(cleanEnv, e)
	}
	// Propagate recursion depth so subtasks know their level
	currentDepth := 0
	if v := os.Getenv("LOOM_DEPTH"); v != "" {
		fmt.Sscanf(v, "%d", &currentDepth)
	}
	cleanEnv = append(cleanEnv,
		"LOOM_WORKER="+name,
		"LOOM_BEAD_ID="+item.ID,
		"LOOM_DIR="+p.Beads.LoomDir,
		fmt.Sprintf("LOOM_DEPTH=%d", currentDepth+1),
		fmt.Sprintf("LOOM_MAX_DEPTH=%d", p.MaxDepth),
	)
	cmd.Env = cleanEnv

	// Capture output to result file
	resultPath := filepath.Join(loomSubdir, "result.txt")
	outFile, err := os.Create(resultPath)
	if err != nil {
		cancel()
		removeWorktree(p.RepoPath, worktreePath)
		return fmt.Errorf("create result file: %w", err)
	}
	cmd.Stdout = outFile
	cmd.Stderr = outFile

	if err := cmd.Start(); err != nil {
		outFile.Close()
		cancel()
		removeWorktree(p.RepoPath, worktreePath)
		return fmt.Errorf("start agent: %w", err)
	}

	now := time.Now()
	rw := &runningWorker{
		Name:           name,
		Cmd:            cmd,
		Cancel:         cancel,
		WorkDir:        worktreePath,
		BeadID:         item.ID,
		ResultPath:     resultPath,
		StartedAt:      now,
		LastOutputSize: 0,
		LastOutputAt:   now,
	}

	p.mu.Lock()
	p.workers[name] = rw
	p.mu.Unlock()

	// Create worker bead
	p.Beads.CreateWorkerBead(name, &beadsclient.WorkerFields{
		AgentState:    "working",
		PID:           cmd.Process.Pid,
		WorkDir:       worktreePath,
		CurrentItem:   item.ID,
		LastHeartbeat: time.Now().UTC().Format(time.RFC3339),
	})

	// Wait for completion in background
	go p.waitForCompletion(rw, outFile)

	return nil
}

func (p *Pool) waitForCompletion(rw *runningWorker, outFile *os.File) {
	err := rw.Cmd.Wait()
	outFile.Close()

	// Mark exited immediately so health checks don't race with post-exit
	// processing (commit, bead updates). Without this, a supervisor tick
	// between Wait() returning and delete(p.workers) can see the process
	// as dead and requeue the item while we're still committing it.
	p.mu.Lock()
	rw.exited = true
	p.mu.Unlock()

	result := ""
	resultPath := filepath.Join(rw.WorkDir, ".loom-agent", "result.txt")
	if data, readErr := os.ReadFile(resultPath); readErr == nil {
		result = string(data)
		if len(result) > 4096 {
			result = result[:4096] + "\n...(truncated)"
		}
	}

	if err != nil {
		requeued, _ := p.Beads.RequeueOrFail(rw.BeadID, err.Error(), p.MaxRetries)
		state := "dead"
		if requeued {
			state = "idle"
		}
		p.Beads.UpdateWorkerBead(rw.Name, &beadsclient.WorkerFields{
			AgentState:    state,
			CurrentItem:   rw.BeadID,
			LastHeartbeat: time.Now().UTC().Format(time.RFC3339),
		})
		removeWorktree(p.RepoPath, rw.WorkDir)
	} else {
		// Commit any changes the agent made in the worktree
		branch := "loom/" + rw.BeadID
		committed, commitErr := commitWorktree(rw.WorkDir, fmt.Sprintf("loom: %s", rw.BeadID))
		if commitErr != nil {
			requeued, _ := p.Beads.RequeueOrFail(rw.BeadID, "commit failed: "+commitErr.Error(), p.MaxRetries)
			state := "dead"
			if requeued {
				state = "idle"
			}
			p.Beads.UpdateWorkerBead(rw.Name, &beadsclient.WorkerFields{
				AgentState:    state,
				CurrentItem:   rw.BeadID,
				LastHeartbeat: time.Now().UTC().Format(time.RFC3339),
			})
			removeWorktree(p.RepoPath, rw.WorkDir)
		} else if committed {
			// Changes were committed — mark pending-merge, remove worktree
			// but PRESERVE the branch for the merger.
			p.Beads.MarkPendingMerge(rw.BeadID, branch)
			p.Beads.UpdateWorkerBead(rw.Name, &beadsclient.WorkerFields{
				AgentState:    "idle",
				LastHeartbeat: time.Now().UTC().Format(time.RFC3339),
			})
			removeWorktree(p.RepoPath, rw.WorkDir)
		} else {
			// No changes to merge — complete immediately and clean up
			comment := "Completed: no file changes"
			if len(result) > 0 {
				excerpt := result
				if len(excerpt) > 200 {
					excerpt = excerpt[:200] + "..."
				}
				comment += "\n" + excerpt
			}
			p.Beads.AddComment(rw.BeadID, comment)
			p.Beads.CompleteWork(rw.BeadID, result)
			p.Beads.UpdateWorkerBead(rw.Name, &beadsclient.WorkerFields{
				AgentState:    "idle",
				LastHeartbeat: time.Now().UTC().Format(time.RFC3339),
			})
			removeWorktree(p.RepoPath, rw.WorkDir)
		}
	}

	p.mu.Lock()
	delete(p.workers, rw.Name)
	p.mu.Unlock()
}

// Reap kills a worker process and cleans up its worktree.
func (p *Pool) Reap(name string) error {
	p.mu.Lock()
	rw, exists := p.workers[name]
	p.mu.Unlock()

	if !exists {
		// Not running, just clean up worktree and close bead
		worktreePath := filepath.Join(p.WorktreeDir, name)
		removeWorktree(p.RepoPath, worktreePath)
		p.Beads.CloseWorkerBead(name)
		return nil
	}

	rw.Cancel()
	rw.Cmd.Wait()

	p.mu.Lock()
	delete(p.workers, name)
	p.mu.Unlock()

	p.Beads.UpdateWorkerBead(name, &beadsclient.WorkerFields{
		AgentState:    "dead",
		CurrentItem:   rw.BeadID,
		LastHeartbeat: time.Now().UTC().Format(time.RFC3339),
	})

	removeWorktree(p.RepoPath, rw.WorkDir)
	p.Beads.CloseWorkerBead(name)

	return nil
}

// IsAlive checks if a worker's process is still running.
func (p *Pool) IsAlive(name string) bool {
	p.mu.Lock()
	rw, exists := p.workers[name]
	p.mu.Unlock()
	if !exists {
		return false
	}
	return isProcessAlive(rw.Cmd.Process)
}

// HealthCheck checks all workers and updates their beads.
// Only reports workers that are still in the pool (not already cleaned up
// by waitForCompletion).
func (p *Pool) HealthCheck() map[string]string {
	p.mu.Lock()
	names := make([]string, 0, len(p.workers))
	for name := range p.workers {
		names = append(names, name)
	}
	p.mu.Unlock()

	result := make(map[string]string, len(names))
	for _, name := range names {
		// Re-check under lock — worker may have been removed or exited
		p.mu.Lock()
		rw, stillExists := p.workers[name]
		completing := stillExists && rw.exited
		p.mu.Unlock()
		if !stillExists || completing {
			continue // cleaned up or waitForCompletion is handling it
		}

		if p.IsAlive(name) {
			result[name] = "working"
			p.Beads.HeartbeatWorker(name)
		} else {
			// Wait briefly for waitForCompletion goroutine to set exited flag
			time.Sleep(500 * time.Millisecond)
			p.mu.Lock()
			rw, finalCheck := p.workers[name]
			exited := finalCheck && rw.exited
			p.mu.Unlock()
			if finalCheck && !exited {
				result[name] = "dead"
			}
			// else: waitForCompletion is handling it or already cleaned up
		}
	}
	return result
}

// Shutdown gracefully stops all workers.
func (p *Pool) Shutdown() {
	p.mu.Lock()
	names := make([]string, 0, len(p.workers))
	for name := range p.workers {
		names = append(names, name)
	}
	p.mu.Unlock()

	for _, name := range names {
		p.Reap(name)
	}
}

// NextWorkerName generates the next available worker name.
func (p *Pool) NextWorkerName() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := 1; ; i++ {
		name := "worker-" + strconv.Itoa(i)
		if _, exists := p.workers[name]; !exists {
			return name
		}
	}
}

// GetRunningBeadID returns the bead ID a named worker is currently working on.
func (p *Pool) GetRunningBeadID(name string) string {
	p.mu.Lock()
	rw, exists := p.workers[name]
	p.mu.Unlock()
	if !exists {
		return ""
	}
	return rw.BeadID
}

// CheckActivity checks each worker's result file for output growth.
// Updates internal tracking and returns workers whose output hasn't grown
// since the last check, along with how long they've been inactive.
func (p *Pool) CheckActivity() map[string]time.Duration {
	stale := make(map[string]time.Duration)

	p.mu.Lock()
	names := make([]string, 0, len(p.workers))
	for name := range p.workers {
		names = append(names, name)
	}
	p.mu.Unlock()

	now := time.Now()
	for _, name := range names {
		p.mu.Lock()
		rw, exists := p.workers[name]
		completing := exists && rw.exited
		p.mu.Unlock()
		if !exists || completing {
			continue
		}

		var currentSize int64
		if info, err := os.Stat(rw.ResultPath); err == nil {
			currentSize = info.Size()
		}

		p.mu.Lock()
		if currentSize > rw.LastOutputSize {
			rw.LastOutputSize = currentSize
			rw.LastOutputAt = now
		}
		inactive := now.Sub(rw.LastOutputAt)
		p.mu.Unlock()

		stale[name] = inactive
	}
	return stale
}

// --- agent invocation ---

// buildAgentArgs returns the binary and arguments for running an agent
// in non-interactive headless mode. Supports known agents (claude, kiro)
// with their native flags, and a generic fallback for custom agents.
func buildAgentArgs(agentCmd string, extraArgs []string, prompt string) (string, []string) {
	// Normalize: extract the base command name (handle paths like /usr/bin/claude)
	base := filepath.Base(agentCmd)
	base = strings.TrimSuffix(base, ".exe")
	base = strings.TrimSuffix(base, "-cli")

	var args []string
	switch base {
	case "claude":
		// claude -p --dangerously-skip-permissions [extra-args] "prompt"
		args = []string{"-p", "--dangerously-skip-permissions"}
		args = append(args, extraArgs...)
		args = append(args, prompt)
	case "kiro":
		// kiro-cli chat --no-interactive --trust-all-tools [extra-args] "prompt"
		args = []string{"chat", "--no-interactive", "--trust-all-tools"}
		args = append(args, extraArgs...)
		args = append(args, prompt)
	default:
		// Generic: pass extra args then the prompt as the last positional arg
		args = append(args, extraArgs...)
		args = append(args, prompt)
	}
	return agentCmd, args
}

// --- git commit helpers ---

// commitWorktree stages all changes and commits them in the given worktree.
// Returns true if changes were committed (or the agent committed its own),
// false if the worktree is clean AND the branch has no new commits.
func commitWorktree(worktreePath, message string) (bool, error) {
	// Check if there are any uncommitted changes (staged or unstaged)
	hasUncommitted := false
	check := exec.Command("git", "diff", "--quiet", "HEAD")
	check.Dir = worktreePath
	if err := check.Run(); err != nil {
		hasUncommitted = true
	} else {
		// diff --quiet exits 0 when clean; also check for untracked files
		// (exclude .loom-agent/ which is loom's internal directory)
		untracked := exec.Command("git", "ls-files", "--others", "--exclude-standard", "--exclude", ".loom-agent/")
		untracked.Dir = worktreePath
		out, _ := untracked.Output()
		if len(strings.TrimSpace(string(out))) > 0 {
			hasUncommitted = true
		}
	}

	if hasUncommitted {
		// Stage everything except loom's internal agent files
		add := exec.Command("git", "add", "-A", "--", ".", ":!.loom-agent")
		add.Dir = worktreePath
		if out, err := add.CombinedOutput(); err != nil {
			return false, fmt.Errorf("git add: %s (%w)", strings.TrimSpace(string(out)), err)
		}

		// Check if anything was actually staged (the pathspec may have excluded all changes)
		staged := exec.Command("git", "diff", "--cached", "--quiet")
		staged.Dir = worktreePath
		if err := staged.Run(); err != nil {
			// Something staged — commit it
			commit := exec.Command("git", "commit", "-m", message)
			commit.Dir = worktreePath
			if out, err := commit.CombinedOutput(); err != nil {
				return false, fmt.Errorf("git commit: %s (%w)", strings.TrimSpace(string(out)), err)
			}
			return true, nil
		}
	}

	// Working tree is clean. But the agent (running with --dangerously-skip-permissions)
	// may have committed changes directly. Check if the branch is ahead of main.
	ahead := exec.Command("git", "log", "main..HEAD", "--oneline")
	ahead.Dir = worktreePath
	out, _ := ahead.Output()
	if len(strings.TrimSpace(string(out))) > 0 {
		return true, nil // agent committed its own changes
	}

	return false, nil // truly no changes
}

// --- git worktree helpers ---

func createWorktree(repoPath, worktreePath, branch string) error {
	// Prune stale worktree entries (e.g. from a previous crash)
	exec.Command("git", "worktree", "prune").Run()

	// Remove leftover directory if it exists
	if _, err := os.Stat(worktreePath); err == nil {
		removeWorktree(repoPath, worktreePath)
	}

	// Delete the branch if it already exists (leftover from previous run)
	exec.Command("git", "-C", repoPath, "branch", "-D", branch).Run()

	cmd := exec.Command("git", "worktree", "add", "-b", branch, worktreePath)
	cmd.Dir = repoPath
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git worktree add: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

func removeWorktree(repoPath, worktreePath string) {
	cmd := exec.Command("git", "worktree", "remove", "--force", worktreePath)
	cmd.Dir = repoPath
	cmd.Run() // best-effort
}

func isProcessAlive(p *os.Process) bool {
	if p == nil {
		return false
	}
	if runtime.GOOS == "windows" {
		handle, err := syscall.OpenProcess(syscall.PROCESS_QUERY_INFORMATION, false, uint32(p.Pid))
		if err != nil {
			return false
		}
		syscall.CloseHandle(handle)
		return true
	}
	return p.Signal(syscall.Signal(0)) == nil
}
