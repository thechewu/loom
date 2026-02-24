// Package prompt contains the canonical CLAUDE.md snippet that teaches
// Claude CLI sessions how to use loom for task orchestration.
package prompt

const ClaudeMDSection = `## Task Orchestration (Loom)

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

` + "```" + `bash
loom queue add -t "Short title" -d "Detailed, self-contained description"  # queue a task
loom queue add -t "Title" -d "Description" -p 0                           # high priority (0=highest, 4=lowest)
loom queue list                                                            # list all work items
loom queue show <id>                                                       # show item details + failure reasons
loom status                                                                # overall summary
loom worker list                                                           # worker states
loom logs                                                                  # tail all worker output
loom logs <worker>                                                         # tail specific worker output
` + "```" + `

### Writing Good Task Descriptions

Writing a good description takes effort comparable to doing the task yourself. Loom only saves time when you amortize that cost across 3+ parallel workers.

Worker agents start with **zero context** from this session. The ` + "`-d`" + ` description is the agent's entire prompt. Before writing descriptions, **read the relevant source files** to understand current signatures, imports, and patterns. Every description must be fully self-contained:

- **Exact file paths**: specify which files to create or modify
- **Complete code context**: include import statements, constructor signatures, and data structures the agent will need
- **Specific test cases**: describe expected behavior with concrete inputs and outputs
- **Constraints**: "Do not change any logic, just add type annotations"
- **One concern per task**: keep tasks scoped to avoid merge conflicts between workers

Bad: "Add a caching layer to the API client"
Good: "Create src/cache.py with a class DiskCache that wraps requests. Constructor takes cache_dir: str and ttl_seconds: int = 3600. Method get(url: str) -> Optional[Response] checks for a cached JSON file at cache_dir/<url_hash>.json, returns None if missing or expired. Method set(url: str, response: Response) writes the JSON. Add tests in tests/test_cache.py covering: cache miss returns None, cache hit returns stored response, expired entry returns None."
`

// Sentinel is the marker string used to detect if the loom section
// has already been added to a CLAUDE.md file.
const Sentinel = "## Task Orchestration (Loom)"
