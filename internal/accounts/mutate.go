package accounts

// This file adds the load→MUTATE→save half of the package that the `fleetops
// accounts` CLI drives. The pure resolution API (accounts.go) is untouched: it
// still only READS. Mutation lives here, behind a Document that round-trips the
// on-disk JSON so a write preserves every top-level field fleetops does not
// model — a forward-compat field a newer fleetops (or a human) added must not be
// dropped the moment an older `accounts bind` rewrites the file.
//
// # Why a Document, not just Config
//
// Load returns a Config with paths already tilde-expanded and validated — the
// right shape for RESOLUTION but the wrong one for EDITING: saving it back would
// rewrite a user's readable "~/.claude-work" into an expanded absolute path even
// when the command only touched a binding, and would silently discard any field
// outside {aliases, bindings}. Document instead keeps the raw literal strings
// and the raw untouched fields, mutates only what a subcommand asks for, and
// validates the RESULT is loadable before writing — so a Save can never produce
// a file that Load would then reject (the fail-closed contract, upheld on the
// write side too).

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/jitokim/fleetops/internal/fsatomic"
)

// Sentinel errors let the CLI (and tests) branch on the KIND of rejection —
// "already exists" vs "unknown alias" vs "still in use" — without string
// matching, while each returned error still wraps a human-readable message via
// %w. Named rather than inline so a caller's errors.Is stays stable if the
// message wording changes.
var (
	// ErrInvalidAlias: the alias is not a safe slug (empty, ".", "..", or
	// contains a character outside [A-Za-z0-9_-]).
	ErrInvalidAlias = errors.New("invalid alias")
	// ErrAliasExists: AddAlias was asked to register a name already present.
	ErrAliasExists = errors.New("alias already exists")
	// ErrUnknownAlias: a bind/remove named an alias absent from "aliases".
	ErrUnknownAlias = errors.New("unknown alias")
	// ErrPathNotAbsolute: a config dir or binding path was not absolute after
	// normalization — the same fail-closed rule Load enforces on read.
	ErrPathNotAbsolute = errors.New("path is not absolute")
	// ErrAliasInUse: RemoveAlias without --force found bindings still pointing
	// at the alias.
	ErrAliasInUse = errors.New("alias still has bindings")
	// ErrEmptyPath: NormalizePath was given "".
	ErrEmptyPath = errors.New("empty path")
)

// tmpPrefix names the sibling temp file fsatomic writes before renaming over
// accounts.json — traceable to this writer if a hard kill ever strands one,
// mirroring the ".hidden-*.tmp"/".sessions-*.tmp" the other registries use.
const tmpPrefix = ".accounts-*.tmp"

// Document is an accounts.json opened for editing: the known {aliases,
// bindings} as literal (un-expanded) values, plus every OTHER top-level field
// kept verbatim so Save round-trips it. Construct it with LoadDocument, never
// by hand.
type Document struct {
	path string
	// other holds top-level fields that are neither "aliases" nor "bindings",
	// decoded generically so Save re-emits them intact instead of dropping them.
	other map[string]json.RawMessage
	// Aliases / Bindings mirror Config's fields but hold the RAW literal strings
	// as written (no tilde expansion), because Save must not rewrite a "~"-form
	// path the current command did not touch.
	Aliases  map[string]string
	Bindings []Binding
}

// LoadDocument opens path for mutation. A MISSING file is not an error — it
// returns an empty, writable Document (the first `accounts add` creates the
// file). A present-but-malformed file (bad JSON, or a top level that is not an
// object) IS an error, never silently clobbered: the user opted in, so a typo
// must stop the write rather than let it overwrite their file with a fresh one.
func LoadDocument(path string) (*Document, error) {
	doc := &Document{
		path:    path,
		other:   map[string]json.RawMessage{},
		Aliases: map[string]string{},
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return doc, nil
		}
		return nil, fmt.Errorf("accounts: reading %s: %w", path, err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		return nil, fmt.Errorf("accounts: parsing %s: %w", path, err)
	}
	for key, raw := range top {
		switch key {
		case "aliases":
			if err := json.Unmarshal(raw, &doc.Aliases); err != nil {
				return nil, fmt.Errorf("accounts: parsing %s: \"aliases\": %w", path, err)
			}
		case "bindings":
			if err := json.Unmarshal(raw, &doc.Bindings); err != nil {
				return nil, fmt.Errorf("accounts: parsing %s: \"bindings\": %w", path, err)
			}
		default:
			doc.other[key] = raw
		}
	}
	if doc.Aliases == nil {
		doc.Aliases = map[string]string{}
	}
	return doc, nil
}

