package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"github.com/thechewu/loom/pkg/beadsclient"
	"github.com/thechewu/loom/pkg/merger"
	"github.com/thechewu/loom/pkg/prompt"
	"github.com/thechewu/loom/pkg/supervisor"
)

func main() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

var rootCmd = &cobra.Command{
	Use:   "loom",
	Short: "Lightweight multi-agent orchestration with beads work tracking",
}

func init() {
	rootCmd.AddCommand(initCmd)
	rootCmd.AddCommand(queueCmd)
	rootCmd.AddCommand(workerCmd)
	rootCmd.AddCommand(runCmd)
	rootCmd.AddCommand(statusCmd)
	rootCmd.AddCommand(doltCmd)
	rootCmd.AddCommand(mergeCmd)
	rootCmd.AddCommand(promptCmd)
	rootCmd.AddCommand(logsCmd)
	rootCmd.AddCommand(dashboardCmd)
	rootCmd.AddCommand(pruneCmd)

	queueCmd.AddCommand(queueAddCmd)
	queueCmd.AddCommand(queueListCmd)
	queueCmd.AddCommand(queueShowCmd)
	queueCmd.AddCommand(queueClearCmd)

	workerCmd.AddCommand(workerListCmd)
	workerCmd.AddCommand(workerClearCmd)

	doltCmd.AddCommand(doltStartCmd)
	doltCmd.AddCommand(doltStopCmd)
	doltCmd.AddCommand(doltStatusCmd)

	// Flags
	queueAddCmd.Flags().StringP("title", "t", "", "work item title")
	queueAddCmd.Flags().StringP("desc", "d", "", "work item description")
	queueAddCmd.Flags().IntP("priority", "p", 2, "priority (0=highest, 4=lowest)")

	runCmd.Flags().IntP("workers", "w", 4, "max concurrent workers")
	runCmd.Flags().String("agent", "claude", "agent command to run")
	runCmd.Flags().StringSlice("agent-args", nil, "additional agent arguments")
	runCmd.Flags().Duration("poll", 5*time.Second, "poll interval")
	runCmd.Flags().Duration("stuck-timeout", 10*time.Minute, "mark worker stuck after this duration")
	runCmd.Flags().String("repo", ".", "git repository path")
	runCmd.Flags().Int("max-depth", 3, "max recursion depth for subtask decomposition")
	runCmd.Flags().String("merge-mode", "", "merge behavior: main (default), loom, none")
	runCmd.Flags().String("merge-branch", "", "target branch for loom merge mode (default: loom)")

	mergeCmd.Flags().String("branch", "", "target branch to merge into (default: main)")

	dashboardCmd.Flags().DurationP("refresh", "r", 2*time.Second, "refresh interval")

	queueShowCmd.Flags().IntP("last", "n", 0, "show last N items with outcomes")

	queueClearCmd.Flags().String("status", "", "only clear items with this status (pending, in_progress, done, failed, pending-merge)")
}

// --- init ---

var initCmd = &cobra.Command{
	Use:   "init [path]",
	Short: "Initialize a loom workspace (Dolt + beads)",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		dir := "."
		if len(args) > 0 {
			dir = args[0]
		}
		dir, _ = filepath.Abs(dir)

		loomDir := filepath.Join(dir, ".loom")
		if err := os.MkdirAll(loomDir, 0o755); err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Join(loomDir, "worktrees"), 0o755); err != nil {
			return err
		}

		// Initialize Dolt
		dolt := beadsclient.NewDoltServer(loomDir)
		fmt.Println("initializing Dolt database...")
		if err := dolt.Init(); err != nil {
			return fmt.Errorf("dolt init: %w", err)
		}

		// Start Dolt server
		fmt.Println("starting Dolt server...")
		if err := dolt.Start(); err != nil {
			return fmt.Errorf("dolt start: %w", err)
		}
		fmt.Printf("Dolt server: %s\n", dolt.Status())

		// Initialize beads
		fmt.Println("initializing beads database...")
		beads := beadsclient.NewClient(dir, loomDir)
		if err := beads.Init(); err != nil {
			return fmt.Errorf("beads init: %w", err)
		}

		// Append loom section to CLAUDE.md
		claudePath := filepath.Join(dir, "CLAUDE.md")
		if err := injectClaudeMD(claudePath); err != nil {
			fmt.Printf("warning: could not update CLAUDE.md: %v\n", err)
		} else {
			fmt.Println("updated CLAUDE.md with loom instructions")
		}

		fmt.Printf("loom workspace initialized at %s\n", loomDir)
		return nil
	},
}

