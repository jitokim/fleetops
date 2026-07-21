package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"time"

	"github.com/jitokim/fleetops/internal/accountstatus"
	"github.com/jitokim/fleetops/internal/gate"
	"github.com/jitokim/fleetops/internal/sessions"
)

// runHookCmd dispatches `fleetops hook <sub>`. Unknown subcommands are
// silently ignored (exit 0) — see notifyHook for why the whole hook path is
// non-fatal.
func runHookCmd(args []string) {
	if len(args) == 0 {
		return
	}
	switch args[0] {
	case "notify":
		notifyHook()
	case "permission":
		permissionHook()
	case "session-start":
		sessionStartHook()
	case "session-end":
		sessionEndHook()
	}
}

// hookPayload is the subset of Claude Code's Notification hook JSON we care
// about; other fields are ignored, not an error (forward-compatible with
// whatever else the hook payload contains). notification_type distinguishes
// a real gate ("permission_prompt" et al) from the 60s idle nudge
// ("idle_prompt") — see internal/gate's scanner-side classification. Older
// claude versions may omit it (empty string), handled by a message-text
// fallback there.
type hookPayload struct {
	SessionID        string `json:"session_id"`
	Message          string `json:"message"`
	Cwd              string `json:"cwd"`
	NotificationType string `json:"notification_type"`

	// PromptID correlates this payload with the OTHER hook that fires for the
	// same gate — see gate.WriteMarker's merge rules, which degrade safely
	// when it is absent or the two hooks disagree. Observed on BOTH hooks'
	// payloads 2026-07-20 with matching values; the fixture in
	// TestPermissionHook_WritesToolDetail pins only this half. Older claude
	// versions omit it.
	PromptID string `json:"prompt_id"`

	// ToolName and ToolInput are PermissionRequest-only: what the session is
	// asking permission to do.
	ToolName  string          `json:"tool_name"`
	ToolInput json.RawMessage `json:"tool_input"`
}

// readGateHookPayload decodes the stdin JSON shared by the two hooks that
// write gate markers, reporting false when there is nothing usable to record.
// It centralizes the swallow-every-error contract those hooks depend on: an
// unreadable stdin, unparseable JSON, or a payload with no session id all mean
// "do nothing", never a non-zero exit. The session hooks decode a different
// payload type and keep their own copy.
func readGateHookPayload() (hookPayload, bool) {
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		return hookPayload{}, false
	}
	var payload hookPayload
	if err := json.Unmarshal(data, &payload); err != nil || payload.SessionID == "" {
		return hookPayload{}, false
	}
	return payload, true
}

// notifyHook reads the Notification hook's JSON from stdin and writes a
// gate marker (internal/gate.WriteMarker) — Claude Code runs this on EVERY
// notification, so it must be fast and must NEVER fail loudly: any error
// here is swallowed, not reported, and the process always exits 0. A bug in
// this path must not be able to break the user's actual claude session.
func notifyHook() {
	payload, ok := readGateHookPayload()
	if !ok {
		return
	}
	_ = gate.WriteMarker(gate.GatesDir(), payload.SessionID, gate.Info{
		Message:  payload.Message,
		Type:     payload.NotificationType,
		PromptID: payload.PromptID,
	})
}

// permissionHook reads the PermissionRequest hook's JSON from stdin and
// enriches the gate marker with WHAT is being asked — the Notification hook
// alone only ever says "Claude needs your permission", which tells an
// operator nothing about whether this gate is worth interrupting for.
//
// THIS HOOK IS A SENSOR AND MUST STAY ONE. Claude Code lets a
// PermissionRequest hook return a permissionDecision and thereby GRANT or
// DENY the permission itself. fleetops writes nothing to stdout and always
// exits 0.
//
// The reason is not timidity about automation — deciding is a direction this
// project is heading. It is that a decision made HERE leaves no trace: no
// event, no actor, no way to attribute or brake it. Decisions belong on the
// actuation path, which records them. A hook that quietly decided would let
// fleet-wide permissions change with nothing in the log to show it happened.
// Keeping sensing and acting apart is what makes the acting auditable.
func permissionHook() {
	payload, ok := readGateHookPayload()
	if !ok {
		return
	}
	_ = gate.WriteMarker(gate.GatesDir(), payload.SessionID, gate.Info{
		Message:    payload.Message,
		Type:       payload.NotificationType,
		PromptID:   payload.PromptID,
		Tool:       payload.ToolName,
		ToolDetail: summarizeToolInput(payload.ToolInput),
	})
}

