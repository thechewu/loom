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
                               to workers, health-checks, detects stuck workers.
pkg/worker/pool.go             Worker pool: creates git worktrees, spawns `claude -p`
                               processes, commits changes on completion.
pkg/merger/merger.go           Merger: polls for pending-merge items, merges worker
                               branches into main. Drops conflicting merges.
pkg/beadsclient/client.go      Beads CLI wrapper: CRUD for work items and worker state.
pkg/beadsclient/dolt.go        Dolt server lifecycle (start/stop/status/init).
pkg/beadsclient/dolt_{unix,windows}.go    Platform-specific process detachment.
pkg/supervisor/process_{unix,windows}.go  Platform-specific process-alive checks.
```

## Key Design Decisions

- **Throughput over preservation**: If a merge conflicts, changes are dropped and the item is marked done. This keeps the pipeline moving.
- **Beads as state store**: All work items and worker state live in beads (Dolt-backed). No in-memory state survives restarts.
- **One worktree per worker**: Each agent gets full repo isolation via `git worktree add`.
- **Headless agents**: Workers run `claude -p --dangerously-skip-permissions` — no interactive approval.

## Dependencies

- Go 1.25+, Git, Dolt, bd (beads CLI), Claude CLI
- Single Go dependency: `github.com/spf13/cobra`

## Adding a New Command

1. Define the `*cobra.Command` in `cmd/loom/main.go`
2. Register it in `init()` with `rootCmd.AddCommand()`
3. If it needs beads access, use the `openBeads()` helper
4. If it needs the loom directory, use `findLoomDir()`