// --- queue ---

var queueCmd = &cobra.Command{
	Use:   "queue",
	Short: "Manage the work queue",
}

var queueAddCmd = &cobra.Command{
	Use:   "add [bead-id]",
	Short: "Add a work item to the queue",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		title, _ := cmd.Flags().GetString("title")
		desc, _ := cmd.Flags().GetString("desc")
		priority, _ := cmd.Flags().GetInt("priority")

		if title == "" && len(args) > 0 {
			title = args[0]
		}
		if title == "" {
			return fmt.Errorf("title is required (use -t or pass as argument)")
		}

		// Recursion guard: if we're inside a worker, check depth limit
		if depthStr := os.Getenv("LOOM_DEPTH"); depthStr != "" {
			var depth, maxDepth int
			fmt.Sscanf(depthStr, "%d", &depth)
			if maxStr := os.Getenv("LOOM_MAX_DEPTH"); maxStr != "" {
				fmt.Sscanf(maxStr, "%d", &maxDepth)
			}
			if maxDepth > 0 && depth >= maxDepth {
				return fmt.Errorf("recursion depth limit reached (%d/%d) — cannot create subtasks at this depth", depth, maxDepth)
			}
		}

		beads, err := openBeads()
		if err != nil {
			return err
		}

		id, err := beads.CreateWorkItem(title, desc, priority)
		if err != nil {
			return err
		}

		fmt.Printf("created work item %s (priority %d)\n", id, priority)
		return nil
	},
}

var queueListCmd = &cobra.Command{
	Use:   "list",
	Short: "List work items",
	RunE: func(cmd *cobra.Command, args []string) error {
		beads, err := openBeads()
		if err != nil {
			return err
		}

		items, err := beads.ListAllWork()
		if err != nil {
			return err
		}

		if len(items) == 0 {
			fmt.Println("queue is empty")
			return nil
		}

		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "BEAD ID\tTITLE\tSTATUS\tPRI\tASSIGNED")
		for _, it := range items {
			assignee := it.Assignee
			if assignee == "" {
				assignee = "-"
			}
			status := effectiveStatus(it)
			fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\n",
				it.ID, truncate(it.Title, 40), status,
				it.Priority, assignee)
		}
		w.Flush()
		return nil
	},
}

var queueShowCmd = &cobra.Command{
	Use:   "show [bead-id]",
	Short: "Show work item details, or last N items with -n",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		n, _ := cmd.Flags().GetInt("last")

		if len(args) == 0 && n == 0 {
			return fmt.Errorf("provide a bead ID or use -n to show last N items")
		}

		beads, err := openBeads()
		if err != nil {
			return err
		}

		// -n mode: show last N items as a summary table
		if n > 0 {
			return showLastN(beads, n)
		}

		// Single-item detail mode
		return showOne(beads, args[0])
	},
}

func showOne(beads *beadsclient.Client, id string) error {
	issue, err := beads.GetIssue(id)
	if err != nil {
		return err
	}

	// Fetch comments early so resolveOutcome can use them
	comments, _ := beads.GetComments(id)

	fmt.Printf("ID:       %s\n", issue.ID)
	fmt.Printf("Title:    %s\n", issue.Title)
	fmt.Printf("Status:   %s\n", effectiveStatus(*issue))
	fmt.Printf("Outcome:  %s\n", resolveOutcome(*issue, comments))
	fmt.Printf("Priority: %d\n", issue.Priority)
	fmt.Printf("Assignee: %s\n", defaultStr(issue.Assignee, "-"))
	fmt.Printf("Created:  %s\n", issue.CreatedAt)
	fmt.Printf("Updated:  %s\n", issue.UpdatedAt)
	if issue.Description != "" {
		fmt.Printf("\n--- Description ---\n%s\n", issue.Description)
	}

	// Show comments (contains failure reasons, requeue history, audit trail)
	if len(comments) > 0 {
		fmt.Printf("\n--- History ---\n")
		for _, c := range comments {
			fmt.Println(c)
		}
	}

	// Show agent output (close_reason) if present
	if issue.CloseReason != "" {
		fmt.Printf("\n--- Result ---\n%s\n", issue.CloseReason)
	}
	return nil
}