// toolInputFields are the tool_input keys worth showing, most-specific
// first. tool_input's shape is per-tool and open-ended, so this is a
// deliberate small heuristic rather than an attempt at generality: these
// cover the tools that actually trigger permission prompts, and anything
// unrecognized simply shows the tool name alone (gate.Info.Detail), which is
// still strictly more than the generic notification said.
var toolInputFields = []string{"command", "file_path", "url", "pattern", "path"}

// summarizeToolInput picks one human-meaningful field out of tool_input and
// bounds it. Returns "" for anything it cannot read — a malformed or novel
// payload must not stop the marker from being written, since the tool name
// on its own is already useful.
func summarizeToolInput(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		return ""
	}
	for _, key := range toolInputFields {
		if v, ok := fields[key].(string); ok && v != "" {
			return truncateToolDetail(v)
		}
	}
	return ""
}

// toolDetailCap bounds the stored detail. A gate callout is one line in a
// cockpit that shows a whole fleet; an unbounded shell command would push
// everything else off it. Bounded at write time rather than render time so
// the marker file itself cannot grow without limit.
const toolDetailCap = 120

func truncateToolDetail(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= toolDetailCap {
		return s
	}
	return string(r[:toolDetailCap]) + "…"
}

// sessionHookPayload is the subset of Claude Code's SessionStart / SessionEnd
// hook JSON we care about; unknown fields are ignored, not an error.
//
// SessionStart's shape is confirmed live (two independent research spikes):
// session_id, cwd, transcript_path, source ("startup"/"resume"/"clear"/
// "compact"). SessionEnd's shape is NOT confirmed live — treated tolerantly:
// only session_id (the one field every hook payload carries) is relied on,
// which is all sessionEndHook needs to find the entry to delete. See
// docs/adr-vendor-independent-actuation.md §3 step 1.
type sessionHookPayload struct {
	SessionID      string `json:"session_id"`
	Cwd            string `json:"cwd"`
	TranscriptPath string `json:"transcript_path"`
	Source         string `json:"source"`
}

// sessionStartHook reads the SessionStart hook's JSON from stdin, resolves
// this session's owning `claude` pid+tty by walking up from os.Getppid(), and
// writes a session-identity entry (internal/sessions.WriteSession). Same
// non-negotiable contract as notifyHook: swallow every error, always exit 0 —
// a bug here must never be able to break the user's real claude session.
func sessionStartHook() {
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		return
	}
	var payload sessionHookPayload
	if err := json.Unmarshal(data, &payload); err != nil || payload.SessionID == "" {
		return
	}
	pid, tty := sessions.ResolveClaudeTTY(os.Getppid())
	hostApp, windowID := resolveHostWindow()
	configDir := os.Getenv(claudeConfigDirEnvVar)
	email, plan := resolveAccountLabel(configDir)
	_ = sessions.WriteSession(sessions.SessionsDir(), payload.SessionID, sessions.SessionEntry{
		PID:            pid,
		TTY:            tty,
		Cwd:            payload.Cwd,
		TranscriptPath: payload.TranscriptPath,
		Source:         payload.Source,
		StartedAt:      time.Now(),
		HostApp:        hostApp,
		WindowID:       windowID,
		ConfigDir:      configDir,
		AccountEmail:   email,
		AccountPlan:    plan,
	})
}

// claudeConfigDirEnvVar is the environment variable that fixes which Claude
// account a session runs as (see internal/accounts' package doc) — read here,
// once, at SessionStart, since that is the ONLY point in a session's life this
// hook fires with the launching environment still in scope. "" means the
// default account.
const claudeConfigDirEnvVar = "CLAUDE_CONFIG_DIR"

// accountStatusTimeout bounds the `claude auth status --json` probe below.
// The hook must never delay SessionStart unacceptably — a wedged or slow
// `claude` binary must not hold up the user's actual session starting.
const accountStatusTimeout = 2 * time.Second

