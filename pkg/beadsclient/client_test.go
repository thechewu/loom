package beadsclient

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestStripToJSON(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "leading warning before JSON array",
			input: "some warning\n[{\"id\":\"lm-1\"}]",
			want:  "[{\"id\":\"lm-1\"}]",
		},
		{
			name:  "no leading junk",
			input: "{\"key\":\"val\"}",
			want:  "{\"key\":\"val\"}",
		},
		{
			name:  "multiple warning lines before JSON object",
			input: "warning 1\nwarning 2\n{\"key\":\"val\"}",
			want:  "{\"key\":\"val\"}",
		},
		{
			name:  "no JSON found returns as-is",
			input: "no json here",
			want:  "no json here",
		},
		{
			name:  "empty string",
			input: "",
			want:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := stripToJSON(tt.input)
			if got != tt.want {
				t.Errorf("stripToJSON(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestParseWorkerDescription(t *testing.T) {
	t.Run("full description", func(t *testing.T) {
		desc := "agent_state: working\npid: 1234\nwork_dir: /tmp/w1\ncurrent_item: lm-5\nlast_heartbeat: 2025-01-01T00:00:00Z"
		fields := ParseWorkerDescription(desc)

		if fields.AgentState != "working" {
			t.Errorf("AgentState = %q, want %q", fields.AgentState, "working")
		}
		if fields.PID != 1234 {
			t.Errorf("PID = %d, want %d", fields.PID, 1234)
		}
		if fields.WorkDir != "/tmp/w1" {
			t.Errorf("WorkDir = %q, want %q", fields.WorkDir, "/tmp/w1")
		}
		if fields.CurrentItem != "lm-5" {
			t.Errorf("CurrentItem = %q, want %q", fields.CurrentItem, "lm-5")
		}
		if fields.LastHeartbeat != "2025-01-01T00:00:00Z" {
			t.Errorf("LastHeartbeat = %q, want %q", fields.LastHeartbeat, "2025-01-01T00:00:00Z")
		}
	})

	t.Run("empty string", func(t *testing.T) {
		fields := ParseWorkerDescription("")
		if fields.AgentState != "" {
			t.Errorf("AgentState = %q, want empty", fields.AgentState)
		}
		if fields.PID != 0 {
			t.Errorf("PID = %d, want 0", fields.PID)
		}
		if fields.WorkDir != "" {
			t.Errorf("WorkDir = %q, want empty", fields.WorkDir)
		}
		if fields.CurrentItem != "" {
			t.Errorf("CurrentItem = %q, want empty", fields.CurrentItem)
		}
		if fields.LastHeartbeat != "" {
			t.Errorf("LastHeartbeat = %q, want empty", fields.LastHeartbeat)
		}
	})

	t.Run("partial description", func(t *testing.T) {
		fields := ParseWorkerDescription("agent_state: idle")
		if fields.AgentState != "idle" {
			t.Errorf("AgentState = %q, want %q", fields.AgentState, "idle")
		}
		if fields.PID != 0 {
			t.Errorf("PID = %d, want 0", fields.PID)
		}
		if fields.WorkDir != "" {
			t.Errorf("WorkDir = %q, want empty", fields.WorkDir)
		}
		if fields.CurrentItem != "" {
			t.Errorf("CurrentItem = %q, want empty", fields.CurrentItem)
		}
		if fields.LastHeartbeat != "" {
			t.Errorf("LastHeartbeat = %q, want empty", fields.LastHeartbeat)
		}
	})

	t.Run("partial with two fields", func(t *testing.T) {
		fields := ParseWorkerDescription("agent_state: idle\npid: 999")
		if fields.AgentState != "idle" {
			t.Errorf("AgentState = %q, want %q", fields.AgentState, "idle")
		}
		if fields.PID != 999 {
			t.Errorf("PID = %d, want 999", fields.PID)
		}
		if fields.WorkDir != "" {
			t.Errorf("WorkDir = %q, want empty", fields.WorkDir)
		}
		if fields.CurrentItem != "" {
			t.Errorf("CurrentItem = %q, want empty", fields.CurrentItem)
		}
		if fields.LastHeartbeat != "" {
			t.Errorf("LastHeartbeat = %q, want empty", fields.LastHeartbeat)
		}
	})

	t.Run("malformed lines skipped", func(t *testing.T) {
		fields := ParseWorkerDescription("garbage line\nagent_state: working\nno-separator\npid: 42")
		if fields.AgentState != "working" {
			t.Errorf("AgentState = %q, want %q", fields.AgentState, "working")
		}
		if fields.PID != 42 {
			t.Errorf("PID = %d, want 42", fields.PID)
		}
	})
}

func TestFormatWorkerDescription(t *testing.T) {
	t.Run("contains all fields", func(t *testing.T) {
		fields := &WorkerFields{
			AgentState:    "working",
			PID:           42,
			WorkDir:       "/tmp",
			CurrentItem:   "lm-1",
			LastHeartbeat: "2025-01-01T00:00:00Z",
		}
		got := FormatWorkerDescription(fields)

		for _, want := range []string{
			"agent_state: working",
			"pid: 42",
			"work_dir: /tmp",
			"current_item: lm-1",
			"last_heartbeat: 2025-01-01T00:00:00Z",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("FormatWorkerDescription output %q does not contain %q", got, want)
			}
		}

		// Verify newline-separated structure
		lines := strings.Split(got, "\n")
		if len(lines) != 5 {
			t.Errorf("expected 5 newline-separated lines, got %d: %q", len(lines), got)
		}
	})

	t.Run("round trip", func(t *testing.T) {
		original := &WorkerFields{
			AgentState:    "working",
			PID:           42,
			WorkDir:       "/tmp",
			CurrentItem:   "lm-1",
			LastHeartbeat: "2025-01-01T00:00:00Z",
		}
		desc := FormatWorkerDescription(original)
		parsed := ParseWorkerDescription(desc)

		if parsed.AgentState != original.AgentState {
			t.Errorf("AgentState round-trip: got %q, want %q", parsed.AgentState, original.AgentState)
		}
		if parsed.PID != original.PID {
			t.Errorf("PID round-trip: got %d, want %d", parsed.PID, original.PID)
		}
		if parsed.WorkDir != original.WorkDir {
			t.Errorf("WorkDir round-trip: got %q, want %q", parsed.WorkDir, original.WorkDir)
		}
		if parsed.CurrentItem != original.CurrentItem {
			t.Errorf("CurrentItem round-trip: got %q, want %q", parsed.CurrentItem, original.CurrentItem)
		}
		if parsed.LastHeartbeat != original.LastHeartbeat {
			t.Errorf("LastHeartbeat round-trip: got %q, want %q", parsed.LastHeartbeat, original.LastHeartbeat)
		}
	})
}

func TestIssueHasLabel(t *testing.T) {
	t.Run("label present", func(t *testing.T) {
		issue := &Issue{Labels: []string{"loom:work", "loom:pending"}}
		if !issue.HasLabel("loom:work") {
			t.Error("HasLabel(\"loom:work\") = false, want true")
		}
	})

	t.Run("label absent", func(t *testing.T) {
		issue := &Issue{Labels: []string{"loom:work", "loom:pending"}}
		if issue.HasLabel("loom:done") {
			t.Error("HasLabel(\"loom:done\") = true, want false")
		}
	})

	t.Run("nil labels", func(t *testing.T) {
		issue := &Issue{Labels: nil}
		if issue.HasLabel("anything") {
			t.Error("HasLabel on nil Labels = true, want false")
		}
	})

	t.Run("empty labels", func(t *testing.T) {
		issue := &Issue{Labels: []string{}}
		if issue.HasLabel("anything") {
			t.Error("HasLabel on empty Labels = true, want false")
		}
	})

	t.Run("empty string label", func(t *testing.T) {
		issue := &Issue{Labels: []string{"loom:work", "loom:pending"}}
		if issue.HasLabel("") {
			t.Error("HasLabel(\"\") = true, want false")
		}
	})
}

// --- WorkspaceLabel tests ---

func TestWorkspaceLabel(t *testing.T) {
	t.Run("normal workspace", func(t *testing.T) {
		c := &Client{Workspace: "myproject"}
		got := c.WorkspaceLabel()
		want := "loom:ws:myproject"
		if got != want {
			t.Errorf("WorkspaceLabel() = %q, want %q", got, want)
		}
	})

	t.Run("empty workspace", func(t *testing.T) {
		c := &Client{Workspace: ""}
		got := c.WorkspaceLabel()
		want := "loom:ws:"
		if got != want {
			t.Errorf("WorkspaceLabel() = %q, want %q", got, want)
		}
	})
}

// --- NewClient tests ---

func TestNewClient(t *testing.T) {
	t.Run("derives workspace from path", func(t *testing.T) {
		c := NewClient("/path/to/myrepo", "/path/to/myrepo/.loom")
		if c.Workspace != "myrepo" {
			t.Errorf("Workspace = %q, want %q", c.Workspace, "myrepo")
		}
		if c.BeadsDir != "/path/to/myrepo" {
			t.Errorf("BeadsDir = %q, want %q", c.BeadsDir, "/path/to/myrepo")
		}
		if c.LoomDir != "/path/to/myrepo/.loom" {
			t.Errorf("LoomDir = %q, want %q", c.LoomDir, "/path/to/myrepo/.loom")
		}
	})

	t.Run("dot path resolved to absolute", func(t *testing.T) {
		c := NewClient(".", ".loom")
		abs, _ := filepath.Abs(".")
		want := filepath.Base(abs)
		if c.Workspace != want {
			t.Errorf("Workspace = %q, want %q (from abs of \".\")", c.Workspace, want)
		}
	})
}
