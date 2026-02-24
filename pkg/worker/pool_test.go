package worker

import (
	"slices"
	"testing"
)

func TestBuildAgentArgs(t *testing.T) {
	tests := []struct {
		name     string
		agentCmd string
		extra    []string
		prompt   string
		wantBin  string
		wantArgs []string
	}{
		{
			name:     "claude basic",
			agentCmd: "claude",
			extra:    nil,
			prompt:   "do task",
			wantBin:  "claude",
			wantArgs: []string{"-p", "--dangerously-skip-permissions", "do task"},
		},
		{
			name:     "claude with extra args",
			agentCmd: "claude",
			extra:    []string{"--model", "sonnet"},
			prompt:   "do task",
			wantBin:  "claude",
			wantArgs: []string{"-p", "--dangerously-skip-permissions", "--model", "sonnet", "do task"},
		},
		{
			name:     "claude with .exe suffix",
			agentCmd: "claude.exe",
			extra:    nil,
			prompt:   "prompt",
			wantBin:  "claude.exe",
			wantArgs: []string{"-p", "--dangerously-skip-permissions", "prompt"},
		},
		{
			name:     "claude with full path",
			agentCmd: "/usr/local/bin/claude",
			extra:    nil,
			prompt:   "prompt",
			wantBin:  "/usr/local/bin/claude",
			wantArgs: []string{"-p", "--dangerously-skip-permissions", "prompt"},
		},
		{
			name:     "kiro cli",
			agentCmd: "kiro-cli",
			extra:    nil,
			prompt:   "do task",
			wantBin:  "kiro-cli",
			wantArgs: []string{"chat", "--no-interactive", "--trust-all-tools", "do task"},
		},
		{
			name:     "kiro with extra args",
			agentCmd: "kiro-cli",
			extra:    []string{"--verbose"},
			prompt:   "do task",
			wantBin:  "kiro-cli",
			wantArgs: []string{"chat", "--no-interactive", "--trust-all-tools", "--verbose", "do task"},
		},
		{
			name:     "generic agent",
			agentCmd: "my-agent",
			extra:    []string{"--headless"},
			prompt:   "do stuff",
			wantBin:  "my-agent",
			wantArgs: []string{"--headless", "do stuff"},
		},
		{
			name:     "generic with no extra args",
			agentCmd: "custom-tool",
			extra:    nil,
			prompt:   "prompt",
			wantBin:  "custom-tool",
			wantArgs: []string{"prompt"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotBin, gotArgs := buildAgentArgs(tt.agentCmd, tt.extra, tt.prompt)
			if gotBin != tt.wantBin {
				t.Errorf("bin = %q, want %q", gotBin, tt.wantBin)
			}
			if !slices.Equal(gotArgs, tt.wantArgs) {
				t.Errorf("args = %v, want %v", gotArgs, tt.wantArgs)
			}
		})
	}
}
