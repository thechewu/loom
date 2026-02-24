// Package prompt contains the canonical CLAUDE.md snippet that teaches
// Claude CLI sessions how to use loom for task orchestration.
package prompt

const ClaudeMDSection = `## Task Orchestration (Loom)

This project uses [loom](https://github.com/thechewu/loom) for parallel multi-agent task execution. When you identify multiple independent tasks, queue them via loom instead of doing them sequentially. A background supervisor assigns each task to a Claude agent running in an isolated git worktree.

### Commands

` + "```" + `bash
loom queue add -t "Short title" -d "Detailed, self-contained description"  # queue a task
loom queue add -t "Title" -d "Description" -p 0                           # high priority (0=highest, 4=lowest)
loom queue list                                                            # list all work items
loom status                                                                # overall summary
loom worker list                                                           # worker states
` + "```" + `

### Writing Good Task Descriptions

Worker agents start with **zero context** from this session. The ` + "`-d`" + ` description is the agent's entire prompt. Every description must be fully self-contained:

- **Name specific files and line numbers** when relevant
- **Describe the expected behavior**, not just "fix the bug"
- **Include constraints**: "Do not change any logic, just add type annotations"
- **One concern per task**: keep tasks scoped to avoid merge conflicts between workers

Bad: "Fix the issue we discussed"
Good: "In strategies.py, the stop_loss calculation on line 84 uses a hardcoded 0.1 multiplier. Change it to read from the atr_multiplier parameter passed to the constructor."

### Avoiding Merge Conflicts

Loom drops conflicting merges to maintain throughput. To minimize lost work:
- Assign tasks to **different files** when possible
- Avoid two tasks that both modify the same function
- For related changes to one file, queue them as a single task
`

// Sentinel is the marker string used to detect if the loom section
// has already been added to a CLAUDE.md file.
const Sentinel = "## Task Orchestration (Loom)"