func showLastN(beads *beadsclient.Client, n int) error {
	items, err := beads.ListAllWork()
	if err != nil {
		return err
	}
	if len(items) == 0 {
		fmt.Println("queue is empty")
		return nil
	}

	// Sort by created_at descending (most recent first)
	sort.Slice(items, func(i, j int) bool {
		return items[i].CreatedAt > items[j].CreatedAt
	})

	if n > len(items) {
		n = len(items)
	}
	items = items[:n]

	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "BEAD ID\tTITLE\tOUTCOME")
	for _, it := range items {
		comments, _ := beads.GetComments(it.ID)
		outcome := resolveOutcome(it, comments)
		fmt.Fprintf(w, "%s\t%s\t%s\n", it.ID, truncate(it.Title, 40), outcome)
	}
	w.Flush()
	return nil
}

var queueClearCmd = &cobra.Command{
	Use:   "clear",
	Short: "Close work items (all active, or filtered by --status)",
	RunE: func(cmd *cobra.Command, args []string) error {
		filter, _ := cmd.Flags().GetString("status")

		beads, err := openBeads()
		if err != nil {
			return err
		}

		items, err := beads.ListAllWork()
		if err != nil {
			return err
		}

		closed := 0
		for _, it := range items {
			status := effectiveStatus(it)
			if filter != "" {
				// Only clear items matching the filter
				if status != filter {
					continue
				}
			} else {
				// Default: skip already-terminal items
				if status == "done" || status == "failed" {
					continue
				}
			}
			if err := beads.CompleteWork(it.ID, "cleared"); err != nil {
				fmt.Printf("warning: could not close %s: %v\n", it.ID, err)
				continue
			}
			closed++
		}

		fmt.Printf("closed %d item(s)\n", closed)
		return nil
	},
}

// --- worker ---

var workerCmd = &cobra.Command{
	Use:   "worker",
	Short: "Manage workers",
}

var workerListCmd = &cobra.Command{
	Use:   "list",
	Short: "List workers",
	RunE: func(cmd *cobra.Command, args []string) error {
		beads, err := openBeads()
		if err != nil {
			return err
		}

		workers, err := beads.ListWorkers()
		if err != nil {
			return err
		}

		if len(workers) == 0 {
			fmt.Println("no workers")
			return nil
		}

		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "NAME\tSTATE\tCURRENT ITEM\tLAST HEARTBEAT")
		for _, wk := range workers {
			fields := beadsclient.ParseWorkerDescription(wk.Description)
			name := wk.Title
			if len(name) > 8 && name[:8] == "Worker: " {
				name = name[8:]
			}
			lastSeen := "-"
			if fields.LastHeartbeat != "" {
				if t, err := time.Parse(time.RFC3339, fields.LastHeartbeat); err == nil {
					lastSeen = time.Since(t).Truncate(time.Second).String() + " ago"
				}
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n",
				name, fields.AgentState,
				defaultStr(fields.CurrentItem, "-"), lastSeen)
		}
		w.Flush()
		return nil
	},
}

var workerClearCmd = &cobra.Command{
	Use:   "clear",
	Short: "Close all worker beads (clears stale workers)",
	RunE: func(cmd *cobra.Command, args []string) error {
		beads, err := openBeads()
		if err != nil {
			return err
		}

		workers, err := beads.ListWorkers()
		if err != nil {
			return err
		}

		if len(workers) == 0 {
			fmt.Println("no workers to clear")
			return nil
		}

		closed := 0
		for _, wk := range workers {
			name := wk.Title
			if len(name) > 8 && name[:8] == "Worker: " {
				name = name[8:]
			}
			if err := beads.CloseWorkerBead(name); err != nil {
				fmt.Fprintf(os.Stderr, "warning: failed to close %s: %v\n", name, err)
			} else {
				closed++
			}
		}
		fmt.Printf("closed %d worker(s)\n", closed)
		return nil
	},
}

// --- run ---

