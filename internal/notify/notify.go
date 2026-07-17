// Package notify sends best-effort desktop notifications — macOS first (via
// `osascript -e 'display notification ...'`), no other platform yet. Every
// caller in this codebase MUST swallow Send's error: a notification failing
// (no osascript, sandboxed environment, timeout) must never disrupt the
// fleet loop it's merely describing — same additive, non-critical-path
// discipline as internal/events.
//
// Known limitation: a notification sent via `osascript -e 'display
// notification'` always shows in Notification Center under the generic
// "Script Editor" icon — osascript has no flag to point it at a different
// one, and there's no way around that short of shipping a real .app bundle
// (out of scope for a CLI tool). Mitigated for now by prefixing the TITLE
// with a 🚀 emoji at the call site (internal/tui's notifyTitlePrefix) so a
// fleetops notification is at least visually identifiable at a glance.
// Future options if this needs a real fix: shell out to
// `terminal-notifier -appIcon <path>` instead (a popular third-party CLI
// that DOES support a custom icon, if the user has it installed), or ship
// fleetops as a proper .app bundle with its own icon and use a native
// notification API instead of osascript entirely.
package notify

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// timeout bounds the osascript call — a hung/blocked notification daemon
// must not hang the caller (the tui's event loop, indirectly, since Send is
// always invoked from a tea.Cmd — see internal/tui).
const timeout = 3 * time.Second

// runner is overridable in tests so Send's argv construction can be
// verified end-to-end without actually invoking osascript — which doesn't
// exist outside macOS, and would pop a real, visible notification on a dev
// machine running `go test`.
var runner = func(ctx context.Context, argv []string) error {
	return exec.CommandContext(ctx, argv[0], argv[1:]...).Run()
}

// Send fires a macOS desktop notification with title/body. Both are
// arbitrary, loop-derived text (a gate prompt, a project label) — never
// naively interpolated into the AppleScript source, see appleScriptString.
func Send(title, body string) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return runner(ctx, argv(title, body))
}

// argv builds osascript's argument VECTOR (never a shell string — argv[i]
// are passed directly to exec, so there is no shell to inject into; the
// only escaping needed is for AppleScript's own string-literal syntax
// inside the -e script, see appleScriptString). Exposed (unexported, but
// separated from Send) so tests can assert on the exact argv without
// executing it.
func argv(title, body string) []string {
	script := fmt.Sprintf("display notification %s with title %s", appleScriptString(body), appleScriptString(title))
	return []string{"osascript", "-e", script}
}

// appleScriptString quotes s as an AppleScript string literal: backslash
// must be escaped FIRST (otherwise escaping the quote afterward would double
// up any backslash that itself preceded a quote), then the double quote
// itself, then — review fix (P2) — any raw newline/carriage-return byte.
// A multi-line gate prompt (a real possibility — Claude Code's
// AskUserQuestion/permission prompts can wrap several lines) previously
// reached osascript with a literal embedded newline, which is not valid
// inside an AppleScript double-quoted string literal: `display
// notification` would fail with a syntax error and the whole notification
// was silently lost (Send's error is always swallowed by callers). Escaping
// to the two-character `\n`/`\r` sequences is safe to do AFTER the
// backslash-doubling pass above — it operates on the RAW newline byte, not
// on a backslash, so it can't interact with (or be corrupted by) that
// earlier step.
func appleScriptString(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "\r", `\r`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	return `"` + s + `"`
}
