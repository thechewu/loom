package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/thechewu/loom/pkg/beadsclient"
)

func TestResolveOutcome(t *testing.T) {
	tests := []struct {
		name     string
		issue    beadsclient.Issue
		comments []string
		want     string
	}{
		{
			name:     "failed with merge conflict",
			issue:    beadsclient.Issue{Labels: []string{beadsclient.LabelFailed}},
			comments: []string{"Failed: merge conflict in file.py — changes dropped"},
			want:     "merge dropped — merge conflict in file.py — changes dropped",
		},
		{
			name:     "failed with branch missing",
			issue:    beadsclient.Issue{Labels: []string{beadsclient.LabelFailed}},
			comments: []string{"Failed: branch loom/lm-abc does not exist"},
			want:     "failed (branch missing)",
		},
		{
			name:     "failed with retries exhausted",
			issue:    beadsclient.Issue{Labels: []string{beadsclient.LabelFailed}},
			comments: []string{"Failed: exceeded 3 retries, last error: spawn failed"},
			want:     "failed (retries exhausted)",
		},
		{
			name:     "failed generic",
			issue:    beadsclient.Issue{Labels: []string{beadsclient.LabelFailed}},
			comments: []string{"Failed: some error"},
			want:     "failed — some error",
		},
		{
			name:     "failed no comments",
			issue:    beadsclient.Issue{Labels: []string{beadsclient.LabelFailed}},
			comments: nil,
			want:     "failed",
		},
		{
			name:     "pending merge",
			issue:    beadsclient.Issue{Labels: []string{beadsclient.LabelPendingMerge}},
			comments: nil,
			want:     "pending merge",
		},
		{
			name:     "done with merge audit",
			issue:    beadsclient.Issue{Labels: []string{beadsclient.LabelDone}},
			comments: []string{"Merged: 3 files changed (+45, -12): a.go, b.go, c.go"},
			want:     "Merged: 3 files changed (+45, -12): a.go, b.go, c.go",
		},
		{
			name: "done no changes with subtasks",
			issue: beadsclient.Issue{
				Labels:      []string{beadsclient.LabelDone},
				CloseReason: "I created subtasks using loom queue add",
			},
			comments: []string{"Completed: no file changes"},
			want:     "completed (subtasks created — see result)",
		},
		{
			name: "done no changes with result",
			issue: beadsclient.Issue{
				Labels:      []string{beadsclient.LabelDone},
				CloseReason: "analysis complete",
			},
			comments: []string{"Completed: no file changes"},
			want:     "completed (no changes needed — see result)",
		},
		{
			name:     "done no changes no result",
			issue:    beadsclient.Issue{Labels: []string{beadsclient.LabelDone}},
			comments: []string{"Completed: no file changes"},
			want:     "completed (no changes needed)",
		},
		{
			name: "done legacy merged",
			issue: beadsclient.Issue{
				Labels:      []string{beadsclient.LabelDone},
				CloseReason: "merged",
			},
			comments: nil,
			want:     "merged",
		},
		{
			name: "done legacy with result",
			issue: beadsclient.Issue{
				Labels:      []string{beadsclient.LabelDone},
				CloseReason: "some result text",
			},
			comments: nil,
			want:     "completed — see result",
		},
		{
			name:     "done bare",
			issue:    beadsclient.Issue{Labels: []string{beadsclient.LabelDone}},
			comments: nil,
			want:     "completed",
		},
		{
			name:     "pending",
			issue:    beadsclient.Issue{Labels: []string{beadsclient.LabelPending}},
			comments: nil,
			want:     "queued",
		},
		{
			name:     "in progress",
			issue:    beadsclient.Issue{Status: "in_progress"},
			comments: nil,
			want:     "in progress",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveOutcome(tt.issue, tt.comments)
			if got != tt.want {
				t.Errorf("resolveOutcome() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestEffectiveStatus(t *testing.T) {
	tests := []struct {
		name  string
		issue beadsclient.Issue
		want  string
	}{
		{
			name:  "done label",
			issue: beadsclient.Issue{Labels: []string{beadsclient.LabelDone}},
			want:  "done",
		},
		{
			name:  "failed label",
			issue: beadsclient.Issue{Labels: []string{beadsclient.LabelFailed}},
			want:  "failed",
		},
		{
			name:  "pending-merge label",
			issue: beadsclient.Issue{Labels: []string{beadsclient.LabelPendingMerge}},
			want:  "pending-merge",
		},
		{
			name:  "pending label",
			issue: beadsclient.Issue{Labels: []string{beadsclient.LabelPending}},
			want:  "pending",
		},
		{
			name:  "fallback to status field",
			issue: beadsclient.Issue{Status: "in_progress"},
			want:  "in_progress",
		},
		{
			name:  "done takes precedence over failed",
			issue: beadsclient.Issue{Labels: []string{beadsclient.LabelDone, beadsclient.LabelFailed}},
			want:  "done",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := effectiveStatus(tt.issue)
			if got != tt.want {
				t.Errorf("effectiveStatus() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestLoadConfigFile(t *testing.T) {
	t.Run("non-existent directory", func(t *testing.T) {
		cfg := loadConfigFile("/nonexistent/path/that/does/not/exist")
		if cfg != (loomConfig{}) {
			t.Errorf("loadConfigFile() = %+v, want zero value", cfg)
		}
	})

	t.Run("valid config", func(t *testing.T) {
		dir := t.TempDir()
		err := os.WriteFile(filepath.Join(dir, "config.json"),
			[]byte(`{"merge_mode":"loom","merge_branch":"develop"}`), 0o644)
		if err != nil {
			t.Fatal(err)
		}

		cfg := loadConfigFile(dir)
		if cfg.MergeMode != "loom" {
			t.Errorf("MergeMode = %q, want %q", cfg.MergeMode, "loom")
		}
		if cfg.MergeBranch != "develop" {
			t.Errorf("MergeBranch = %q, want %q", cfg.MergeBranch, "develop")
		}
	})

	t.Run("empty config", func(t *testing.T) {
		dir := t.TempDir()
		err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{}`), 0o644)
		if err != nil {
			t.Fatal(err)
		}

		cfg := loadConfigFile(dir)
		if cfg != (loomConfig{}) {
			t.Errorf("loadConfigFile() = %+v, want zero value", cfg)
		}
	})

	t.Run("invalid JSON", func(t *testing.T) {
		dir := t.TempDir()
		err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`not json`), 0o644)
		if err != nil {
			t.Fatal(err)
		}

		cfg := loadConfigFile(dir)
		if cfg != (loomConfig{}) {
			t.Errorf("loadConfigFile() = %+v, want zero value", cfg)
		}
	})
}

func TestTruncate(t *testing.T) {
	tests := []struct {
		name string
		s    string
		max  int
		want string
	}{
		{"short string", "hello", 10, "hello"},
		{"truncated", "hello world", 8, "hello..."},
		{"exact at max", "hi", 2, "hi"},
		{"exact length", "abc", 3, "abc"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := truncate(tt.s, tt.max)
			if got != tt.want {
				t.Errorf("truncate(%q, %d) = %q, want %q", tt.s, tt.max, got, tt.want)
			}
		})
	}
}

func TestContainsSubtaskSignal(t *testing.T) {
	tests := []struct {
		name string
		s    string
		want bool
	}{
		{"loom queue add mention", "I used loom queue add to create tasks", true},
		{"subtask mention", "Created subtask for validation", true},
		{"sub-task mention", "Created sub-task items", true},
		{"no signal", "All work completed directly", false},
		{"empty string", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := containsSubtaskSignal(tt.s)
			if got != tt.want {
				t.Errorf("containsSubtaskSignal(%q) = %v, want %v", tt.s, got, tt.want)
			}
		})
	}
}

func TestExtractAfter(t *testing.T) {
	tests := []struct {
		name   string
		s      string
		prefix string
		want   string
	}{
		{"prefix found", "Failed: merge conflict", "Failed: ", "merge conflict"},
		{"prefix not found", "no prefix here", "Failed: ", "no prefix here"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractAfter(tt.s, tt.prefix)
			if got != tt.want {
				t.Errorf("extractAfter(%q, %q) = %q, want %q", tt.s, tt.prefix, got, tt.want)
			}
		})
	}
}

func TestDefaultStr(t *testing.T) {
	tests := []struct {
		name string
		s    string
		def  string
		want string
	}{
		{"non-empty returns s", "hello", "default", "hello"},
		{"empty returns default", "", "default", "default"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := defaultStr(tt.s, tt.def)
			if got != tt.want {
				t.Errorf("defaultStr(%q, %q) = %q, want %q", tt.s, tt.def, got, tt.want)
			}
		})
	}
}
