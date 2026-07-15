// missionctl — fleet cockpit for Claude Code loops.
//
// Temporary CLI: prints the discovered fleet from Claude Code session logs, to
// prove the observation core on real data. The Bubble Tea cockpit replaces this.
package main

import (
	"fmt"
	"os"
	"time"

	"github.com/jitokim/missionctl/internal/claude"
	"github.com/jitokim/missionctl/internal/domain"
)

func main() {
	now := time.Now()
	loops, err := claude.DiscoverLoops(now)
	if err != nil {
		fmt.Fprintln(os.Stderr, "scan error:", err)
		os.Exit(1)
	}
	if len(loops) == 0 {
		fmt.Println("no Claude Code sessions found under", claude.ProjectsDir())
		return
	}

	stalled := 0
	for _, l := range loops {
		if l.State == domain.StateStalled {
			stalled++
		}
	}
	fmt.Printf("\n◎ missionctl — fleet  (%d loops · %d stalled · idle>%s)\n\n",
		len(loops), stalled, claude.IdleThreshold)
	fmt.Printf("  %-20s %-9s %-14s %s\n", "PROJECT", "STATE", "LAST ACTIVITY", "NOTE")
	fmt.Printf("  %s\n", "────────────────────────────────────────────────────────────────")
	for _, l := range loops {
		fmt.Printf("  %-20s %-9s %-14s %s\n",
			trunc(l.Name, 20), stateLabel(l.State), rel(now.Sub(l.LastActivity)), stallNote(l.Stall))
	}
	fmt.Println()
}

func stateLabel(s domain.LoopState) string {
	switch s {
	case domain.StateStalled:
		return "STALLED"
	case domain.StateRunning:
		return "running"
	default:
		return string(s)
	}
}

func stallNote(k domain.StallKind) string {
	if k == domain.StallNone {
		return ""
	}
	return "⚠ " + string(k)
}

func rel(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