// AddAlias registers alias → configDir. It rejects a name that is not a safe
// slug, a duplicate name, and a non-absolute configDir — the three ways an add
// could seed a file that later resolves to the wrong (or an unauthenticated)
// account. configDir is stored verbatim; the caller has already resolved it to
// an absolute path.
func (d *Document) AddAlias(alias, configDir string) error {
	if !IsSafeAlias(alias) {
		return fmt.Errorf("accounts: %q is not a valid alias — use letters, digits, '-' or '_': %w", alias, ErrInvalidAlias)
	}
	if _, exists := d.Aliases[alias]; exists {
		return fmt.Errorf("accounts: alias %q already exists (config dir %s): %w", alias, d.Aliases[alias], ErrAliasExists)
	}
	if !filepath.IsAbs(configDir) {
		return fmt.Errorf("accounts: config dir %q for alias %q is not absolute: %w", configDir, alias, ErrPathNotAbsolute)
	}
	d.Aliases[alias] = configDir
	return nil
}

// Bind attaches an absolute path to an existing alias. An unknown alias is an
// error (binding to a name "aliases" does not define is exactly the fail-closed
// mistake Load rejects on read). A path already bound is REPLACED in place,
// preserving its position; a new path is appended. Longest-prefix precedence is
// the resolver's job — Bind only records.
func (d *Document) Bind(path, alias string) error {
	if _, ok := d.Aliases[alias]; !ok {
		return fmt.Errorf("accounts: unknown alias %q — register it first with 'fleetops accounts add %s': %w", alias, alias, ErrUnknownAlias)
	}
	if !filepath.IsAbs(path) {
		return fmt.Errorf("accounts: bind path %q is not absolute: %w", path, ErrPathNotAbsolute)
	}
	clean := filepath.Clean(path)
	for i := range d.Bindings {
		if filepath.Clean(d.Bindings[i].Path) == clean {
			d.Bindings[i].Alias = alias
			return nil
		}
	}
	d.Bindings = append(d.Bindings, Binding{Path: clean, Alias: alias})
	return nil
}

// Unbind removes every binding whose path matches (component-wise clean) path,
// returning whether anything was removed so the CLI can report "no such
// binding" instead of silently succeeding.
func (d *Document) Unbind(path string) bool {
	clean := filepath.Clean(path)
	kept := make([]Binding, 0, len(d.Bindings))
	removed := false
	for _, b := range d.Bindings {
		if filepath.Clean(b.Path) == clean {
			removed = true
			continue
		}
		kept = append(kept, b)
	}
	if removed {
		d.Bindings = kept
	}
	return removed
}

// RemoveAlias deletes the alias from "aliases". It NEVER touches the config dir
// or its credentials on disk — removing an alias is un-naming an account, not
// logging it out. If bindings still reference the alias it refuses unless force
// is set; with force it also drops those bindings, because leaving a binding
// that names a now-absent alias would make the file fail to load (the same
// fail-closed rule, kept true across the mutation).
func (d *Document) RemoveAlias(alias string, force bool) error {
	if _, ok := d.Aliases[alias]; !ok {
		return fmt.Errorf("accounts: unknown alias %q: %w", alias, ErrUnknownAlias)
	}
	refs := d.BindingsForAlias(alias)
	if len(refs) > 0 && !force {
		return fmt.Errorf("accounts: alias %q still bound by %d path(s): %v — unbind them or pass --force: %w", alias, len(refs), refs, ErrAliasInUse)
	}
	if force && len(refs) > 0 {
		kept := make([]Binding, 0, len(d.Bindings))
		for _, b := range d.Bindings {
			if b.Alias == alias {
				continue
			}
			kept = append(kept, b)
		}
		d.Bindings = kept
	}
	delete(d.Aliases, alias)
	return nil
}

// BindingsForAlias returns the paths currently bound to alias, in file order —
// what RemoveAlias reports as blockers and what `list` groups under each alias.
func (d *Document) BindingsForAlias(alias string) []string {
	return bindingPathsFor(d.Bindings, alias)
}

// BindingsForAlias returns the paths a resolved Config binds to alias, in file
// order — the read-side twin of Document.BindingsForAlias, so the `list`
// surface groups bindings under each alias without re-implementing the filter.
func (c Config) BindingsForAlias(alias string) []string {
	return bindingPathsFor(c.Bindings, alias)
}

