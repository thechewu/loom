package main

import (
	"bytes"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"github.com/thechewu/loom/pkg/beadsclient"
)

var dashboardCmd = &cobra.Command{
	Use:   "dashboard",
	Short: "Live terminal dashboard showing workers, tasks, and progress",
	RunE: func(cmd *cobra.Command, args []string) error {
		refresh, _ := cmd.Flags().GetDuration("refresh")

		beads, err := openBeads()
		if err != nil {
			return err
		}

		ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt)
		defer cancel()

		// Enter alternate screen buffer + hide cursor
		fmt.Print("\033[?1049h\033[?25l\033[2J")

		// Ensure we restore terminal on exit
		defer func() {
			fmt.Print("\033[?25h\033[?1049l") // show cursor + leave alt screen
		}()

		printFrame := func() {
			raw := renderDashboard(beads)
			// Append clear-to-EOL on each line so shorter lines
			// overwrite longer previous ones without artifacts.
			lines := strings.Split(raw, "\n")
			var frame strings.Builder
			for i, line := range lines {
				frame.WriteString(line)
				frame.WriteString("\033[K") // clear to end of line
				if i < len(lines)-1 {
					frame.WriteByte('\n')
				}
			}
			fmt.Print("\033[H")   // cursor home
			fmt.Print(frame.String())
			fmt.Print("\033[J")   // clear from cursor to end of screen
		}

		printFrame()

		ticker := time.NewTicker(refresh)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return nil
			case <-ticker.C:
				printFrame()
			}
		}
	},
}

const (
	colorReset   = "\033[0m"
	colorBold    = "\033[1m"
	colorDim     = "\033[2m"
	colorGreen   = "\033[32m"
	colorYellow  = "\033[33m"
	colorRed     = "\033[31m"
	colorCyan    = "\033[36m"
	colorMagenta = "\033[35m"
	colorWhite   = "\033[37m"
)

func renderDashboard(beads *beadsclient.Client) string {
	var buf bytes.Buffer

	now := time.Now().Format("15:04:05")

	// Header
	header := fmt.Sprintf("%s%s  LOOM DASHBOARD%s  %s%s[%s]%s", colorBold, colorCyan, colorReset, colorDim, colorWhite, beads.Workspace, colorReset)
	refreshed := fmt.Sprintf("%s%s refreshed %s%s", colorDim, colorWhite, now, colorReset)
	fmt.Fprintf(&buf, "  %s          %s\n", header, refreshed)
	fmt.Fprintf(&buf, "  %s────────────────────────────────────────────────────────────────────────────%s\n", colorDim, colorReset)
	fmt.Fprintf(&buf, "\n")

	// Progress section
	ws, wsErr := beads.GetWorkSummary()
	if wsErr != nil {
		fmt.Fprintf(&buf, "  %sError: %v%s\n", colorRed, wsErr, colorReset)
	} else {
		completed := ws.Done + ws.Failed
		bar := progressBar(completed, ws.Total, 30)
		pct := 0
		if ws.Total > 0 {
			pct = completed * 100 / ws.Total
		}
		fmt.Fprintf(&buf, "  %sProgress:%s %s %d/%d (%d%%)\n",
			colorBold, colorReset, bar, completed, ws.Total, pct)

		fmt.Fprintf(&buf, "  %sPending:%s %d  %sIn Progress:%s %d  %sPending Merge:%s %d  %sDone:%s %d  %sFailed:%s %d\n",
			colorYellow, colorReset, ws.Pending,
			colorCyan, colorReset, ws.InProgress,
			colorMagenta, colorReset, ws.PendingMerge,
			colorGreen, colorReset, ws.Done,
			colorRed, colorReset, ws.Failed)
	}
	fmt.Fprintf(&buf, "\n")

	// Workers section
	fmt.Fprintf(&buf, "  %s%sWORKERS%s\n", colorBold, colorWhite, colorReset)

	workers, wkErr := beads.ListWorkers()
	if wkErr != nil {
		fmt.Fprintf(&buf, "  %sError: %v%s\n", colorRed, wkErr, colorReset)
	} else if len(workers) == 0 {
		fmt.Fprintf(&buf, "  %s(no workers)%s\n", colorDim, colorReset)
	} else {
		var wbuf bytes.Buffer
		w := tabwriter.NewWriter(&wbuf, 0, 4, 2, ' ', 0)
		fmt.Fprintf(w, "  %sNAME\tSTATE\tITEM\tLAST SEEN\tOUTPUT%s\n", colorDim, colorReset)
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
			stateColor := colorDim
			switch fields.AgentState {
			case "working":
				stateColor = colorGreen
			case "stuck":
				stateColor = colorYellow
			case "dead":
				stateColor = colorRed
			case "idle":
				stateColor = colorDim
			}

			// Tail last output from the worker's result file
			output := tailWorkerOutput(fields.WorkDir, 100)

			fmt.Fprintf(w, "  %s\t%s%s%s\t%s\t%s\t%s%s%s\n",
				name,
				stateColor, fields.AgentState, colorReset,
				defaultStr(fields.CurrentItem, "-"),
				lastSeen,
				colorDim, output, colorReset)
		}
		w.Flush()
		for _, line := range strings.Split(strings.TrimRight(wbuf.String(), "\n"), "\n") {
			fmt.Fprintf(&buf, "%s\n", line)
		}
	}
	fmt.Fprintf(&buf, "\n")

	// Queue section — only show active items (not done/failed)
	fmt.Fprintf(&buf, "  %s%sQUEUE%s\n", colorBold, colorWhite, colorReset)

	items, qErr := beads.ListAllWork()
	if qErr != nil {
		fmt.Fprintf(&buf, "  %sError: %v%s\n", colorRed, qErr, colorReset)
	} else {
		// Split into active and completed
		var active, completed []beadsclient.Issue
		for _, it := range items {
			s := effectiveStatus(it)
			if s == "done" || s == "failed" {
				completed = append(completed, it)
			} else {
				active = append(active, it)
			}
		}

		if len(active) == 0 && len(completed) == 0 {
			fmt.Fprintf(&buf, "  %s(empty)%s\n", colorDim, colorReset)
		} else {
			// Sort active: in_progress first, then pending, pending-merge
			sort.Slice(active, func(i, j int) bool {
				return statusOrder(active[i]) < statusOrder(active[j])
			})

			var qbuf bytes.Buffer
			w := tabwriter.NewWriter(&qbuf, 0, 4, 2, ' ', 0)
			fmt.Fprintf(w, "  %sID\tTITLE\tSTATUS\tPRI\tWORKER%s\n", colorDim, colorReset)
			for _, it := range active {
				status := effectiveStatus(it)
				assignee := defaultStr(it.Assignee, "-")
				statusColor := colorDim
				switch status {
				case "in_progress":
					statusColor = colorCyan
				case "pending":
					statusColor = colorYellow
				case "pending-merge":
					statusColor = colorMagenta
				}
				fmt.Fprintf(w, "  %s\t%s\t%s%s%s\t%d\t%s\n",
					it.ID,
					truncate(it.Title, 40),
					statusColor, status, colorReset,
					it.Priority,
					assignee)
			}
			w.Flush()
			for _, line := range strings.Split(strings.TrimRight(qbuf.String(), "\n"), "\n") {
				fmt.Fprintf(&buf, "%s\n", line)
			}

			// Show completed count as a summary line
			if len(completed) > 0 {
				doneCount := 0
				failedCount := 0
				for _, it := range completed {
					if effectiveStatus(it) == "failed" {
						failedCount++
					} else {
						doneCount++
					}
				}
				fmt.Fprintf(&buf, "  %s+ %d done, %d failed (hidden)%s\n", colorDim, doneCount, failedCount, colorReset)
			}
		}
	}
	fmt.Fprintf(&buf, "\n")

	return buf.String()
}