var runCmd = &cobra.Command{
	Use:   "run",
	Short: "Start the supervisor (foreground)",
	RunE: func(cmd *cobra.Command, args []string) error {
		maxWorkers, _ := cmd.Flags().GetInt("workers")
		agentCmd, _ := cmd.Flags().GetString("agent")
		agentArgs, _ := cmd.Flags().GetStringSlice("agent-args")
		poll, _ := cmd.Flags().GetDuration("poll")
		stuckTimeout, _ := cmd.Flags().GetDuration("stuck-timeout")
		repoPath, _ := cmd.Flags().GetString("repo")
		maxDepth, _ := cmd.Flags().GetInt("max-depth")

		repoPath, err := filepath.Abs(repoPath)
		if err != nil {
			return err
		}

		// Load config file, then apply CLI flag overrides
		loomDir := filepath.Join(repoPath, ".loom")
		fileCfg := loadConfigFile(loomDir)

		mergeMode := fileCfg.MergeMode
		if cmd.Flags().Changed("merge-mode") {
			mergeMode, _ = cmd.Flags().GetString("merge-mode")
		}

		mergeBranch := fileCfg.MergeBranch
		if cmd.Flags().Changed("merge-branch") {
			mergeBranch, _ = cmd.Flags().GetString("merge-branch")
		}

		cfg := supervisor.Config{
			LoomDir:      loomDir,
			RepoPath:     repoPath,
			AgentCmd:     agentCmd,
			AgentArgs:    agentArgs,
			MaxWorkers:   maxWorkers,
			PollInterval: poll,
			StuckTimeout: stuckTimeout,
			MaxDepth:     maxDepth,
			MergeMode:    mergeMode,
			MergeBranch:  mergeBranch,
		}

		sv, err := supervisor.New(cfg)
		if err != nil {
			return err
		}

		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
		defer cancel()

		return sv.Run(ctx)
	},
}

// --- status ---

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show overall status",
	RunE: func(cmd *cobra.Command, args []string) error {
		beads, err := openBeads()
		if err != nil {
			return err
		}

		// Dolt status
		loomDir, _ := findLoomDir()
		dolt := beadsclient.NewDoltServer(loomDir)
		fmt.Printf("Dolt: %s\n", dolt.Status())

		// Work summary
		ws, err := beads.GetWorkSummary()
		if err != nil {
			fmt.Printf("Queue: error (%v)\n", err)
		} else {
			fmt.Printf("Queue: pending=%d  in_progress=%d  pending_merge=%d  done=%d  failed=%d  total=%d\n",
				ws.Pending, ws.InProgress, ws.PendingMerge, ws.Done, ws.Failed, ws.Total)
		}

		// Worker summary
		wks, err := beads.GetWorkerSummary()
		if err != nil {
			fmt.Printf("Workers: error (%v)\n", err)
		} else {
			fmt.Printf("Workers: idle=%d  working=%d  stuck=%d  dead=%d  total=%d\n",
				wks.Idle, wks.Working, wks.Stuck, wks.Dead, wks.Total)
		}
		return nil
	},
}

// --- dolt ---

var doltCmd = &cobra.Command{
	Use:   "dolt",
	Short: "Manage the Dolt server",
}

var doltStartCmd = &cobra.Command{
	Use:   "start",
	Short: "Start the Dolt server",
	RunE: func(cmd *cobra.Command, args []string) error {
		loomDir, err := findLoomDir()
		if err != nil {
			return err
		}
		dolt := beadsclient.NewDoltServer(loomDir)
		if err := dolt.Start(); err != nil {
			return err
		}
		fmt.Println(dolt.Status())
		return nil
	},
}

var doltStopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop the Dolt server",
	RunE: func(cmd *cobra.Command, args []string) error {
		loomDir, err := findLoomDir()
		if err != nil {
			return err
		}
		dolt := beadsclient.NewDoltServer(loomDir)
		if err := dolt.Stop(); err != nil {
			return err
		}
		fmt.Println("stopped")
		return nil
	},
}

var doltStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show Dolt server status",
	RunE: func(cmd *cobra.Command, args []string) error {
		loomDir, err := findLoomDir()
		if err != nil {
			return err
		}
		dolt := beadsclient.NewDoltServer(loomDir)
		fmt.Println(dolt.Status())
		return nil
	},
}

// --- merge ---

var mergeCmd = &cobra.Command{
	Use:   "merge",
	Short: "Manually merge all pending worker branches",
	RunE: func(cmd *cobra.Command, args []string) error {
		loomDir, err := findLoomDir()
		if err != nil {
			return err
		}
		repoPath := filepath.Dir(loomDir)
		beads := beadsclient.NewClient(repoPath, loomDir)

		// Resolve target branch: CLI flag > config file > "main"
		fileCfg := loadConfigFile(loomDir)
		targetBranch := "main"
		if fileCfg.MergeMode == "loom" {
			targetBranch = fileCfg.MergeBranch
			if targetBranch == "" {
				targetBranch = "loom"
			}
		}
		if cmd.Flags().Changed("branch") {
			targetBranch, _ = cmd.Flags().GetString("branch")
		}

		m := &merger.Merger{
			Beads:        beads,
			RepoPath:     repoPath,
			WorktreeDir:  filepath.Join(loomDir, "worktrees"),
			TargetBranch: targetBranch,
			Logger:       log.Default(),
		}

		merged, dropped, err := m.MergeAll()
		if err != nil {
			return err
		}

		fmt.Printf("merged=%d  dropped=%d\n", merged, dropped)
		return nil
	},
}

