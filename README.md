# Loom

Lightweight multi-agent orchestration for Git repos. Loom spawns parallel Claude agents in isolated git worktrees, commits their changes, and merges results back to main.

## How It Works

```
loom queue add -t "task title" -d "what the agent should do"
loom run --workers 4

Worker 1 claims task → runs claude in worktree → commits changes → merger merges to main
Worker 2 claims task → runs claude in worktree → commits changes → merger merges to main
...
```

Each worker gets its own git worktree (isolated branch). When a worker finishes, its changes are committed and queued for merge. A merger agent runs alongside the supervisor and merges branches into main one at a time. If a merge conflicts, the changes are dropped and the item is marked done anyway -- throughput over preservation.

## Dependencies

| Dependency | Purpose | Install |
|---|---|---|
| **Go 1.25+** | Build loom | https://go.dev/dl/ |
| **Git** | Worktree management | https://git-scm.com/ |
| **Dolt** | SQL database for beads state | https://docs.dolthub.com/introduction/installation |
| **bd (beads CLI)** | Issue/work tracking | `go install github.com/steveyegge/gastown/cmd/bd@latest` |
| **Claude CLI** | AI agent (workers run `claude -p`) | https://docs.anthropic.com/en/docs/claude-code |

## Build

```bash
cd loom
go build -o loom ./cmd/loom/
```

Add the output directory to your PATH or move the binary somewhere on PATH.

## Setup

### 1. Initialize a project

```bash
cd /path/to/your/git/repo
loom init .
```

This creates `.loom/` (workspace) and `.beads/` (issue database), and starts a Dolt server.

### 2. Configure beads types

```bash
bd config set issue_prefix lm
bd config set types.custom "work,worker"
```

If your project already uses beads with custom types, append `work,worker` to the existing list.

### 3. Add to .gitignore

```bash
echo -e ".loom/\n.beads/" >> .gitignore
```

## Usage

### Queue work

```bash
loom queue add -t "Short title" -d "Detailed description of what the agent should do"
```

The `-d` description is the agent's prompt -- be specific. Priority with `-p` (0=highest, 4=lowest, default 2):

```bash
loom queue add -t "Critical fix" -d "..." -p 0
```

### Run the supervisor

```bash
loom run --workers 4 --repo .
```

This starts the supervisor (foreground) which:
1. Polls for pending work items
2. Spawns Claude agents in git worktrees
3. Commits agent changes when done
4. Merges branches into main
5. Cleans up worktrees

Only one `loom run` instance is allowed per project (enforced by lockfile).

Flags:
- `--workers N` -- max parallel agents (default 4)
- `--repo PATH` -- git repo path (default `.`)
- `--poll DURATION` -- poll interval (default `5s`)
- `--stuck-timeout DURATION` -- mark worker stuck after this (default `10m`)
- `--agent CMD` -- agent command (default `claude`)
- `--agent-args` -- extra args passed to the agent

### Queue work while running

From another terminal, queue more items. The running supervisor picks them up automatically:

```bash
loom queue add -t "Another task" -d "..."
```

### Manual merge

Merge all pending branches without running the full supervisor:

```bash
loom merge
```

### Check status

```bash
loom status          # overall summary
loom queue list      # work items
loom worker list     # worker states
loom dolt status     # dolt server
```

### Manage the Dolt server

```bash
loom dolt start
loom dolt stop
loom dolt status
```

The Dolt server runs independently -- it survives loom process exits. If you reboot or close your terminal, restart it with `loom dolt start` before running loom.

## Architecture

```
cmd/loom/main.go          CLI entry point (cobra commands)
pkg/supervisor/            Supervisor loop: polls, assigns, health-checks
pkg/worker/pool.go         Worker pool: spawns agents in worktrees, commits changes
pkg/merger/merger.go       Merger: merges worker branches into main
pkg/beadsclient/client.go  Beads wrapper: work items, worker state, labels
pkg/beadsclient/dolt.go    Dolt server lifecycle management
```

## Troubleshooting

**Dolt server not running:** Run `loom dolt start`. The server must be running before `loom run` or `loom queue add`.

**Items stuck in in_progress:** If loom was killed while workers were active, items can get orphaned. Requeue them manually:
```bash
bd update <bead-id> --status open --assignee "" --add-label loom:pending
```

**Stale worktrees/branches:** Clean up with:
```bash
rm -rf .loom/worktrees/*
git worktree prune
git branch | grep loom/ | xargs -r git branch -D
```

**"loom is already running":** A previous instance didn't shut down cleanly. Check if loom is actually running (`ps aux | grep loom`), then remove the lockfile:
```bash
rm .loom/loom.lock
```