// tailWorkerOutput reads the last maxLen bytes from a worker's result file.
func tailWorkerOutput(workDir string, maxLen int) string {
	if workDir == "" {
		return "-"
	}
	resultPath := filepath.Join(workDir, ".loom-agent", "result.txt")
	f, err := os.Open(resultPath)
	if err != nil {
		return "-"
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil || info.Size() == 0 {
		return "-"
	}

	// Read last maxLen bytes
	offset := info.Size() - int64(maxLen)
	if offset < 0 {
		offset = 0
	}
	buf := make([]byte, int64(maxLen))
	n, err := f.ReadAt(buf, offset)
	if n == 0 {
		return "-"
	}

	s := string(buf[:n])
	// Clean up: collapse whitespace, remove control chars
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		if r < 32 {
			return -1 // drop control chars
		}
		return r
	}, s)
	s = collapseSpaces(s)
	s = strings.TrimSpace(s)

	if len(s) > maxLen {
		s = s[len(s)-maxLen:]
	}
	if s == "" {
		return "-"
	}
	return s
}

func collapseSpaces(s string) string {
	var b strings.Builder
	prevSpace := false
	for _, r := range s {
		if r == ' ' {
			if !prevSpace {
				b.WriteRune(r)
			}
			prevSpace = true
		} else {
			b.WriteRune(r)
			prevSpace = false
		}
	}
	return b.String()
}

func progressBar(done, total, width int) string {
	if total == 0 {
		return colorDim + strings.Repeat("░", width) + colorReset
	}
	filled := done * width / total
	if filled > width {
		filled = width
	}
	return colorGreen + strings.Repeat("█", filled) + colorDim + strings.Repeat("░", width-filled) + colorReset
}

func statusOrder(it beadsclient.Issue) int {
	switch effectiveStatus(it) {
	case "in_progress":
		return 0
	case "pending":
		return 1
	case "pending-merge":
		return 2
	case "done":
		return 3
	case "failed":
		return 4
	default:
		return 5
	}
}