// --- prune ---

var pruneCmd = &cobra.Command{
	Use:   "prune",
	Short: "Delete orphaned loom/* branches with no active bead",
	RunE: func(cmd *cobra.Command, args []string) error {
		loomDir, err := findLoomDir()
		if err != nil {
			return err
		}
		repoPath := filepath.Dir(loomDir)
		beads := beadsclient.NewClient(repoPath, loomDir)

		pruned := supervisor.PruneOrphanedBranches(beads, repoPath, log.Default())
		fmt.Printf("pruned %d branch(es)\n", pruned)
		return nil
	},
}

// --- logs ---

var logsCmd = &cobra.Command{
	Use:   "logs [worker-name]",
	Short: "Tail worker output (all workers or a specific one)",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		loomDir, err := findLoomDir()
		if err != nil {
			return err
		}
		worktreeDir := filepath.Join(loomDir, "worktrees")

		if len(args) > 0 {
			// Tail a specific worker
			resultPath := filepath.Join(worktreeDir, args[0], ".loom", "result.txt")
			if _, err := os.Stat(resultPath); os.IsNotExist(err) {
				fmt.Printf("%s: worker completed (worktree cleaned up)\n", args[0])
				fmt.Printf("use 'loom queue show <bead-id>' to see results\n")
				return nil
			}
			return tailWorkerLog(resultPath)
		}

		// Tail all workers
		entries, err := os.ReadDir(worktreeDir)
		if err != nil {
			return fmt.Errorf("read worktrees dir: %w", err)
		}

		if len(entries) == 0 {
			fmt.Println("no active workers")
			return nil
		}

		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			name := entry.Name()
			resultPath := filepath.Join(worktreeDir, name, ".loom", "result.txt")
			info, err := os.Stat(resultPath)
			if err != nil {
				fmt.Printf("=== %s === (no output)\n\n", name)
				continue
			}
			fmt.Printf("=== %s === (%s, %d bytes)\n", name, info.ModTime().Format("15:04:05"), info.Size())
			printTail(resultPath, 20)
			fmt.Println()
		}
		return nil
	},
}

func tailWorkerLog(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open log: %w", err)
	}
	defer f.Close()

	// Print existing content
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fmt.Println(scanner.Text())
	}

	// Follow new output
	fmt.Println("--- following (ctrl-c to stop) ---")
	for {
		line, err := f.Read(make([]byte, 0))
		_ = line
		if err == io.EOF {
			time.Sleep(500 * time.Millisecond)
			continue
		}
		buf := make([]byte, 4096)
		n, err := f.Read(buf)
		if n > 0 {
			os.Stdout.Write(buf[:n])
		}
		if err != nil && err != io.EOF {
			return err
		}
	}
}

func printTail(path string, lines int) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	allLines := strings.Split(string(data), "\n")
	start := len(allLines) - lines
	if start < 0 {
		start = 0
	}
	for _, line := range allLines[start:] {
		fmt.Println(line)
	}
}

// --- prompt ---

var promptCmd = &cobra.Command{
	Use:   "prompt",
	Short: "Print the CLAUDE.md snippet for teaching Claude about loom",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Print(prompt.ClaudeMDSection)
	},
}

// --- helpers ---

func injectClaudeMD(path string) error {
	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}

	content := string(existing)

	// If already present, replace the existing section with the latest version
	if idx := strings.Index(content, prompt.Sentinel); idx != -1 {
		before := content[:idx]
		// The section runs to the end of the file (it's always appended last)
		updated := strings.TrimRight(before, "\n") + "\n\n" + prompt.ClaudeMDSection
		return os.WriteFile(path, []byte(updated), 0o644)
	}

	// First time — append
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()

	// Add a blank line separator if the file already has content
	if len(existing) > 0 && !strings.HasSuffix(content, "\n\n") {
		f.WriteString("\n")
	}

	_, err = f.WriteString(prompt.ClaudeMDSection)
	return err
}

