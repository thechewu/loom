package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
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

	queueCmd.AddCommand(queueAddCmd)
	queueCmd.AddCommand(queueListCmd)

	workerCmd.AddCommand(workerListCmd)

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

		repoPath, err := filepath.Abs(repoPath)
		if err != nil {
			return err
		}

		cfg := supervisor.Config{
			LoomDir:      filepath.Join(repoPath, ".loom"),
			RepoPath:     repoPath,
			AgentCmd:     agentCmd,
			AgentArgs:    agentArgs,
			MaxWorkers:   maxWorkers,
			PollInterval: poll,
			StuckTimeout: stuckTimeout,
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
	Short: "Manually merge all pending worker branches into main",
	RunE: func(cmd *cobra.Command, args []string) error {
		loomDir, err := findLoomDir()
		if err != nil {
			return err
		}
		repoPath := filepath.Dir(loomDir)
		beads := beadsclient.NewClient(repoPath, loomDir)

		m := &merger.Merger{
			Beads:       beads,
			RepoPath:    repoPath,
			WorktreeDir: filepath.Join(loomDir, "worktrees"),
			Logger:      log.Default(),
		}

		merged, dropped, err := m.MergeAll()
		if err != nil {
			return err
		}

		fmt.Printf("merged=%d  dropped=%d\n", merged, dropped)
		return nil
	},
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

	// Don't duplicate if already present
	if strings.Contains(string(existing), prompt.Sentinel) {
		return nil
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()

	// Add a blank line separator if the file already has content
	if len(existing) > 0 && !strings.HasSuffix(string(existing), "\n\n") {
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
