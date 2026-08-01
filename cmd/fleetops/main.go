// fleetops — fleet cockpit for Claude Code loops.
//
// No args: launch the Bubble Tea TUI (the fleet cockpit).
// `--demo`: launch the same TUI seeded with a fixed synthetic fleet instead
// of scanning ~/.claude/projects — no real data read, nothing written to
// ~/.fleetops (see internal/tui.NewDemo).
// `--rename-tabs`: opt-in (default off) — auto-rename the cmux tab of each
// unambiguously-mapped loop to reflect its state/delegation (see
// internal/tui tab-title sync; cmux-only, a terminal write side-effect).
// Subcommands: `hook notify|session-start|session-end` (Claude Code hook
// entry points, see hook.go), `hooks install|uninstall` (register/remove
// those hooks in ~/.claude/settings.json, see hooks.go), and
// `report --since 24h` (a plain-text summary of the append-only event
// history, internal/events — see report.go).
package main

import (
	"fmt"
	"os"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/jitokim/fleetops/internal/control"
	"github.com/jitokim/fleetops/internal/tui"
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "hook":
			runHookCmd(os.Args[2:])
			return
		case "hooks":
			runHooksCmd(os.Args[2:])
			return
		case "accounts":
			runAccountsCmd(os.Args[2:])
			return
		case "report":
			runReportCmd(os.Args[2:])
			return
		case "help", "--help", "-h":
			fmt.Print(helpText())
			return
		default:
			// Not a subcommand — parse the remaining args as TUI flags
			// (--demo / --rename-tabs). An unknown token still errors (naming the
			// OFFENDING token, not just os.Args[1]), so `fleetops bogus` keeps its
			// exit-1 behavior.
			f, bad, ok := parseTUIFlags(os.Args[1:])
			if !ok {
				fmt.Fprintf(os.Stderr, "fleetops: unknown command %q\n", bad)
				os.Exit(1)
			}
			runTUI(f.demo, f.renameTabs)
			return
		}
	}
	runTUI(false, false)
}

// tuiFlags are the flags that modify the TUI launch (no subcommand).
type tuiFlags struct {
	demo       bool
	renameTabs bool
}

// parseTUIFlags parses the TUI-launch flags out of args (everything after the
// program name). Order-independent; on the first unrecognized token it returns
// ok=false and that token (unknown), so the caller reports the OFFENDING token
// rather than a misattributed os.Args[1]. Pure, so the routing is unit-testable
// without starting a real Bubble Tea program.
func parseTUIFlags(args []string) (f tuiFlags, unknown string, ok bool) {
	for _, a := range args {
		switch a {
		case "--demo":
			f.demo = true
		case "--rename-tabs":
			f.renameTabs = true
		default:
			return tuiFlags{}, a, false
		}
	}
	return f, "", true
}

// helpText is `fleetops --help`/`-h`/`help`'s full output: a one-line
// description, every subcommand, and the TUI's keymap.
//
// fix/exit-gate-ux (UX judge item 6, "cheap credibility fix"): `--help`
// used to fall straight through to the unknown-command branch and exit 1
// — anyone's very first reflex with an unfamiliar CLI reads that as a
// broken/abandoned tool, regardless of how solid the rest of it is.
func helpText() string {
	return `fleetops — fleet cockpit for Claude Code loops: observes running Claude
Code sessions and lets you approve/resume/inject/kill them from one TUI.

Usage:
  fleetops                     launch the fleet cockpit (TUI)
  fleetops --demo              launch the TUI with a synthetic fleet — no real data, no disk writes
  fleetops --rename-tabs       (with the TUI) mirror each loop's state + goal onto its cmux tab title (opt-in, cmux only)
  fleetops report [--since D]  plain-text summary of the event history (default 24h)
  fleetops hooks install       register fleetops's Claude Code hooks (gate/idle detection)
  fleetops hooks uninstall     remove them
  fleetops hooks status        report whether the hooks are installed and healthy
  fleetops accounts            list the Claude accounts, their login + hook state, and bindings
  fleetops accounts add <alias> [--dir <path>] [--no-login]
                                  register an account and (by default) log it in
  fleetops accounts login <alias>    (re)run the browser login for an account
  fleetops accounts bind <alias> <path>   bind a directory/repo to an account
  fleetops accounts unbind <path>         remove a directory binding
  fleetops accounts remove <alias> [--force]   un-name an account (never logs it out)
  fleetops hook <event>        Claude Code hook entry point (notify|session-start|session-end)
                                  — invoked BY Claude Code itself, not typically run by hand
  fleetops help | --help | -h  show this help

TUI keymap:
  ↑/↓/g/G     move selection
  /           filter the fleet list
  ↵           attach to the selected loop's terminal
  a           approve a GATE
  r           resume a STALLED loop / re-drive a DRIFT loop with a hint
  i           inject an arbitrary prompt
  p           stop (interrupt) a running/gated loop
  k           kill (press twice within 3s to confirm)
  n           spawn a new loop (contract wizard)
  o           view the selected loop's raw log (pager)
  d           hide the selected loop (persists across restart)
  x           hide + remove registry entry (press twice within 3s to confirm)
  q           quit
`
}

// newModel picks tui.NewDemo() (a fixed synthetic fleet — no
// ~/.claude/projects scan, no ~/.fleetops writes) or tui.New() (the
// normal cockpit) — pulled out of runTUI so the --demo routing decision is
// directly testable without starting a real Bubble Tea program (Run()
// takes over the terminal and blocks on input, unsafe to invoke in a
// test).
func newModel(demo, renameTabs bool) tea.Model {
	if demo {
		// --demo ignores renameTabs: demo never scans real loops, so there is
		// nothing to mirror onto a tab (and demo must touch nothing real).
		return tui.NewDemo()
	}
	return tui.New().WithRenameTabs(renameTabs)
}

func runTUI(demo, renameTabs bool) {
	if demo {
		// --demo ignores ~/.fleetops/settings.json entirely and always spawns
		// with the built-in ["claude"]. Demo mode's contract is "nothing real",
		// which has to include the person's own configuration: a demo that
		// picked up their spawn.command would behave differently on every
		// machine and would leak their local setup into any screenshot or
		// recording. The TUI already refuses every spawning key in demo mode
		// (isDemoBlockedKey), so this is defence in depth — it makes the
		// guarantee a property of the mechanism rather than of the current
		// keymap.
		control.UseDefaultSpawnCommand()
	}
	p := tea.NewProgram(newModel(demo, renameTabs), tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "fleetops:", err)
		os.Exit(1)
	}
}
