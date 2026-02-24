package main

import (
	"bytes"
	"fmt"
	"os"
	"os/signal"
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

		// Initial render
		fmt.Print("\033[2J\033[H")
		fmt.Print(renderDashboard(beads))

		ticker := time.NewTicker(refresh)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				fmt.Print("\033[0m") // reset colors
				return nil
			case <-ticker.C:
				fmt.Print("\033[H") // cursor home (no clear — reduces flicker)
				fmt.Print(renderDashboard(beads))
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
	clearToEOL   = "\033[K"
)

func renderDashboard(beads *beadsclient.Client) string {
	var buf bytes.Buffer

	now := time.Now().Format("15:04:05")

	// Header
	header := fmt.Sprintf("%s%s  LOOM DASHBOARD%s  %s%s[%s]%s", colorBold, colorCyan, colorReset, colorDim, colorWhite, beads.Workspace, colorReset)
	refreshed := fmt.Sprintf("%s%s refreshed %s%s", colorDim, colorWhite, now, colorReset)
	fmt.Fprintf(&buf, "  %s          %s%s\n", header, refreshed, clearToEOL)
	fmt.Fprintf(&buf, "  %s────────────────────────────────────────────────────────%s%s\n", colorDim, colorReset, clearToEOL)
	fmt.Fprintf(&buf, "%s\n", clearToEOL)

	// Progress section
	ws, wsErr := beads.GetWorkSummary()
	if wsErr != nil {
		fmt.Fprintf(&buf, "  %sError: %v%s%s\n", colorRed, wsErr, colorReset, clearToEOL)
	} else {
		completed := ws.Done + ws.Failed
		bar := progressBar(completed, ws.Total, 30)
		pct := 0
		if ws.Total > 0 {
			pct = completed * 100 / ws.Total
		}
		fmt.Fprintf(&buf, "  %sProgress:%s %s %d/%d (%d%%)%s\n",
			colorBold, colorReset, bar, completed, ws.Total, pct, clearToEOL)

		fmt.Fprintf(&buf, "  %sPending:%s %d  %sIn Progress:%s %d  %sPending Merge:%s %d  %sDone:%s %d  %sFailed:%s %d%s\n",
			colorYellow, colorReset, ws.Pending,
			colorCyan, colorReset, ws.InProgress,
			colorMagenta, colorReset, ws.PendingMerge,
			colorGreen, colorReset, ws.Done,
			colorRed, colorReset, ws.Failed,
			clearToEOL)
	}
	fmt.Fprintf(&buf, "%s\n", clearToEOL)

	// Workers section
	fmt.Fprintf(&buf, "  %s%sWORKERS%s%s\n", colorBold, colorWhite, colorReset, clearToEOL)

	workers, wkErr := beads.ListWorkers()
	if wkErr != nil {
		fmt.Fprintf(&buf, "  %sError: %v%s%s\n", colorRed, wkErr, colorReset, clearToEOL)
	} else if len(workers) == 0 {
		fmt.Fprintf(&buf, "  %s(no workers)%s%s\n", colorDim, colorReset, clearToEOL)
	} else {
		var wbuf bytes.Buffer
		w := tabwriter.NewWriter(&wbuf, 0, 4, 2, ' ', 0)
		fmt.Fprintf(w, "  %sNAME\tSTATE\tCURRENT ITEM\tLAST SEEN%s\n", colorDim, colorReset)
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
			}
			fmt.Fprintf(w, "  %s\t%s%s%s\t%s\t%s\n",
				name,
				stateColor, fields.AgentState, colorReset,
				defaultStr(fields.CurrentItem, "-"),
				lastSeen)
		}
		w.Flush()
		// Append clearToEOL to each line
		for _, line := range strings.Split(strings.TrimRight(wbuf.String(), "\n"), "\n") {
			fmt.Fprintf(&buf, "%s%s\n", line, clearToEOL)
		}
	}
	fmt.Fprintf(&buf, "%s\n", clearToEOL)

	// Queue section
	fmt.Fprintf(&buf, "  %s%sQUEUE%s%s\n", colorBold, colorWhite, colorReset, clearToEOL)

	items, qErr := beads.ListAllWork()
	if qErr != nil {
		fmt.Fprintf(&buf, "  %sError: %v%s%s\n", colorRed, qErr, colorReset, clearToEOL)
	} else if len(items) == 0 {
		fmt.Fprintf(&buf, "  %s(empty)%s%s\n", colorDim, colorReset, clearToEOL)
	} else {
		// Sort: in_progress first, then pending, pending-merge, then done/failed
		sort.Slice(items, func(i, j int) bool {
			return statusOrder(items[i]) < statusOrder(items[j])
		})

		var qbuf bytes.Buffer
		w := tabwriter.NewWriter(&qbuf, 0, 4, 2, ' ', 0)
		fmt.Fprintf(w, "  %sID\tTITLE\tSTATUS\tPRI\tWORKER%s\n", colorDim, colorReset)
		for _, it := range items {
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
			case "done":
				statusColor = colorGreen
			case "failed":
				statusColor = colorRed
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
			fmt.Fprintf(&buf, "%s%s\n", line, clearToEOL)
		}
	}
	fmt.Fprintf(&buf, "%s\n", clearToEOL)

	// Clear any leftover lines from previous render
	for i := 0; i < 10; i++ {
		fmt.Fprintf(&buf, "%s\n", clearToEOL)
	}

	return buf.String()
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
