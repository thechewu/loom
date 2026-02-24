// Package beadsclient wraps the beads (bd) CLI and Dolt server for loom's
// work queue and worker state management. All persistent state lives in beads.
package beadsclient

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	// Beads prefix for loom issues.
	Prefix = "lm"

	// Custom types registered with beads.
	CustomTypes = "work,worker"

	// Labels for filtering.
	LabelWork    = "loom:work"
	LabelWorker  = "loom:worker"
	LabelPending      = "loom:pending"
	LabelDone         = "loom:done"
	LabelFailed       = "loom:failed"
	LabelPendingMerge = "loom:pending-merge"
)

// Client wraps the bd CLI for beads operations.
type Client struct {
	BeadsDir string // path to .beads directory
	LoomDir  string // path to .loom directory
}

// NewClient creates a beads client. beadsDir is the directory containing
// the .beads database, loomDir is the .loom workspace directory.
func NewClient(beadsDir, loomDir string) *Client {
	return &Client{BeadsDir: beadsDir, LoomDir: loomDir}
}

// Init initializes the beads database with server mode and loom's custom types.
func (c *Client) Init() error {
	beadsDir := filepath.Join(c.BeadsDir, ".beads")
	if _, err := os.Stat(beadsDir); err == nil {
		// Already initialized, just ensure custom types
		return c.ensureCustomTypes()
	}

	// Initialize beads in server mode
	if _, err := c.run("bd", "init", "--prefix", Prefix, "--server"); err != nil {
		return fmt.Errorf("bd init: %w", err)
	}

	// Write metadata.json pointing to dolt server
	metadataPath := filepath.Join(beadsDir, "metadata.json")
	metadata := map[string]string{
		"database":      "dolt",
		"backend":       "dolt",
		"dolt_mode":     "server",
		"dolt_database": "loom",
	}
	data, _ := json.MarshalIndent(metadata, "", "  ")
	if err := os.WriteFile(metadataPath, data, 0o644); err != nil {
		return fmt.Errorf("write metadata: %w", err)
	}

	// Set prefix
	if _, err := c.run("bd", "config", "set", "issue_prefix", Prefix); err != nil {
		return fmt.Errorf("set prefix: %w", err)
	}

	// Run migrations
	c.run("bd", "migrate", "--yes") // ignore error, may not be needed

	return c.ensureCustomTypes()
}

func (c *Client) ensureCustomTypes() error {
	sentinelPath := filepath.Join(c.BeadsDir, ".beads", ".loom-types-configured")
	if _, err := os.Stat(sentinelPath); err == nil {
		return nil // already configured
	}

	if _, err := c.run("bd", "config", "set", "types.custom", CustomTypes); err != nil {
		return fmt.Errorf("set custom types: %w", err)
	}

	os.WriteFile(sentinelPath, []byte("ok"), 0o644)
	return nil
}

// --- Issue type ---

// Issue represents a beads issue.
type Issue struct {
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Status      string   `json:"status"`
	Priority    int      `json:"priority"`
	Type        string   `json:"issue_type"`
	CreatedAt   string   `json:"created_at"`
	UpdatedAt   string   `json:"updated_at"`
	Assignee    string   `json:"assignee,omitempty"`
	Labels      []string `json:"labels,omitempty"`
}

// HasLabel returns true if the issue has the given label.
func (i *Issue) HasLabel(label string) bool {
	for _, l := range i.Labels {
		if l == label {
			return true
		}
	}
	return false
}

// --- Work item operations ---

// CreateWorkItem creates a new work item bead and returns its ID.
func (c *Client) CreateWorkItem(title, description string, priority int) (string, error) {
	args := []string{"create", "--json",
		"--title", title,
		"--type", "work",
		"--priority", fmt.Sprintf("%d", priority),
		"--label", LabelWork,
		"--label", LabelPending,
	}
	if description != "" {
		args = append(args, "--description", description)
	}

	out, err := c.run("bd", args...)
	if err != nil {
		return "", fmt.Errorf("create work item: %w", err)
	}

	issue, err := c.parseIssue(out)
	if err != nil {
		return "", err
	}
	return issue.ID, nil
}

// ListPendingWork returns work items that are pending (open + loom:pending label).
func (c *Client) ListPendingWork() ([]Issue, error) {
	return c.listIssues("open", LabelPending, "work")
}

