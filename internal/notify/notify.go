// Package notify sends best-effort desktop notifications — macOS first (via
// `osascript -e 'display notification ...'`), no other platform yet. Every
// caller in this codebase MUST swallow Send's error: a notification failing
// (no osascript, sandboxed environment, timeout) must never disrupt the
// fleet loop it's merely describing — same additive, non-critical-path
// discipline as internal/events.
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
// itself.
func appleScriptString(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}
