# Contributing to fleetops

fleetops is a small, 0.1.0-alpha Go CLI. Contributions — bug reports,
backend fixes, platform support — are welcome, but read this first: the
project actuates real terminals (see README's "How it works"), so changes to
`internal/control` in particular need to be verified against the real CLI
they target, not just unit-tested.

## Before you start

For anything beyond a small fix, open an issue first describing what you want
to change and why. This avoids wasted work on a PR that doesn't fit the
project's direction — especially for new backends or engine-shaped features
(see [`VISION.md`](./VISION.md) for where the project is headed and what's
explicitly not built yet).

## Development setup

```bash
git clone https://github.com/jitokim/fleetops.git
cd fleetops
make build
./fleetops
```

Requires Go 1.25+ (see `go.mod`). No other toolchain or codegen step. See the
`Makefile` for `build`/`install`/`test`/`fmt`/`vet` targets.

## Making changes

1. Fork the repo and create a branch off `main`.
2. Keep changes focused — one logical change per PR. Unrelated cleanups
   belong in a separate PR.
3. Match the existing code style: no new dependencies without a strong
   reason (the project deliberately stays thin — see `go.mod`), comments
   explain *why*, not *what*.
4. If you touch a backend (`internal/control/{orca,tmux,cmux}.go`), state in
   the PR description what you verified it against (CLI version, manual
   test) — this codebase's history includes actuation code that looked
   correct but was dead-on-arrival against the real CLI (see README's "Known
   rough edges"). A behavior claim without verification will get pushback.

## Testing

```bash
make build
make test   # go test ./... -count=1 -race
```

- Add unit tests for new logic, especially parsing/state-machine code
  (`internal/claude`, `internal/engine`, `internal/oracle`).
- Backend actuation code (`internal/control`) is inherently hard to unit-test
  against a real terminal multiplexer — if you can't write a meaningful test,
  say so in the PR and describe your manual verification instead of skipping
  verification altogether.
- There is no CI pipeline yet; `go build ./...` and `go test ./...` passing
  locally is the current bar.

## Commit messages

This repo follows [Conventional Commits](https://www.conventionalcommits.org/)
(`feat:`, `fix:`, `docs:`, `refactor:`, `test:`, `chore:`). Keep the subject
line short; use the body for the "why" if it's not obvious.

## Submitting a PR

- Describe what changed and why, not just what.
- Link any related issue.
- If the change affects behavior described in `README.md` (backend matrix,
  keymap, limitations), update the README in the same PR — docs and code
  drifting apart is exactly the kind of gap this project wants to avoid in
  itself.

## Reporting bugs

Open a GitHub issue with:
- fleetops version / commit, OS, and which backend (orca / tmux / cmux /
  bare terminal) you were using.
- What you expected vs. what happened.
- The relevant session's observed state if applicable (`run`/`idle`/`stalled`/
  etc. from the fleet table) — it narrows down whether the bug is in
  observation or actuation.

For anything actuation-related that could plausibly be a security issue
(sending keystrokes to, or killing, the wrong process), see
[`SECURITY.md`](./SECURITY.md) instead of a public issue.

## License

By contributing, you agree that your contributions will be licensed under
the project's [MIT License](./LICENSE).
