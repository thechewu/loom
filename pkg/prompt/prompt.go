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

- **Exact file paths**: specify which files to create or modify
- **Complete code context**: include import statements, constructor signatures, and data structures the agent will need
- **Specific test cases**: describe expected behavior with concrete inputs and outputs
- **Constraints**: "Do not change any logic, just add type annotations"
- **One concern per task**: keep tasks scoped to avoid merge conflicts between workers

Bad: "Add a caching layer to the API client"
Good: "Create src/cache.py with a class DiskCache that wraps requests. Constructor takes cache_dir: str and ttl_seconds: int = 3600. Method get(url: str) -> Optional[Response] checks for a cached JSON file at cache_dir/<url_hash>.json, returns None if missing or expired. Method set(url: str, response: Response) writes the JSON. Add tests in tests/test_cache.py covering: cache miss returns None, cache hit returns stored response, expired entry returns None."

### Avoiding Merge Conflicts

Loom drops conflicting merges to maintain throughput. To minimize lost work:
- Assign tasks to **different files** when possible
- Avoid two tasks that both modify the same function
- For related changes to one file, queue them as a single task
`

// Sentinel is the marker string used to detect if the loom section
// has already been added to a CLAUDE.md file.
const Sentinel = "## Task Orchestration (Loom)"
