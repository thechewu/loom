package prompt

import (
	"strings"
	"testing"
)

func TestSentinelInSection(t *testing.T) {
	if !strings.Contains(ClaudeMDSection, Sentinel) {
		t.Error("ClaudeMDSection does not contain Sentinel")
	}
}

func TestClaudeMDSectionContainsCommands(t *testing.T) {
	commands := []string{
		"loom queue add",
		"loom queue list",
		"loom queue show",
		"loom status",
		"loom worker list",
		"loom logs",
	}
	for _, cmd := range commands {
		if !strings.Contains(ClaudeMDSection, cmd) {
			t.Errorf("ClaudeMDSection does not contain command %q", cmd)
		}
	}
}

func TestClaudeMDSectionContainsGuidance(t *testing.T) {
	sections := []string{
		"### When to Use Loom",
		"### When NOT to Use Loom",
		"### Writing Good Task Descriptions",
		"### Commands",
	}
	for _, section := range sections {
		if !strings.Contains(ClaudeMDSection, section) {
			t.Errorf("ClaudeMDSection does not contain guidance section %q", section)
		}
	}
}

func TestClaudeMDSectionNotEmpty(t *testing.T) {
	if len(ClaudeMDSection) <= 100 {
		t.Errorf("ClaudeMDSection is too short: %d bytes", len(ClaudeMDSection))
	}
}

func TestSentinelIsMarkdownHeader(t *testing.T) {
	if !strings.HasPrefix(Sentinel, "## ") {
		t.Errorf("Sentinel %q does not start with '## '", Sentinel)
	}
}