// accountStatusFn is the injectable seam for the `claude auth status --json`
// probe, so tests never spawn a real `claude` binary. Production default is
// accountstatus.Query — the single definition of that subprocess and its JSON
// shape, shared with the TUI's account picker (see internal/accountstatus).
var accountStatusFn = accountstatus.Query

// resolveAccountLabel best-effort resolves the display email/plan for the
// account scoped by configDir, for the SessionStart hook to persist onto the
// session registry.
//
// configDir=="" (the default account — the overwhelmingly common,
// zero-config case) SKIPS the probe entirely rather than merely being one
// more input to it: a single-account user never displays this pair (see
// domain.Account.Label's unconditional default-account guard), so spawning
// `claude auth status` for every one of their sessions would cost a
// subprocess for information nothing ever shows — exactly the "no behavior
// change" this feature promises the common case.
//
// Any other failure — timeout, non-zero exit, malformed JSON, or a genuine
// loggedIn:false for a configured-but-not-logged-in account — degrades to
// ("", "") just as silently: this is display metadata, never load-bearing
// (ConfigDir alone is what a resume needs), so there is nothing here worth
// failing loudly over.
func resolveAccountLabel(configDir string) (email, plan string) {
	if configDir == "" {
		return "", ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), accountStatusTimeout)
	defer cancel()
	st, ok := accountStatusFn(ctx, configDir)
	if !ok || !st.LoggedIn {
		return "", ""
	}
	return st.Email, st.Plan
}

// Host terminal markers this hook recognizes: the $TERM_PROGRAM value each one
// exports, paired below with the env var carrying ITS OWN window id.
const (
	itermTermProgram = "iTerm.app"
	tmuxTermProgram  = "tmux"
)

// resolveHostWindow reads the host terminal's identity that the SessionStart
// hook inherited from the user's shell (Claude Code runs hooks as children of
// the `claude` process, which inherited these from the host terminal).
//
// The two fields are ONE fact and must be resolved together: HostApp comes from
// $TERM_PROGRAM, and WindowID from whichever env var THAT host uses. Reading
// them independently ("first non-empty of $ITERM_SESSION_ID/$TMUX_PANE") breaks
// on the common nested case — claude in tmux inside iTerm2, where tmux sets
// $TERM_PROGRAM=tmux while the outer iTerm2's $ITERM_SESSION_ID is still
// inherited — and records HostApp=tmux with an iTerm2 window id: a single
// record describing two different terminals, which would hand a foreign window
// id to any multiplexer adapter that later trusts it.
//
// An unrecognized host keeps its $TERM_PROGRAM but yields no window id: no
// window id at all beats a mismatched pair, while the host name itself stays a
// true, useful fact. Every value is best-effort and optional — empties mean no focus adapter
// and attach degrades to the manual hint, and the hook always exits 0
// regardless. Pulled out as its own helper so the env→field mapping is directly
// testable. SocketPath is intentionally left unpopulated (out of scope for this
// slice — see SessionEntry's field doc).
func resolveHostWindow() (hostApp, windowID string) {
	switch host := os.Getenv("TERM_PROGRAM"); host {
	case itermTermProgram:
		return host, os.Getenv("ITERM_SESSION_ID")
	case tmuxTermProgram:
		return host, os.Getenv("TMUX_PANE")
	default:
		// Unrecognized host (Apple_Terminal, Ghostty, WezTerm, …): keep the
		// real $TERM_PROGRAM — it is a true fact worth recording for
		// diagnostics and for showing a human where the loop lives — but
		// record NO window id, because we don't know which env var this host
		// publishes it in, and a foreign id is worse than none. No adapter
		// resolves for an unrecognized host, so attach degrades either way.
		return host, ""
	}
}

// sessionEndHook reads the SessionEnd hook's JSON from stdin and removes the
// session's identity entry (internal/sessions.DeleteSession). A SIGKILL'd
// session skips SessionEnd entirely, so a leaked entry is expected and pruned
// elsewhere by a liveness check — this is only the clean-shutdown path. Same
// swallow-every-error, always-exit-0 contract as notifyHook.
func sessionEndHook() {
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		return
	}
	var payload sessionHookPayload
	if err := json.Unmarshal(data, &payload); err != nil || payload.SessionID == "" {
		return
	}
	_ = sessions.DeleteSession(sessions.SessionsDir(), payload.SessionID)
}
