package supervisor

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/thechewu/loom/pkg/beadsclient"
	"github.com/thechewu/loom/pkg/merger"
	"github.com/thechewu/loom/pkg/worker"
)

// Supervisor is the main control loop: it polls beads for pending work,
// assigns items to available workers, health-checks running workers,
// and handles failures with retries.
type Supervisor struct {
	Beads        *beadsclient.Client
	Dolt         *beadsclient.DoltServer
	Pool         *worker.Pool
	Merger       *merger.Merger
	LoomDir      string
	PollInterval time.Duration
	StuckTimeout time.Duration
	MaxRetries   int
	Logger       *log.Logger
}

// Config holds supervisor configuration.
type Config struct {
	LoomDir      string        // .loom directory
	RepoPath     string        // git repo for worktrees
	AgentCmd     string        // agent command (e.g. "claude")
	AgentArgs    []string      // extra agent args
	MaxWorkers   int           // max concurrent workers
	PollInterval time.Duration // poll interval
	StuckTimeout time.Duration // mark worker stuck after this
	MaxRetries   int           // per-item retry limit
}

// DefaultConfig returns sensible defaults.
func DefaultConfig() Config {
	return Config{
		LoomDir:      ".loom",
		AgentCmd:     "claude",
		MaxWorkers:   4,
		PollInterval: 5 * time.Second,
		StuckTimeout: 10 * time.Minute,
		MaxRetries:   3,
	}
}

// New creates a Supervisor from config.
func New(cfg Config) (*Supervisor, error) {
	loomDir := cfg.LoomDir
	beadsDir := cfg.RepoPath // beads database lives at repo root
	if beadsDir == "" {
		beadsDir = "."
	}

	dolt := beadsclient.NewDoltServer(loomDir)
	beads := beadsclient.NewClient(beadsDir, loomDir)
	worktreeDir := filepath.Join(loomDir, "worktrees")

	pool := worker.NewPool(beads, cfg.RepoPath, worktreeDir, cfg.AgentCmd, cfg.AgentArgs, cfg.MaxWorkers)

	m := &merger.Merger{
		Beads:        beads,
		RepoPath:     cfg.RepoPath,
		WorktreeDir:  worktreeDir,
		PollInterval: cfg.PollInterval,
		Logger:       log.Default(),
	}

	return &Supervisor{
		Beads:        beads,
		Dolt:         dolt,
		Pool:         pool,
		Merger:       m,
		LoomDir:      loomDir,
		PollInterval: cfg.PollInterval,
		StuckTimeout: cfg.StuckTimeout,
		MaxRetries:   cfg.MaxRetries,
		Logger:       log.Default(),
	}, nil
}

// Run starts the supervisor loop. It blocks until ctx is cancelled.
func (sv *Supervisor) Run(ctx context.Context) error {
	// Acquire lockfile — only one supervisor per project
	if err := sv.acquireLock(); err != nil {
		return err
	}
	defer sv.releaseLock()

	// Ensure Dolt server is running
	if !sv.Dolt.IsRunning() {
		sv.Logger.Println("starting Dolt server...")
		if err := sv.Dolt.Start(); err != nil {
			return fmt.Errorf("start dolt: %w", err)
		}
	}

	sv.Logger.Printf("supervisor started: max_workers=%d poll=%s stuck_timeout=%s",
		sv.Pool.MaxWorkers, sv.PollInterval, sv.StuckTimeout)

	// Start merger in background
	go func() {
		if err := sv.Merger.Run(ctx); err != nil && ctx.Err() == nil {
			sv.Logger.Printf("merger exited: %v", err)
		}
	}()

	ticker := time.NewTicker(sv.PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			sv.Logger.Println("supervisor shutting down...")
			sv.Pool.Shutdown()
			return ctx.Err()
		case <-ticker.C:
			sv.tick()
		}
	}
}

