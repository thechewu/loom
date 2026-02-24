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

Each worker gets its own git worktree (isolated branch named `loom/<bead-id>`). When a worker finishes, its changes are committed and queued for merge. A merger runs alongside the supervisor and merges branches into main one at a time. If a merge conflicts, the changes are dropped and the item is marked done anyway -- throughput over preservation.

Failed tasks are automatically retried up to 3 times before being permanently marked as failed. Workers that produce no output for the stuck timeout duration (default 10m) are killed and their tasks requeued.

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

This creates `.loom/` (workspace) and `.beads/` (issue database), starts a Dolt server, and appends loom usage instructions to the project's `CLAUDE.md` so that Claude CLI sessions automatically learn how to use loom.

### 2. Add to .gitignore

```bash
echo -e ".loom/\n.beads/" >> .gitignore
```

## Usage

### Queue work

```bash
loom queue add -t "Short title" -d "Detailed description of what the agent should do"
```

The `-d` description is the agent's prompt -- be specific. Workers start with zero context, so descriptions must be fully self-contained. Priority with `-p` (0=highest, 4=lowest, default 2):

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
3. Monitors worker activity (kills stuck workers, retries failed tasks)
4. Commits agent changes when done
5. Merges branches into main
6. Cleans up branches after merge

Only one `loom run` instance is allowed per project (enforced by lockfile).

Flags:
- `--workers N` -- max parallel agents (default 4)
- `--repo PATH` -- git repo path (default `.`)
- `--poll DURATION` -- poll interval (default `5s`)
- `--stuck-timeout DURATION` -- kill worker if no output for this long (default `10m`)
- `--agent CMD` -- agent command (default `claude`). Supported agents: `claude`, `kiro-cli`, or any custom command
- `--agent-args` -- extra args passed to the agent

### Using with different agents

Loom auto-detects known agent CLIs and uses their native headless flags:

```bash
# Claude (default)
loom run --workers 4

# Kiro
loom run --workers 4 --agent kiro-cli

# Custom agent (prompt passed as last positional arg)
loom run --workers 4 --agent my-agent --agent-args="--headless,--no-confirm"
```

### Queue work while running

From another terminal, queue more items. The running supervisor picks them up automatically:

```bash
loom queue add -t "Another task" -d "..."
```

### Monitor progress

```bash
loom status              # overall summary (pending/in_progress/done/failed counts)
loom queue list          # work items with status
loom queue show <id>     # item details including failure reasons and retry history
loom worker list         # worker states
loom logs                # last 20 lines from all active workers
loom logs <worker>       # tail a specific worker's output (follow mode)
```

### Manual merge

Merge all pending branches without running the full supervisor:

```bash
loom merge
```

### Claude CLI integration

Print the CLAUDE.md snippet that teaches Claude about loom (useful for manual review or custom CLAUDE.md files):

```bash
loom prompt
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
cmd/loom/main.go              CLI entry point (cobra commands)
pkg/supervisor/                Supervisor loop: polls, assigns, health-checks, stuck detection
pkg/worker/pool.go             Worker pool: spawns agents in worktrees, commits changes, retries
pkg/merger/merger.go           Merger: merges task branches (loom/<bead-id>) into main
pkg/beadsclient/client.go      Beads wrapper: work items, worker state, retry tracking
pkg/beadsclient/dolt.go        Dolt server lifecycle management
pkg/prompt/prompt.go           CLAUDE.md snippet injected by `loom init`
```

### Retry & Failure Handling

- Workers that exit non-zero have their tasks automatically requeued (up to 3 retries)
- Workers producing no output for `--stuck-timeout` are killed and their tasks requeued
- After exhausting retries, tasks are marked permanently failed with a reason
- Use `loom queue show <id>` to see failure reasons and retry history

### Branch Naming

Branches are named per-task (`loom/lm-xxx`), not per-worker. This prevents a race condition where a worker being reassigned would destroy the previous task's branch before the merger could merge it.

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

## License

MIT
