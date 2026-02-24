package beadsclient

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// DoltServer manages the lifecycle of a Dolt SQL server process.
type DoltServer struct {
	Port    int
	DataDir string // .loom/.dolt-data/
	PidFile string // .loom/dolt.pid
	LogFile string // .loom/dolt.log
}

// NewDoltServer creates a DoltServer with paths rooted at loomDir.
func NewDoltServer(loomDir string) *DoltServer {
	port := 3307
	if p := os.Getenv("LOOM_DOLT_PORT"); p != "" {
		if n, err := strconv.Atoi(p); err == nil {
			port = n
		}
	}
	return &DoltServer{
		Port:    port,
		DataDir: filepath.Join(loomDir, ".dolt-data"),
		PidFile: filepath.Join(loomDir, "dolt.pid"),
		LogFile: filepath.Join(loomDir, "dolt.log"),
	}
}

// Init initializes the Dolt data directory and creates the "loom" database.
func (d *DoltServer) Init() error {
	// Ensure dolt identity is configured
	if err := ensureDoltIdentity(); err != nil {
		return err
	}

	dbDir := filepath.Join(d.DataDir, "loom")
	if _, err := os.Stat(filepath.Join(dbDir, ".dolt")); err == nil {
		return nil // already initialized
	}

	if err := os.MkdirAll(dbDir, 0o755); err != nil {
		return fmt.Errorf("create dolt data dir: %w", err)
	}

	cmd := exec.Command("dolt", "init")
	cmd.Dir = dbDir
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("dolt init: %s (%w)", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// Start starts the Dolt SQL server in the background.
func (d *DoltServer) Start() error {
	if d.IsRunning() {
		return nil
	}

	if err := os.MkdirAll(filepath.Dir(d.LogFile), 0o755); err != nil {
		return err
	}

	logFile, err := os.OpenFile(d.LogFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("open log file: %w", err)
	}

	cmd := exec.Command("dolt", "sql-server",
		"--port", strconv.Itoa(d.Port),
		"--data-dir", d.DataDir,
		"--max-connections", "50",
	)
	cmd.Stdout = logFile
	cmd.Stderr = logFile

	// Detach the process so it survives if loom exits
	setSysProcAttr(cmd)

	if err := cmd.Start(); err != nil {
		logFile.Close()
		return fmt.Errorf("start dolt server: %w", err)
	}

	// Write PID file
	if err := os.WriteFile(d.PidFile, []byte(strconv.Itoa(cmd.Process.Pid)), 0o644); err != nil {
		cmd.Process.Kill()
		logFile.Close()
		return fmt.Errorf("write pid file: %w", err)
	}

	// Don't wait on the process — it's a background server
	go func() {
		cmd.Wait()
		logFile.Close()
	}()

	// Wait for server to accept connections
	if err := d.waitReady(10 * time.Second); err != nil {
		d.Stop()
		return fmt.Errorf("dolt server not ready: %w", err)
	}

	return nil
}

// Stop stops the Dolt SQL server.
func (d *DoltServer) Stop() error {
	pid, err := d.readPID()
	if err != nil {
		return nil // no PID file, nothing to stop
	}

	proc, err := os.FindProcess(pid)
	if err != nil {
		os.Remove(d.PidFile)
		return nil
	}

	// Graceful shutdown
	if runtime.GOOS == "windows" {
		proc.Kill()
	} else {
		proc.Signal(syscall.SIGTERM)
	}

	// Wait up to 5 seconds
	for i := 0; i < 10; i++ {
		if !d.isProcessAlive(pid) {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}

	// Force kill if still running
	if d.isProcessAlive(pid) {
		proc.Kill()
	}

	os.Remove(d.PidFile)
	return nil
}

// IsRunning returns true if the Dolt server is accepting connections.
func (d *DoltServer) IsRunning() bool {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", d.Port), 2*time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// Status returns a human-readable status string.
func (d *DoltServer) Status() string {
	pid, _ := d.readPID()
	if d.IsRunning() {
		return fmt.Sprintf("running (pid=%d, port=%d)", pid, d.Port)
	}
	if pid > 0 {
		return fmt.Sprintf("dead (stale pid=%d, port=%d)", pid, d.Port)
	}
	return "stopped"
}

func (d *DoltServer) waitReady(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if d.IsRunning() {
			return nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Errorf("timeout after %s waiting for port %d", timeout, d.Port)
}

func (d *DoltServer) readPID() (int, error) {
	data, err := os.ReadFile(d.PidFile)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(data)))
}

func (d *DoltServer) isProcessAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	if runtime.GOOS == "windows" {
		handle, err := syscall.OpenProcess(syscall.PROCESS_QUERY_INFORMATION, false, uint32(pid))
		if err != nil {
			return false
		}
		syscall.CloseHandle(handle)
		return true
	}
	return proc.Signal(syscall.Signal(0)) == nil
}

// ensureDoltIdentity copies git user.name/user.email to dolt config if not set.
func ensureDoltIdentity() error {
	for _, key := range []string{"user.name", "user.email"} {
		cmd := exec.Command("dolt", "config", "--global", "--get", key)
		out, err := cmd.Output()
		if err == nil && len(strings.TrimSpace(string(out))) > 0 {
			continue // already set
		}

		// Copy from git
		gitOut, err := exec.Command("git", "config", "--global", key).Output()
		if err != nil || len(strings.TrimSpace(string(gitOut))) == 0 {
			return fmt.Errorf("dolt %s not configured and git %s not available; run: dolt config --global --add %s \"value\"", key, key, key)
		}

		// Unset first (ignore error), then add
		exec.Command("dolt", "config", "--global", "--unset", key).Run()
		if err := exec.Command("dolt", "config", "--global", "--add", key, strings.TrimSpace(string(gitOut))).Run(); err != nil {
			return fmt.Errorf("set dolt %s: %w", key, err)
		}
	}
	return nil
}

// setSysProcAttr is defined in platform-specific files:
// dolt_windows.go and dolt_unix.go
