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
	AgentCmd    string   // e.g. "claude"
	AgentArgs   []string // e.g. ["--print"]
	RepoPath    string   // base git repo
	WorktreeDir string   // directory to create worktrees in
	Beads       *beadsclient.Client

	mu      sync.Mutex
	workers map[string]*runningWorker
}

type runningWorker struct {
	Name    string
	Cmd     *exec.Cmd
	Cancel  context.CancelFunc
	WorkDir string
	BeadID  string
}

// NewPool creates a worker pool.
func NewPool(beads *beadsclient.Client, repoPath, worktreeDir, agentCmd string, agentArgs []string, maxWorkers int) *Pool {
	return &Pool{
		MaxWorkers:  maxWorkers,
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

	// Create git worktree
	worktreePath := filepath.Join(p.WorktreeDir, name)
	branch := "loom/" + name
	if err := createWorktree(p.RepoPath, worktreePath, branch); err != nil {
		return fmt.Errorf("create worktree: %w", err)
	}

	// Write prompt and result files under .loom/ so they stay out of git
	loomSubdir := filepath.Join(worktreePath, ".loom")
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

	// Build agent command
	// Use -p (non-interactive print mode) with the prompt as a positional arg.
	// Also pass --dangerously-skip-permissions so the agent can write files
	// without interactive approval in the headless worktree.
	ctx, cancel := context.WithCancel(context.Background())
	args := []string{"-p", "--dangerously-skip-permissions"}
	args = append(args, p.AgentArgs...)
	args = append(args, prompt)

	cmd := exec.CommandContext(ctx, p.AgentCmd, args...)
	cmd.Dir = worktreePath

	// Build clean environment: strip CLAUDECODE to avoid nested session detection
	var cleanEnv []string
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "CLAUDECODE=") {
			cleanEnv = append(cleanEnv, e)
		}
	}
	cleanEnv = append(cleanEnv,
		"LOOM_WORKER="+name,
		"LOOM_BEAD_ID="+item.ID,
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

	rw := &runningWorker{
		Name:    name,
		Cmd:     cmd,
		Cancel:  cancel,
		WorkDir: worktreePath,
		BeadID:  item.ID,
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

	result := ""
	resultPath := filepath.Join(rw.WorkDir, ".loom", "result.txt")
	if data, readErr := os.ReadFile(resultPath); readErr == nil {
		result = string(data)
		if len(result) > 4096 {
			result = result[:4096] + "\n...(truncated)"
		}
	}

	if err != nil {
		p.Beads.FailWork(rw.BeadID, err.Error())
		p.Beads.UpdateWorkerBead(rw.Name, &beadsclient.WorkerFields{
			AgentState:    "dead",
			CurrentItem:   rw.BeadID,
			LastHeartbeat: time.Now().UTC().Format(time.RFC3339),
		})
		// On failure: clean up worktree immediately (nothing to merge)
		removeWorktree(p.RepoPath, rw.WorkDir)
	} else {
		// Commit any changes the agent made in the worktree
		branch := "loom/" + rw.Name
		committed, commitErr := commitWorktree(rw.WorkDir, fmt.Sprintf("loom: %s", rw.BeadID))
		if commitErr != nil {
			p.Beads.FailWork(rw.BeadID, "commit failed: "+commitErr.Error())
			p.Beads.UpdateWorkerBead(rw.Name, &beadsclient.WorkerFields{
				AgentState:    "dead",
				CurrentItem:   rw.BeadID,
				LastHeartbeat: time.Now().UTC().Format(time.RFC3339),
			})
			removeWorktree(p.RepoPath, rw.WorkDir)
		} else if committed {
			// Changes were committed — mark pending-merge and PRESERVE worktree
			p.Beads.MarkPendingMerge(rw.BeadID, branch)
			p.Beads.UpdateWorkerBead(rw.Name, &beadsclient.WorkerFields{
				AgentState:    "idle",
				LastHeartbeat: time.Now().UTC().Format(time.RFC3339),
			})
		} else {
			// No changes to merge — complete immediately and clean up
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
		// Re-check under lock — worker may have been removed by waitForCompletion
		p.mu.Lock()
		_, stillExists := p.workers[name]
		p.mu.Unlock()
		if !stillExists {
			continue // already cleaned up, skip
		}

		if p.IsAlive(name) {
			result[name] = "working"
			p.Beads.HeartbeatWorker(name)
		} else {
			// Wait briefly for waitForCompletion goroutine to clean up
			time.Sleep(500 * time.Millisecond)
			p.mu.Lock()
			_, finalCheck := p.workers[name]
			p.mu.Unlock()
			if finalCheck {
				result[name] = "dead"
			}
			// else: already cleaned up by waitForCompletion, not dead
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

// --- git commit helpers ---

// commitWorktree stages all changes and commits them in the given worktree.
// Returns true if changes were committed, false if the worktree was clean.
func commitWorktree(worktreePath, message string) (bool, error) {
	// Check if there are any changes (staged or unstaged)
	check := exec.Command("git", "diff", "--quiet", "HEAD")
	check.Dir = worktreePath
	if err := check.Run(); err == nil {
		// diff --quiet exits 0 when clean; also check for untracked files
		untracked := exec.Command("git", "ls-files", "--others", "--exclude-standard")
		untracked.Dir = worktreePath
		out, _ := untracked.Output()
		if len(strings.TrimSpace(string(out))) == 0 {
			return false, nil // no changes at all
		}
	}

	// Stage everything
	add := exec.Command("git", "add", "-A")
	add.Dir = worktreePath
	if out, err := add.CombinedOutput(); err != nil {
		return false, fmt.Errorf("git add: %s (%w)", strings.TrimSpace(string(out)), err)
	}

	// Commit
	commit := exec.Command("git", "commit", "-m", message)
	commit.Dir = worktreePath
	if out, err := commit.CombinedOutput(); err != nil {
		return false, fmt.Errorf("git commit: %s (%w)", strings.TrimSpace(string(out)), err)
	}

	return true, nil
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
