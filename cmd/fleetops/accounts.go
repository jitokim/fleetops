package main

// `fleetops accounts <sub>` — the account management CLI (Phase E). It is the
// single surface a user drives to set up, inspect, and tear down the Claude
// accounts the multi-account feature spawns loops under, so nobody ever has to
// hand-edit ~/.fleetops/accounts.json or type `CLAUDE_CONFIG_DIR=… claude login`
// in a shell. Every mutation goes through internal/accounts.Document (atomic,
// field-preserving writes); every login goes through the shared
// control.LoginArgv builder; no token is ever read, printed, or stored.

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jitokim/fleetops/internal/accounts"
	"github.com/jitokim/fleetops/internal/accountstatus"
	"github.com/jitokim/fleetops/internal/control"
	"github.com/jitokim/fleetops/internal/hooks"
)

// statusProbeTimeout bounds each `claude auth status` probe `accounts list`
// runs — the same 2s the SessionStart hook and the wizard give it. list is a
// human-facing "did it work" surface, not a hot path, so a wedged or missing
// claude degrades that account's row to "unknown", never hangs the command.
const statusProbeTimeout = 2 * time.Second

// loginRunner runs `claude login` for a config dir with the terminal attached,
// so the human sees claude's OAuth URL and prompt directly. A package var so a
// test substitutes a recorder and asserts WHICH config dir the add/login flow
// would authenticate, without spawning a real claude.
var loginRunner = runClaudeLogin

// runClaudeLogin executes the shared control.LoginArgv invocation
// (`env CLAUDE_CONFIG_DIR=<dir> claude login`, or bare `claude login` for the
// default account) with stdio inherited — this IS the browser login the old
// manual shell step performed, now triggered by fleetops. It returns whatever
// claude exits with; the credential write into configDir is claude's, never
// fleetops's, so no token passes through this process.
func runClaudeLogin(configDir string) error {
	argv := control.LoginArgv(configDir)
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

// runAccountsCmd dispatches `fleetops accounts <sub>`. Bare `accounts` (no
// subcommand) lists, since the list is the surface a user reaches for most.
func runAccountsCmd(args []string) {
	if len(args) == 0 {
		listAccounts()
		return
	}
	switch args[0] {
	case "list":
		listAccounts()
	case "add":
		addAccount(args[1:])
	case "login":
		loginAccount(args[1:])
	case "bind":
		bindAccount(args[1:])
	case "unbind":
		unbindAccount(args[1:])
	case "remove":
		removeAccount(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "fleetops accounts: unknown subcommand %q (want list|add|login|bind|unbind|remove)\n", args[0])
		os.Exit(1)
	}
}

// splitFlagsAndArgs partitions a subcommand's args into flag tokens (with the
// value of any flag named in valueFlags) and positional operands, so a flag may
// follow the positional alias — `accounts add work --no-login`, the ordering the
// documented signature `add <alias> [--dir <path>] [--no-login]` implies. Go's
// flag package otherwise stops at the first positional and would mis-read a
// trailing `--no-login` as an operand, rejecting a perfectly ordinary command.
// valueFlags is keyed by flag name WITHOUT dashes ("dir"); a `--flag=value`
// token carries its own value and consumes no following arg.
func splitFlagsAndArgs(args []string, valueFlags map[string]bool) (flags, positional []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "-" || !strings.HasPrefix(a, "-") {
			positional = append(positional, a)
			continue
		}
		flags = append(flags, a)
		name := strings.TrimLeft(a, "-")
		if !strings.Contains(a, "=") && valueFlags[name] && i+1 < len(args) {
			i++
			flags = append(flags, args[i])
		}
	}
	return flags, positional
}

// addAccount registers an alias and, by DEFAULT, launches its browser login —
// the one-step "set up an account" flow Phase E exists to deliver (the manual
// step it replaces was exactly `CLAUDE_CONFIG_DIR=… claude login`). `--no-login`
// registers only and prints how to log in later. `--dir` overrides the default
// config dir (~/.fleetops/accounts/<alias>) and must be absolute.
func addAccount(args []string) {
	fs := flag.NewFlagSet("accounts add", flag.ExitOnError)
	dir := fs.String("dir", "", "config dir for the account (absolute); default ~/.fleetops/accounts/<alias>")
	noLogin := fs.Bool("no-login", false, "register the alias only; do not launch claude login")
	flags, rest := splitFlagsAndArgs(args, map[string]bool{"dir": true})
	_ = fs.Parse(flags)
	if len(rest) != 1 {
		fmt.Fprintln(os.Stderr, "usage: fleetops accounts add <alias> [--dir <path>] [--no-login]")
		os.Exit(1)
	}
	alias := rest[0]

	configDir, err := resolveAddDir(alias, *dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fleetops accounts add:", err)
		os.Exit(1)
	}

	doc, err := accounts.LoadDocument(accounts.DefaultPath())
	if err != nil {
		fmt.Fprintln(os.Stderr, "fleetops accounts add:", err)
		os.Exit(1)
	}
	// AddAlias validates slug/duplicate/absolute BEFORE we create any directory,
	// so a rejected add leaves no stray dir behind.
	if err := doc.AddAlias(alias, configDir); err != nil {
		fmt.Fprintln(os.Stderr, "fleetops accounts add:", err)
		os.Exit(1)
	}
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		fmt.Fprintf(os.Stderr, "fleetops accounts add: creating %s: %v\n", configDir, err)
		os.Exit(1)
	}
	if err := doc.Save(); err != nil {
		fmt.Fprintln(os.Stderr, "fleetops accounts add:", err)
		os.Exit(1)
	}
	fmt.Printf("registered alias %q → %s\n", alias, configDir)

	if *noLogin {
		fmt.Printf("not logged in yet — run: fleetops accounts login %s\n", alias)
		return
	}
	launchLogin(alias, configDir)
}

