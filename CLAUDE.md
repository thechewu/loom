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
                               (loom/<bead-id>) into main. Spawns agent to resolve conflicts;
                               drops changes only if resolution fails.
pkg/beadsclient/client.go      Beads CLI wrapper: CRUD for work items and worker state,
                               retry tracking via RequeueOrFail.
pkg/beadsclient/dolt.go        Dolt server lifecycle (start/stop/status/init).
pkg/beadsclient/dolt_{unix,windows}.go    Platform-specific process detachment.
pkg/supervisor/process_{unix,windows}.go  Platform-specific process-alive checks.
pkg/prompt/prompt.go           Canonical CLAUDE.md snippet injected into projects by `loom init`.
```

## Key Design Decisions

- **Conflict resolution before dropping**: If a merge conflicts, the merger spawns an agent to resolve conflict markers. If resolution fails, changes are dropped and the item is marked done. This keeps the pipeline moving.
- **Beads as state store**: All work items and worker state live in beads (Dolt-backed). No in-memory state survives restarts.
- **One worktree per worker**: Each agent gets full repo isolation via `git worktree add`.
- **Branches per task, not per worker**: Branches are named `loom/<bead-id>` so they survive worker reassignment. Prevents the race where a recycled worker destroys a pending-merge branch.
- **Headless agents**: Workers run in non-interactive mode. Agent invocation is auto-detected: `claude -p --dangerously-skip-permissions`, `kiro-cli chat --no-interactive --trust-all-tools`, or generic (prompt as last arg). See `buildAgentArgs()` in `pkg/worker/pool.go`.
- **Automatic retries**: Failed tasks are requeued up to 3 times (configurable via MaxRetries). Stuck workers (no output growth for StuckTimeout) are killed and their tasks requeued.
- **CLAUDE.md injection**: `loom init` appends usage instructions to the project's CLAUDE.md so Claude CLI sessions automatically discover loom.
- **Subtask decomposition**: Workers can call `loom queue add` to create subtasks. Recursion depth is tracked via `LOOM_DEPTH`/`LOOM_MAX_DEPTH` env vars and capped at `--max-depth` (default 3).

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
| `loom dashboard` | Live terminal dashboard (auto-refresh) |
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

## Task Orchestration (Loom)

This project uses [loom](https://github.com/thechewu/loom) for parallel task execution. Loom is a batch tool for parallelizing known work — not a delegation tool for hard problems. Use it when parallelism pays for the orchestration cost.

### When to Use Loom

Use loom when you have **independent tasks** that touch **different files** and have **clear specs**:
- Writing tests for multiple modules
- Applying mechanical changes across files (type hints, docstrings, lint fixes)
- Creating multiple new files from well-defined specs

### When NOT to Use Loom

Do the work directly when:
- The task requires **exploration** (debugging, profiling, understanding unfamiliar code)
- Changes are **interdependent** (feature B depends on how you implement feature A)
- You need to **iterate** (write → test → fix → retry)
- Multiple tasks **modify the same file** (merge conflicts will drop work)

Workers are executors, not thinkers. They follow a spec — they don't explore, investigate, or make design decisions. If the task requires understanding the problem before solving it, do it directly.

### Commands

```bash
loom queue add -t "Short title" -d "Detailed, self-contained description"  # queue a task
loom queue add -t "Title" -d "Description" -p 0                           # high priority (0=highest, 4=lowest)
loom queue list                                                            # list all work items
loom queue show <id>                                                       # show item details + failure reasons
loom status                                                                # overall summary
loom worker list                                                           # worker states
loom logs                                                                  # tail all worker output
loom logs <worker>                                                         # tail specific worker output
```

### Writing Good Task Descriptions

Writing a good description takes effort comparable to doing the task yourself. Loom only saves time when you amortize that cost across 3+ parallel workers.

Worker agents start with **zero context** from this session. The `-d` description is the agent's entire prompt. Before writing descriptions, **read the relevant source files** to understand current signatures, imports, and patterns. Every description must be fully self-contained:

- **Exact file paths**: specify which files to create or modify
- **Complete code context**: include import statements, constructor signatures, and data structures the agent will need
- **Specific test cases**: describe expected behavior with concrete inputs and outputs
- **Constraints**: "Do not change any logic, just add type annotations"
- **One concern per task**: keep tasks scoped to avoid merge conflicts between workers

Bad: "Add a caching layer to the API client"
Good: "Create src/cache.py with a class DiskCache that wraps requests. Constructor takes cache_dir: str and ttl_seconds: int = 3600. Method get(url: str) -> Optional[Response] checks for a cached JSON file at cache_dir/<url_hash>.json, returns None if missing or expired. Method set(url: str, response: Response) writes the JSON. Add tests in tests/test_cache.py covering: cache miss returns None, cache hit returns stored response, expired entry returns None."
