package main

import (
	"encoding/json"
	"io"
	"os"

	"github.com/jitokim/missionctl/internal/gate"
)

// runHookCmd dispatches `missionctl hook <sub>`. Unknown subcommands are
// silently ignored (exit 0) — see notifyHook for why.
func runHookCmd(args []string) {
	if len(args) == 0 || args[0] != "notify" {
		return
	}
	notifyHook()
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