// bindingPathsFor is the single filter both BindingsForAlias methods share:
// the paths of every binding naming alias, in slice order. Extracted so the
// Config and Document twins cannot drift on what "bound to this alias" means.
func bindingPathsFor(bindings []Binding, alias string) []string {
	var paths []string
	for _, b := range bindings {
		if b.Alias == alias {
			paths = append(paths, b.Path)
		}
	}
	return paths
}

// Save writes the document back to disk atomically, preserving unknown
// top-level fields. It first validates that the result is LOADABLE (paths
// absolute after expansion, no binding naming a missing alias) so a Save can
// never leave a file Load would reject. The write is fsatomic (sibling temp +
// rename), so a reader — or a crash mid-write — sees either the old file or the
// new one, never a torn mix.
//
// Preservation is TOP-LEVEL only: an unknown key beside "aliases"/"bindings"
// round-trips via d.other, but "aliases" and "bindings" are re-serialized from
// the typed fields, so an unknown key added INSIDE an individual binding object
// (a future per-binding note, say) would not survive a Save. Binding has only
// {path, alias} everywhere in the codebase today; this is a known limitation to
// revisit if per-binding metadata is ever introduced.
func (d *Document) Save() error {
	if err := d.expandedConfig().validate(); err != nil {
		return err
	}
	out := make(map[string]any, len(d.other)+2)
	for key, raw := range d.other {
		var decoded any
		if err := json.Unmarshal(raw, &decoded); err != nil {
			return fmt.Errorf("accounts: re-encoding preserved field %q: %w", key, err)
		}
		out[key] = decoded
	}
	// Normalize empties so a fresh file reads {"aliases": {...}, "bindings": []}
	// rather than a null — the shape the README documents and the resolver and
	// LoadDocument both round-trip cleanly.
	aliases := d.Aliases
	if aliases == nil {
		aliases = map[string]string{}
	}
	bindings := d.Bindings
	if bindings == nil {
		bindings = []Binding{}
	}
	out["aliases"] = aliases
	out["bindings"] = bindings

	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return fmt.Errorf("accounts: encoding %s: %w", d.path, err)
	}
	data = append(data, '\n')
	if err := fsatomic.WriteFile(d.path, data, tmpPrefix); err != nil {
		return fmt.Errorf("accounts: writing %s: %w", d.path, err)
	}
	return nil
}

// expandedConfig builds the resolution-shaped Config (tilde-expanded) from the
// current in-memory state, without mutating the Document's own literal strings.
// Used by both Save's pre-write validation and Config's read snapshot so the two
// apply the identical expansion the resolver does.
func (d *Document) expandedConfig() Config {
	cfg := Config{
		Aliases:  make(map[string]string, len(d.Aliases)),
		Bindings: make([]Binding, len(d.Bindings)),
	}
	for name, dir := range d.Aliases {
		cfg.Aliases[name] = dir
	}
	copy(cfg.Bindings, d.Bindings)
	home, _ := os.UserHomeDir()
	cfg.expandPaths(home)
	return cfg
}

// NormalizePath expands a leading "~"/"~/" and makes path absolute (relative to
// the process cwd) — the form bindings and alias config dirs are stored in. An
// empty path, or one that cannot be made absolute (cwd unavailable), is an
// error so a bad argument stops the command rather than writing a path that can
// never match a real cwd.
func NormalizePath(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("accounts: %w", ErrEmptyPath)
	}
	home, _ := os.UserHomeDir()
	expanded := expandTilde(path, home)
	abs, err := filepath.Abs(expanded)
	if err != nil {
		return "", fmt.Errorf("accounts: cannot make %q absolute: %w", path, err)
	}
	return abs, nil
}

// IsSafeAlias reports whether alias is a filesystem- and display-safe slug: a
// non-empty string of [A-Za-z0-9_-] that is not "." or "..". It gates AddAlias
// because an alias also seeds the default config-dir path
// (~/.fleetops/accounts/<alias>); a "/" or ".." there would escape that
// directory, and a display badge full of control characters would corrupt the
// TUI.
func IsSafeAlias(alias string) bool {
	if alias == "" || alias == "." || alias == ".." {
		return false
	}
	for _, r := range alias {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '-' || r == '_':
		default:
			return false
		}
	}
	return true
}
