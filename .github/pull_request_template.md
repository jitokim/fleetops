<!--
Delete whatever doesn't apply. The two prompts below exist because this repo has
shipped actuation code that was dead-on-arrival against the real CLI, and because
README claims drift from behaviour easily.
-->

## What and why

## Verification

- Backend change (`internal/control/{orca,tmux,cmux}.go`, iTerm2 host send)? State the CLI
  version and what you manually drove — "unit tests pass" is not verification here.
- Behaviour described in README (backend matrix, keymap, limitations) changed? Update it in
  this PR.