func (sv *Supervisor) tick() {
	// 1. Health-check all workers
	sv.checkHealth()

	// 2. Handle stale workers
	sv.handleStale()

	// 3. Assign pending work to available workers
	sv.assignWork()
}

func (sv *Supervisor) checkHealth() {
	statuses := sv.Pool.HealthCheck()
	for name, status := range statuses {
		if status == "dead" {
			// Double-check the worker is still in the pool (it may have
			// already been cleaned up by waitForCompletion)
			if !sv.Pool.IsAlive(name) && sv.Pool.GetRunningBeadID(name) != "" {
				sv.Logger.Printf("worker %s is dead, handling failure", name)
				sv.handleDeadWorker(name)
			}
		}
	}
}

func (sv *Supervisor) handleDeadWorker(name string) {
	beadID := sv.Pool.GetRunningBeadID(name)
	if beadID != "" {
		// Only requeue if the item is still in_progress (not already completed/failed)
		issue, err := sv.Beads.GetIssue(beadID)
		if err == nil && issue != nil && issue.Status == "in_progress" {
			sv.Beads.RequeueWork(beadID, fmt.Sprintf("worker %s died, retrying", name))
			sv.Logger.Printf("requeued %s (worker %s died)", beadID, name)
		}
	}
	sv.Pool.Reap(name)
}

func (sv *Supervisor) handleStale() {
	workers, err := sv.Beads.ListWorkers()
	if err != nil {
		return
	}

	for _, w := range workers {
		if w.Status == "closed" {
			continue
		}
		fields := beadsclient.ParseWorkerDescription(w.Description)
		if fields.AgentState != "working" {
			continue
		}

		// Check if heartbeat is stale
		if fields.LastHeartbeat == "" {
			continue
		}
		lastSeen, err := time.Parse(time.RFC3339, fields.LastHeartbeat)
		if err != nil {
			continue
		}
		if time.Since(lastSeen) > sv.StuckTimeout {
			name := w.Title
			// Strip "Worker: " prefix
			if len(name) > 8 && name[:8] == "Worker: " {
				name = name[8:]
			}
			sv.Logger.Printf("worker %s is stale (last heartbeat %s ago), reaping",
				name, time.Since(lastSeen).Truncate(time.Second))
			sv.handleDeadWorker(name)
		}
	}
}

func (sv *Supervisor) assignWork() {
	for sv.Pool.Available() > 0 {
		name := sv.Pool.NextWorkerName()
		item, err := sv.Beads.ClaimWork(name)
		if err != nil {
			sv.Logger.Printf("error claiming work: %v", err)
			return
		}
		if item == nil {
			return // queue empty
		}

		sv.Logger.Printf("assigning %s (%s) to %s", item.ID, item.Title, name)

		if err := sv.Pool.Spawn(name, item); err != nil {
			sv.Logger.Printf("error spawning %s: %v", name, err)
			sv.Beads.FailWork(item.ID, "spawn failed: "+err.Error())
			return // stop assigning this tick to avoid hot-looping
		}
	}
}

// --- lockfile ---

func (sv *Supervisor) lockPath() string {
	return filepath.Join(sv.LoomDir, "loom.lock")
}

func (sv *Supervisor) acquireLock() error {
	path := sv.lockPath()

	// Check for existing lock
	if data, err := os.ReadFile(path); err == nil {
		pidStr := strings.TrimSpace(string(data))
		if pid, err := strconv.Atoi(pidStr); err == nil {
			if isProcessRunning(pid) {
				return fmt.Errorf("loom is already running (pid %d). Only one instance allowed per project", pid)
			}
			// Stale lockfile — process is dead
			sv.Logger.Printf("removing stale lockfile (pid %s)", pidStr)
		}
	}

	// Write our PID
	if err := os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
		return fmt.Errorf("write lockfile: %w", err)
	}
	return nil
}

func (sv *Supervisor) releaseLock() {
	os.Remove(sv.lockPath())
}
