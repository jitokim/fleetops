package domain

import "path/filepath"

// Account is a loop's Claude account identity, captured once at SessionStart
// and read-only afterward (multi-account Phase B — see internal/accounts'
// package doc for the model this observes).
//
// ConfigDir is the load-bearing field: it is the CLAUDE_CONFIG_DIR the
// session was ACTUALLY launched under, recorded verbatim in the session
// registry (internal/sessions.SessionEntry.ConfigDir) — never re-derived from
// whatever ~/.fleetops/accounts.json currently binds the loop's cwd to, which
// can disagree with what this specific session started under. A resume
// (control.Redrive) must act on THIS value, not on a fresh cwd-based lookup —
// see Redrive's own "Phase B" doc for why the two must never be conflated.
//
// The zero value (every field "") is the common case — no accounts.json, or a
// session recorded before this field existed — and Label() renders it as ""
// (no display, no noise), matching the zero-config posture the rest of the
// accounts feature holds throughout.
type Account struct {
	// ConfigDir is "" for the default account (CLAUDE_CONFIG_DIR unset at
	// launch). This is the ONLY field IsDefault and a resume act on.
	ConfigDir string
	// Alias is the human name accounts.json's Aliases map ConfigDir to
	// (internal/accounts.Config.AliasForConfigDir), resolved by the scanner
	// once per scan and copied in — "" when ConfigDir has no registered
	// alias. Domain itself never imports internal/accounts (see this
	// package's doc: it stays dependency-free); the resolution happens one
	// layer up, in internal/claude's scan.
	Alias string
	// Email is the account's login identity, best-effort from `claude auth
	// status --json` at SessionStart (internal/sessions.SessionEntry.
	// AccountEmail) — never a token, and "" whenever the status probe was
	// skipped, timed out, errored, or reported loggedIn:false.
	Email string
	// Plan is the account's subscription tier (SessionEntry.AccountPlan),
	// same best-effort provenance as Email.
	Plan string
}

// IsDefault reports whether this is the default account — CLAUDE_CONFIG_DIR
// was unset (or empty) when the session launched. Derived from ConfigDir
// rather than stored as its own flag: a second, independently-settable field
// could drift from the ConfigDir it's supposed to describe.
func (a Account) IsDefault() bool {
	return a.ConfigDir == ""
}

// Label is the account's human-facing display string, in falling priority:
//
//  1. Alias, when ConfigDir maps to one ("company").
//  2. Email, when known ("jito@company.com").
//  3. A short form of ConfigDir itself (its base name), when nothing else is
//     known but the account is still non-default.
//  4. "" for the default account, UNCONDITIONALLY — checked first, before
//     Alias/Email, so a single-account user (ConfigDir=="") never sees a
//     row even in the hypothetical case Alias/Email got populated for the
//     default account by some future caller. This is the zero-noise
//     guarantee: the common single-account user must see nothing new.
func (a Account) Label() string {
	if a.IsDefault() {
		return ""
	}
	if a.Alias != "" {
		return a.Alias
	}
	if a.Email != "" {
		return a.Email
	}
	return filepath.Base(filepath.Clean(a.ConfigDir))
}

// DetailValue is the DETAIL panel's CLAUDE row text: the alias and the login
// email together when both are known ("my — a@b.com"), so the row reads
// symmetrically with the GIT identity row beside it. Falls back gracefully:
// alias alone when no email was captured, email alone when the config dir has
// no registered alias, and "" for the default account (no row — same as
// Label). Never a token; Email is login identity only.
func (a Account) DetailValue() string {
	if a.IsDefault() {
		return ""
	}
	switch {
	case a.Alias != "" && a.Email != "":
		return a.Alias + " — " + a.Email
	case a.Alias != "":
		return a.Alias
	case a.Email != "":
		return a.Email
	default:
		return filepath.Base(filepath.Clean(a.ConfigDir))
	}
}
