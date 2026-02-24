package merger

import (
	"slices"
	"testing"
)

func TestTargetBranch(t *testing.T) {
	tests := []struct {
		name   string
		target string
		want   string
	}{
		{"empty defaults to main", "", "main"},
		{"explicit main", "main", "main"},
		{"loom branch", "loom", "loom"},
		{"custom branch", "develop", "develop"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &Merger{TargetBranch: tt.target}
			got := m.targetBranch()
			if got != tt.want {
				t.Errorf("targetBranch() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestBuildMergeAgentArgs(t *testing.T) {
	tests := []struct {
		name      string
		agentCmd  string
		extraArgs []string
		prompt    string
		wantBin   string
		wantArgs  []string
	}{
		{
			name:     "claude agent",
			agentCmd: "claude",
			prompt:   "resolve conflicts",
			wantBin:  "claude",
			wantArgs: []string{"-p", "--dangerously-skip-permissions", "resolve conflicts"},
		},
		{
			name:      "claude with extra args",
			agentCmd:  "claude",
			extraArgs: []string{"--model", "opus"},
			prompt:    "fix it",
			wantBin:   "claude",
			wantArgs:  []string{"-p", "--dangerously-skip-permissions", "--model", "opus", "fix it"},
		},
		{
			name:     "claude with .exe suffix",
			agentCmd: "claude.exe",
			prompt:   "prompt",
			wantBin:  "claude.exe",
			wantArgs: []string{"-p", "--dangerously-skip-permissions", "prompt"},
		},
		{
			name:     "claude with full path",
			agentCmd: "/usr/local/bin/claude",
			prompt:   "prompt",
			wantBin:  "/usr/local/bin/claude",
			wantArgs: []string{"-p", "--dangerously-skip-permissions", "prompt"},
		},
		{
			name:     "kiro agent",
			agentCmd: "kiro-cli",
			prompt:   "resolve",
			wantBin:  "kiro-cli",
			wantArgs: []string{"chat", "--no-interactive", "--trust-all-tools", "resolve"},
		},
		{
			name:      "kiro with extra args",
			agentCmd:  "kiro-cli",
			extraArgs: []string{"--verbose"},
			prompt:    "resolve",
			wantBin:   "kiro-cli",
			wantArgs:  []string{"chat", "--no-interactive", "--trust-all-tools", "--verbose", "resolve"},
		},
		{
			name:      "generic custom agent",
			agentCmd:  "my-agent",
			extraArgs: []string{"--headless"},
			prompt:    "do stuff",
			wantBin:   "my-agent",
			wantArgs:  []string{"--headless", "do stuff"},
		},
		{
			name:     "generic with no extra args",
			agentCmd: "custom",
			prompt:   "prompt",
			wantBin:  "custom",
			wantArgs: []string{"prompt"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotBin, gotArgs := buildMergeAgentArgs(tt.agentCmd, tt.extraArgs, tt.prompt)
			if gotBin != tt.wantBin {
				t.Errorf("bin = %q, want %q", gotBin, tt.wantBin)
			}
			if !slices.Equal(gotArgs, tt.wantArgs) {
				t.Errorf("args = %v, want %v", gotArgs, tt.wantArgs)
			}
		})
	}
}