func openBeads() (*beadsclient.Client, error) {
	loomDir, err := findLoomDir()
	if err != nil {
		return nil, err
	}
	// beadsDir is the parent of .loom (the repo root)
	beadsDir := filepath.Dir(loomDir)
	return beadsclient.NewClient(beadsDir, loomDir), nil
}

func findLoomDir() (string, error) {
	// Workers propagate LOOM_DIR so agents can always find the workspace
	if envDir := os.Getenv("LOOM_DIR"); envDir != "" {
		if info, err := os.Stat(envDir); err == nil && info.IsDir() {
			return envDir, nil
		}
	}
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		loomDir := filepath.Join(dir, ".loom")
		if info, err := os.Stat(loomDir); err == nil && info.IsDir() {
			return loomDir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no .loom workspace found (run 'loom init' first)")
		}
		dir = parent
	}
}

func effectiveStatus(issue beadsclient.Issue) string {
	if issue.HasLabel(beadsclient.LabelDone) {
		return "done"
	}
	if issue.HasLabel(beadsclient.LabelFailed) {
		return "failed"
	}
	if issue.HasLabel(beadsclient.LabelPendingMerge) {
		return "pending-merge"
	}
	if issue.HasLabel(beadsclient.LabelPending) {
		return "pending"
	}
	return issue.Status
}

// resolveOutcome synthesizes a human-readable outcome from the issue's labels,
// comments, and close reason. This answers "what happened?" at a glance.
func resolveOutcome(issue beadsclient.Issue, comments []string) string {
	commentText := strings.Join(comments, "\n")

	if issue.HasLabel(beadsclient.LabelFailed) {
		for _, c := range comments {
			if strings.Contains(c, "merge conflict") {
				return "merge dropped — " + extractAfter(c, "Failed: ")
			}
			if strings.Contains(c, "branch") && strings.Contains(c, "does not exist") {
				return "failed (branch missing)"
			}
			if strings.Contains(c, "exceeded") && strings.Contains(c, "retries") {
				return "failed (retries exhausted)"
			}
			if strings.HasPrefix(c, "Failed: ") {
				return "failed — " + strings.TrimPrefix(c, "Failed: ")
			}
		}
		return "failed"
	}

	if issue.HasLabel(beadsclient.LabelPendingMerge) {
		return "pending merge"
	}

	if issue.HasLabel(beadsclient.LabelDone) {
		// Check for merge audit trail (new merger)
		for _, c := range comments {
			if strings.HasPrefix(c, "Merged:") {
				return strings.TrimSpace(c)
			}
		}
		// Check for no-changes completion
		if strings.Contains(commentText, "no file changes") {
			if issue.CloseReason != "" && containsSubtaskSignal(issue.CloseReason) {
				return "completed (subtasks created — see result)"
			}
			if issue.CloseReason != "" {
				return "completed (no changes needed — see result)"
			}
			return "completed (no changes needed)"
		}
		// Legacy path: merged via close_reason "merged"
		if issue.CloseReason == "merged" {
			return "merged"
		}
		// Has close_reason but no structured comment (legacy items)
		if issue.CloseReason != "" {
			return "completed — see result"
		}
		return "completed"
	}

	if issue.HasLabel(beadsclient.LabelPending) {
		return "queued"
	}

	if issue.Status == "in_progress" {
		return "in progress"
	}

	return issue.Status
}

func extractAfter(s, prefix string) string {
	if i := strings.Index(s, prefix); i >= 0 {
		return s[i+len(prefix):]
	}
	return s
}

// containsSubtaskSignal checks if the agent's output mentions creating subtasks.
func containsSubtaskSignal(s string) bool {
	lower := strings.ToLower(s)
	return strings.Contains(lower, "loom queue add") ||
		strings.Contains(lower, "subtask") ||
		strings.Contains(lower, "sub-task")
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-3] + "..."
}

func defaultStr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// --- config file ---

// loomConfig holds persistent settings from .loom/config.json.
type loomConfig struct {
	MergeMode   string `json:"merge_mode"`
	MergeBranch string `json:"merge_branch"`
}

// loadConfigFile reads .loom/config.json. Returns zero-value if missing or invalid.
func loadConfigFile(loomDir string) loomConfig {
	var cfg loomConfig
	data, err := os.ReadFile(filepath.Join(loomDir, "config.json"))
	if err != nil {
		return cfg
	}
	json.Unmarshal(data, &cfg)
	return cfg
}