// resolveAddDir picks the config dir for a new alias: the caller's --dir if
// given (which MUST be absolute — a relative config dir would spawn an
// unauthenticated session), else the default ~/.fleetops/accounts/<alias>.
func resolveAddDir(alias, dirFlag string) (string, error) {
	if dirFlag != "" {
		if !filepath.IsAbs(dirFlag) {
			return "", fmt.Errorf("--dir %q must be absolute", dirFlag)
		}
		return filepath.Clean(dirFlag), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot determine home dir for default config dir: %w", err)
	}
	return filepath.Join(home, ".fleetops", "accounts", alias), nil
}

// loginAccount runs the browser login for an already-registered alias.
func loginAccount(args []string) {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: fleetops accounts login <alias>")
		os.Exit(1)
	}
	alias := args[0]
	cfg, err := accounts.Load(accounts.DefaultPath())
	if err != nil {
		fmt.Fprintln(os.Stderr, "fleetops accounts login:", err)
		os.Exit(1)
	}
	dir, ok := cfg.Aliases[alias]
	if !ok {
		fmt.Fprintf(os.Stderr, "fleetops accounts login: unknown alias %q — see: fleetops accounts list\n", alias)
		os.Exit(1)
	}
	launchLogin(alias, dir)
}

// launchLogin triggers claude's browser OAuth for configDir and reports the
// outcome. A login that does not complete (claude missing, user aborted) is not
// fatal to a prior registration — it prints how to retry and exits non-zero so a
// script can tell it did not finish.
func launchLogin(alias, configDir string) {
	fmt.Printf("launching login for %q (a browser window opens; complete it there)…\n", alias)
	if err := loginRunner(configDir); err != nil {
		fmt.Fprintf(os.Stderr, "login for %q did not complete: %v\n  retry with: fleetops accounts login %s\n", alias, err, alias)
		os.Exit(1)
	}
	fmt.Printf("logged in: %s\n", alias)
}

// bindAccount attaches a directory (or repo) to an alias so loops spawned under
// it — and worktrees derived from it — run on that account.
func bindAccount(args []string) {
	if len(args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: fleetops accounts bind <alias> <path>")
		os.Exit(1)
	}
	alias, rawPath := args[0], args[1]
	abs, err := accounts.NormalizePath(rawPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fleetops accounts bind:", err)
		os.Exit(1)
	}
	doc, err := accounts.LoadDocument(accounts.DefaultPath())
	if err != nil {
		fmt.Fprintln(os.Stderr, "fleetops accounts bind:", err)
		os.Exit(1)
	}
	if err := doc.Bind(abs, alias); err != nil {
		fmt.Fprintln(os.Stderr, "fleetops accounts bind:", err)
		os.Exit(1)
	}
	if err := doc.Save(); err != nil {
		fmt.Fprintln(os.Stderr, "fleetops accounts bind:", err)
		os.Exit(1)
	}
	fmt.Printf("bound %s → %s\n", abs, alias)
}

// unbindAccount removes a directory binding (the reverse of bind).
func unbindAccount(args []string) {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: fleetops accounts unbind <path>")
		os.Exit(1)
	}
	abs, err := accounts.NormalizePath(args[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, "fleetops accounts unbind:", err)
		os.Exit(1)
	}
	doc, err := accounts.LoadDocument(accounts.DefaultPath())
	if err != nil {
		fmt.Fprintln(os.Stderr, "fleetops accounts unbind:", err)
		os.Exit(1)
	}
	if !doc.Unbind(abs) {
		fmt.Fprintf(os.Stderr, "fleetops accounts unbind: no binding for %s\n", abs)
		os.Exit(1)
	}
	if err := doc.Save(); err != nil {
		fmt.Fprintln(os.Stderr, "fleetops accounts unbind:", err)
		os.Exit(1)
	}
	fmt.Printf("unbound %s\n", abs)
}