// ListAllWork returns all work items (open and closed).
func (c *Client) ListAllWork() ([]Issue, error) {
	open, err := c.listIssues("open", LabelWork, "work")
	if err != nil {
		return nil, err
	}
	closed, err := c.listIssues("closed", LabelWork, "work")
	if err != nil {
		return nil, err
	}
	return append(open, closed...), nil
}

// ClaimWork atomically assigns a pending work item to a worker.
// Returns nil, nil if no pending work is available.
func (c *Client) ClaimWork(workerName string) (*Issue, error) {
	pending, err := c.ListPendingWork()
	if err != nil {
		return nil, err
	}
	if len(pending) == 0 {
		return nil, nil
	}

	// Take the first (highest priority) item
	item := &pending[0]

	// Assign it atomically: set assignee + update labels
	if _, err := c.run("bd", "update", item.ID,
		"--status", "in_progress",
		"--assignee", workerName,
		"--remove-label", LabelPending,
	); err != nil {
		return nil, fmt.Errorf("claim %s: %w", item.ID, err)
	}

	item.Status = "in_progress"
	item.Assignee = workerName
	return item, nil
}

// CompleteWork marks a work item as done.
func (c *Client) CompleteWork(beadID, result string) error {
	args := []string{"close", beadID}
	if result != "" {
		args = append(args, "--reason", result)
	}
	if _, err := c.run("bd", args...); err != nil {
		return fmt.Errorf("complete %s: %w", beadID, err)
	}
	// Add done label
	c.run("bd", "update", beadID, "--add-label", LabelDone)
	return nil
}

// FailWork marks a work item as failed.
func (c *Client) FailWork(beadID, reason string) error {
	if _, err := c.run("bd", "update", beadID,
		"--add-label", LabelFailed,
		"--remove-label", LabelPending,
	); err != nil {
		return fmt.Errorf("fail %s: %w", beadID, err)
	}
	if reason != "" {
		c.run("bd", "comment", beadID, "Failed: "+reason)
	}
	return nil
}

// MarkPendingMerge marks a work item as awaiting merge and records the branch.
func (c *Client) MarkPendingMerge(beadID, branch string) error {
	if _, err := c.run("bd", "update", beadID,
		"--add-label", LabelPendingMerge,
	); err != nil {
		return fmt.Errorf("mark pending-merge %s: %w", beadID, err)
	}
	c.run("bd", "comment", beadID, "branch: "+branch)
	return nil
}

// ListPendingMerge returns work items awaiting merge.
func (c *Client) ListPendingMerge() ([]Issue, error) {
	return c.listIssues("", LabelPendingMerge, "work")
}

// MarkMerged closes a work item and marks it as done after a successful merge.
func (c *Client) MarkMerged(beadID string) error {
	args := []string{"close", beadID, "--reason", "merged"}
	if _, err := c.run("bd", args...); err != nil {
		return fmt.Errorf("mark merged %s: %w", beadID, err)
	}
	c.run("bd", "update", beadID,
		"--add-label", LabelDone,
		"--remove-label", LabelPendingMerge,
	)
	return nil
}

// RequeueWork resets a work item back to pending for retry.
func (c *Client) RequeueWork(beadID, reason string) error {
	if _, err := c.run("bd", "update", beadID,
		"--status", "open",
		"--assignee", "",
		"--add-label", LabelPending,
		"--remove-label", LabelFailed,
	); err != nil {
		return fmt.Errorf("requeue %s: %w", beadID, err)
	}
	if reason != "" {
		c.run("bd", "comment", beadID, "Requeued: "+reason)
	}
	return nil
}

// RequeueOrFail requeues a work item if it hasn't exceeded maxRetries,
// otherwise marks it as permanently failed. Returns true if requeued.
func (c *Client) RequeueOrFail(beadID, reason string, maxRetries int) (bool, error) {
	retries := c.CountRetries(beadID)
	if retries >= maxRetries {
		err := c.FailWork(beadID, fmt.Sprintf("exceeded %d retries, last error: %s", maxRetries, reason))
		return false, err
	}
	err := c.RequeueWork(beadID, fmt.Sprintf("(attempt %d/%d) %s", retries+1, maxRetries, reason))
	return err == nil, err
}

