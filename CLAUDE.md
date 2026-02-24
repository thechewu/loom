# CLAUDE.md

This file provides guidance to Claude Code when working on the loom codebase.

## Project Overview

Loom is a lightweight multi-agent orchestrator for Git repos. It spawns parallel Claude agents in isolated git worktrees, commits their changes, and merges results back to main. State is tracked via beads (a Dolt-backed issue tracker).

## Build & Test

```bash
go build -o loom ./cmd/loom/       # build binary
go vet ./...                        # lint
```

No test suite yet — contributions welcome.

## Architecture

```
cmd/loom/main.go              CLI entry point (Cobra). All subcommands defined here.
pkg/supervisor/supervisor.go   Supervisor loop: polls beads for pending work, assigns
                               to workers, health-checks, activity-based stuck detection.
pkg/worker/pool.go             Worker pool: creates git worktrees, spawns `claude -p`
                               processes, commits changes on completion, retries on failure.
pkg/merger/merger.go           Merger: polls for pending-merge items, merges task branches
                               (loom/<bead-id>) into main. Drops conflicting merges.
pkg/beadsclient/client.go      Beads CLI wrapper: CRUD for work items and worker state,
                               retry tracking via RequeueOrFail.
pkg/beadsclient/dolt.go        Dolt server lifecycle (start/stop/status/init).
pkg/beadsclient/dolt_{unix,windows}.go    Platform-specific process detachment.
pkg/supervisor/process_{unix,windows}.go  Platform-specific process-alive checks.
pkg/prompt/prompt.go           Canonical CLAUDE.md snippet injected into projects by `loom init`.
```

## Key Design Decisions

- **Throughput over preservation**: If a merge conflicts, changes are dropped and the item is marked done. This keeps the pipeline moving.
- **Beads as state store**: All work items and worker state live in beads (Dolt-backed). No in-memory state survives restarts.
- **One worktree per worker**: Each agent gets full repo isolation via `git worktree add`.
- **Branches per task, not per worker**: Branches are named `loom/<bead-id>` so they survive worker reassignment. Prevents the race where a recycled worker destroys a pending-merge branch.
- **Headless agents**: Workers run `claude -p --dangerously-skip-permissions` — no interactive approval.
- **Automatic retries**: Failed tasks are requeued up to 3 times (configurable via MaxRetries). Stuck workers (no output growth for StuckTimeout) are killed and their tasks requeued.
- **CLAUDE.md injection**: `loom init` appends usage instructions to the project's CLAUDE.md so Claude CLI sessions automatically discover loom.

## CLI Commands

| Command | Description |
|---|---|
| `loom init [path]` | Initialize workspace + inject CLAUDE.md |
| `loom run` | Start supervisor (foreground) |
| `loom queue add` | Add work item |
| `loom queue list` | List all work items |
| `loom queue show <id>` | Show item details + failure reasons |
| `loom status` | Overall summary |
| `loom worker list` | Worker states |
| `loom logs [worker]` | Tail worker output |
| `loom merge` | Manual merge of pending branches |
| `loom prompt` | Print CLAUDE.md snippet |
| `loom dolt start/stop/status` | Manage Dolt server |

## Dependencies

- Go 1.25+, Git, Dolt, bd (beads CLI), Claude CLI
- Single Go dependency: `github.com/spf13/cobra`

## Adding a New Command

1. Define the `*cobra.Command` in `cmd/loom/main.go`
2. Register it in `init()` with `rootCmd.AddCommand()`
3. If it needs beads access, use the `openBeads()` helper
4. If it needs the loom directory, use `findLoomDir()`