// removeAccount un-names an alias. It refuses while bindings still reference it
// unless --force (which also drops those bindings). It never deletes the config
// dir or its credentials — removing an alias is not logging out.
func removeAccount(args []string) {
	fs := flag.NewFlagSet("accounts remove", flag.ExitOnError)
	force := fs.Bool("force", false, "remove even if bindings still reference the alias (those bindings are dropped)")
	flags, rest := splitFlagsAndArgs(args, nil)
	_ = fs.Parse(flags)
	if len(rest) != 1 {
		fmt.Fprintln(os.Stderr, "usage: fleetops accounts remove <alias> [--force]")
		os.Exit(1)
	}
	alias := rest[0]
	doc, err := accounts.LoadDocument(accounts.DefaultPath())
	if err != nil {
		fmt.Fprintln(os.Stderr, "fleetops accounts remove:", err)
		os.Exit(1)
	}
	if err := doc.RemoveAlias(alias, *force); err != nil {
		fmt.Fprintln(os.Stderr, "fleetops accounts remove:", err)
		os.Exit(1)
	}
	if err := doc.Save(); err != nil {
		fmt.Fprintln(os.Stderr, "fleetops accounts remove:", err)
		os.Exit(1)
	}
	fmt.Printf("removed alias %q (its config dir and credentials are left untouched)\n", alias)
}

// accountRow is one alias's fully-resolved display state — gathered by the I/O
// driver (listAccounts) and rendered by the pure formatAccountsList, so the
// formatting is unit-testable without a real claude or filesystem.
type accountRow struct {
	alias     string
	configDir string
	probeOK   bool // whether `claude auth status` ran at all
	loggedIn  bool
	email     string
	plan      string
	hooksOK   bool
	bindings  []string
}

// listAccounts prints every alias with its config dir, login state + email, hook
// install state, and the paths bound to it — the "did it work" surface. Login
// state and hook state are probed live (bounded), so this is the driver: it does
// the I/O, then hands rows to the pure formatter.
func listAccounts() {
	cfg, err := accounts.Load(accounts.DefaultPath())
	if err != nil {
		fmt.Fprintln(os.Stderr, "fleetops accounts:", err)
		os.Exit(1)
	}
	if len(cfg.Aliases) == 0 {
		fmt.Println("no accounts configured — add one with:\n  fleetops accounts add <alias>")
		return
	}

	names := make([]string, 0, len(cfg.Aliases))
	for name := range cfg.Aliases {
		names = append(names, name)
	}
	sort.Strings(names)

	rows := make([]accountRow, 0, len(names))
	for _, name := range names {
		dir := cfg.Aliases[name]
		row := accountRow{alias: name, configDir: dir, bindings: cfg.BindingsForAlias(name)}
		if st, ok := probeStatus(dir); ok {
			row.probeOK = true
			row.loggedIn = st.LoggedIn
			row.email = st.Email
			row.plan = st.Plan
		}
		row.hooksOK = hooksInstalledIn(dir)
		rows = append(rows, row)
	}
	fmt.Print(formatAccountsList(rows))
}

// probeStatus runs the bounded `claude auth status` probe for one config dir.
func probeStatus(configDir string) (accountstatus.Status, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), statusProbeTimeout)
	defer cancel()
	return accountstatus.Query(ctx, configDir)
}

// hooksInstalledIn reports whether fleetops's hooks are fully installed in a
// config dir — reusing the same Health check `hooks status` uses, so a
// not-installed account (whose loops would record nothing) surfaces here too.
func hooksInstalledIn(configDir string) bool {
	return hooks.HealthAt(filepath.Join(configDir, "settings.json"), hooks.BinaryExists).OK
}

// formatAccountsList renders resolved rows as human-readable blocks. Pure (no
// I/O) so it is table-testable — listAccounts only gathers the rows and prints
// what this returns. It never shows a token: only alias, config dir, email,
// plan, login state, hook state, and bindings.
func formatAccountsList(rows []accountRow) string {
	var b strings.Builder
	for i, r := range rows {
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "%s\n", r.alias)
		fmt.Fprintf(&b, "  config  %s\n", r.configDir)
		fmt.Fprintf(&b, "  login   %s\n", loginLine(r))
		fmt.Fprintf(&b, "  hooks   %s\n", hooksLine(r))
		fmt.Fprintf(&b, "  bound   %s\n", bindingsLine(r.bindings))
	}
	return b.String()
}

// loginLine describes an account's login state without ever naming a token —
// only whether it is signed in and, if so, its email and plan.
func loginLine(r accountRow) string {
	if !r.probeOK {
		return "unknown (claude auth status unavailable)"
	}
	if !r.loggedIn {
		return fmt.Sprintf("not logged in — run: fleetops accounts login %s", r.alias)
	}
	detail := r.email
	if detail == "" {
		detail = "logged in"
	}
	if r.plan != "" {
		detail += " (" + r.plan + ")"
	}
	return "logged in: " + detail
}

// hooksLine describes whether fleetops's hooks are installed in the account's
// config dir — a not-installed account records nothing, a real gotcha worth
// surfacing right beside the login state.
func hooksLine(r accountRow) string {
	if r.hooksOK {
		return "installed"
	}
	return "not installed — run: fleetops hooks install"
}

// bindingsLine lists the paths bound to an alias, or "(none)".
func bindingsLine(paths []string) string {
	if len(paths) == 0 {
		return "(none)"
	}
	return strings.Join(paths, "\n          ")
}
