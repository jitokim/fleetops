package main

import (
	"encoding/json"
	"io"
	"os"
	"time"

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
}

// notifyHook reads the Notification hook's JSON from stdin and writes a
// gate marker (internal/gate.WriteMarker) — Claude Code runs this on EVERY
// notification, so it must be fast and must NEVER fail loudly: any error
// here is swallowed, not reported, and the process always exits 0. A bug in
// this path must not be able to break the user's actual claude session.
func notifyHook() {
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		return
	}
	var payload hookPayload
	if err := json.Unmarshal(data, &payload); err != nil || payload.SessionID == "" {
		return
	}
	_ = gate.WriteMarker(gate.GatesDir(), payload.SessionID, payload.Message, payload.NotificationType)
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
	_ = sessions.WriteSession(sessions.SessionsDir(), payload.SessionID, sessions.SessionEntry{
		PID:            pid,
		TTY:            tty,
		Cwd:            payload.Cwd,
		TranscriptPath: payload.TranscriptPath,
		Source:         payload.Source,
		StartedAt:      time.Now(),
	})
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