// CountRetries counts how many times a work item has been requeued
// by counting "Requeued:" comments.
func (c *Client) CountRetries(beadID string) int {
	out, err := c.run("bd", "show", beadID, "--json")
	if err != nil {
		return 0
	}
	return strings.Count(out, "Requeued:")
}

// GetIssue retrieves a single issue by ID.
func (c *Client) GetIssue(id string) (*Issue, error) {
	out, err := c.run("bd", "show", id, "--json")
	if err != nil {
		return nil, fmt.Errorf("get issue %s: %w", id, err)
	}
	return c.parseIssue(out)
}

// --- Worker bead operations ---

// WorkerFields holds structured data stored in a worker bead's description.
type WorkerFields struct {
	AgentState    string `json:"agent_state"`    // idle, working, stuck, dead
	PID           int    `json:"pid"`
	WorkDir       string `json:"work_dir"`
	CurrentItem   string `json:"current_item"`
	LastHeartbeat string `json:"last_heartbeat"`
}

// FormatWorkerDescription formats worker fields as a structured description.
func FormatWorkerDescription(fields *WorkerFields) string {
	return fmt.Sprintf("agent_state: %s\npid: %d\nwork_dir: %s\ncurrent_item: %s\nlast_heartbeat: %s",
		fields.AgentState, fields.PID, fields.WorkDir, fields.CurrentItem, fields.LastHeartbeat)
}

// ParseWorkerDescription extracts worker fields from a description string.
func ParseWorkerDescription(desc string) *WorkerFields {
	fields := &WorkerFields{}
	for _, line := range strings.Split(desc, "\n") {
		parts := strings.SplitN(line, ": ", 2)
		if len(parts) != 2 {
			continue
		}
		key, val := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
		switch key {
		case "agent_state":
			fields.AgentState = val
		case "pid":
			fmt.Sscanf(val, "%d", &fields.PID)
		case "work_dir":
			fields.WorkDir = val
		case "current_item":
			fields.CurrentItem = val
		case "last_heartbeat":
			fields.LastHeartbeat = val
		}
	}
	return fields
}

// CreateWorkerBead creates or updates a worker bead.
func (c *Client) CreateWorkerBead(name string, fields *WorkerFields) error {
	id := Prefix + "-" + name
	desc := FormatWorkerDescription(fields)

	// Try to create with explicit ID
	_, err := c.run("bd", "create", "--json",
		"--id="+id,
		"--title", "Worker: "+name,
		"--description", desc,
		"--type", "worker",
		"--label", LabelWorker,
		"--force",
	)
	if err != nil {
		return fmt.Errorf("create worker bead %s: %w", name, err)
	}
	return nil
}

// UpdateWorkerBead updates a worker bead's description with new fields.
func (c *Client) UpdateWorkerBead(name string, fields *WorkerFields) error {
	id := Prefix + "-" + name
	desc := FormatWorkerDescription(fields)

	_, err := c.run("bd", "update", id, "--description", desc)
	if err != nil {
		return fmt.Errorf("update worker bead %s: %w", name, err)
	}
	return nil
}

// HeartbeatWorker updates the worker's last_heartbeat timestamp.
func (c *Client) HeartbeatWorker(name string) error {
	id := Prefix + "-" + name
	issue, err := c.GetIssue(id)
	if err != nil {
		return err
	}

	fields := ParseWorkerDescription(issue.Description)
	fields.LastHeartbeat = time.Now().UTC().Format(time.RFC3339)
	return c.UpdateWorkerBead(name, fields)
}

// GetWorkerBead retrieves a worker bead and parses its fields.
func (c *Client) GetWorkerBead(name string) (*Issue, *WorkerFields, error) {
	id := Prefix + "-" + name
	issue, err := c.GetIssue(id)
	if err != nil {
		return nil, nil, err
	}
	fields := ParseWorkerDescription(issue.Description)
	return issue, fields, nil
}

// ListWorkers returns all worker beads.
func (c *Client) ListWorkers() ([]Issue, error) {
	return c.listIssues("", LabelWorker, "worker")
}

// CloseWorkerBead closes a worker bead (worker reaped).
func (c *Client) CloseWorkerBead(name string) error {
	id := Prefix + "-" + name
	_, err := c.run("bd", "close", id, "--reason", "reaped")
	return err
}

