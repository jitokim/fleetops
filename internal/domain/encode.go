package domain

import "strings"

// EncodeCwd maps an absolute path to Claude Code's project-dir encoding:
// both "/" and "." become "-" (e.g. /Users/x/.claude-mem/obs →
// -Users-x--claude-mem-obs). The encoding is lossy many-to-one, so matching
// must always be done in THIS direction (real path → encoded), never by
// decoding a project dir back to a path. Single source of truth — the
// claude, control, and registry packages all match through this.
func EncodeCwd(path string) string {
	return strings.ReplaceAll(strings.ReplaceAll(path, "/", "-"), ".", "-")
}