// --- Summary operations ---

// WorkSummary holds aggregate counts of work items.
type WorkSummary struct {
	Pending      int
	InProgress   int
	PendingMerge int
	Done         int
	Failed       int
	Total        int
}

// GetWorkSummary returns aggregate counts of work items by status.
func (c *Client) GetWorkSummary() (*WorkSummary, error) {
	items, err := c.ListAllWork()
	if err != nil {
		return nil, err
	}

	s := &WorkSummary{Total: len(items)}
	for _, item := range items {
		switch {
		case item.HasLabel(LabelPendingMerge):
			s.PendingMerge++
		case item.HasLabel(LabelPending):
			s.Pending++
		case item.HasLabel(LabelDone):
			s.Done++
		case item.HasLabel(LabelFailed):
			s.Failed++
		case item.Status == "in_progress":
			s.InProgress++
		}
	}
	return s, nil
}

// WorkerSummary holds aggregate counts of workers by state.
type WorkerSummary struct {
	Idle    int
	Working int
	Stuck   int
	Dead    int
	Total   int
}

// GetWorkerSummary returns aggregate counts of workers by state.
func (c *Client) GetWorkerSummary() (*WorkerSummary, error) {
	workers, err := c.ListWorkers()
	if err != nil {
		return nil, err
	}

	s := &WorkerSummary{Total: len(workers)}
	for _, w := range workers {
		fields := ParseWorkerDescription(w.Description)
		switch fields.AgentState {
		case "idle":
			s.Idle++
		case "working":
			s.Working++
		case "stuck":
			s.Stuck++
		case "dead":
			s.Dead++
		}
	}
	return s, nil
}

// --- Helpers ---

// Available returns true if the bd CLI is installed.
func (c *Client) Available() bool {
	_, err := exec.LookPath("bd")
	return err == nil
}

func (c *Client) listIssues(status, label, issueType string) ([]Issue, error) {
	args := []string{"list", "--json", "--limit", "0"}
	if status != "" {
		args = append(args, "--status", status)
	}
	if label != "" {
		args = append(args, "--label", label)
	}
	if issueType != "" {
		args = append(args, "--type", issueType)
	}

	out, err := c.run("bd", args...)
	if err != nil {
		return nil, fmt.Errorf("bd list: %w", err)
	}

	if strings.TrimSpace(out) == "" || strings.TrimSpace(out) == "[]" {
		return nil, nil
	}

	var issues []Issue
	if err := json.Unmarshal([]byte(out), &issues); err != nil {
		return nil, fmt.Errorf("parse issues: %w", err)
	}
	return issues, nil
}

func (c *Client) parseIssue(out string) (*Issue, error) {
	out = strings.TrimSpace(out)

	// bd show returns an array with one element
	var issues []Issue
	if err := json.Unmarshal([]byte(out), &issues); err == nil && len(issues) > 0 {
		return &issues[0], nil
	}

	// Or it might return a single object
	var issue Issue
	if err := json.Unmarshal([]byte(out), &issue); err != nil {
		return nil, fmt.Errorf("parse issue JSON: %w (raw: %s)", err, out)
	}
	return &issue, nil
}

func (c *Client) run(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = c.BeadsDir
	cmd.Env = append(os.Environ(), "BEADS_DIR="+filepath.Join(c.BeadsDir, ".beads"))

	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err != nil {
		errMsg := strings.TrimSpace(stderr.String())
		if errMsg == "" {
			errMsg = strings.TrimSpace(stdout.String())
		}
		return "", fmt.Errorf("%s %s: %s (%w)", name, strings.Join(args, " "), errMsg, err)
	}

	out := stdout.String()
	// If stdout is empty but stderr has content, bd may have written JSON to stderr
	if strings.TrimSpace(out) == "" {
		out = stderr.String()
	}

	// Strip any non-JSON lines (warnings) before the actual JSON content
	out = stripToJSON(out)
	return out, nil
}

// stripToJSON removes leading non-JSON lines (warnings, info messages)
// from bd CLI output, returning just the JSON portion.
func stripToJSON(s string) string {
	s = strings.TrimSpace(s)
	// Find first { or [
	for i, c := range s {
		if c == '{' || c == '[' {
			return s[i:]
		}
	}
	return s
}
