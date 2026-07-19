# The `/review` command (maintainers)

Status: **dispatch plumbing only.** The trigger, authorization gate, and
status reporting work. The review engine behind them does not exist yet — see
"What this does not do".

## Using it

Comment on a pull request, as the entire first line of the comment:

```
/review
```

You should see a 👀 reaction within a few seconds (the command was received),
then either a status comment on the PR (dispatch ran) or a 👎 reaction and a
failed workflow run (you are not authorized).

`/review` must be the whole first line. A comment that mentions `/review` in
prose does not trigger anything — this is intentional, so that discussing the
command in a thread does not fire it.

There is also a manual path: **Actions → PR review dispatch → Run workflow**,
which takes a PR number. Authorization is checked identically there.

## Who can run it

Anyone with **write**, **maintain**, or **admin** permission on this
repository. Read-only collaborators, triage collaborators, and drive-by
contributors cannot, regardless of how GitHub labels them in the comment UI.

Permission is resolved at dispatch time against the GitHub API
(`GET /repos/{owner}/{repo}/collaborators/{user}/permission`), not from the
comment event's `author_association` field — that field describes a social
relationship, not access, and would let any member of the owning org run the
command. `.github/scripts/authorize.sh` explains this at length.

Every failure path denies. If the API lookup errors out, the dispatch is
refused rather than allowed.

An unauthorized attempt gets a 👎 reaction and a failed run — no reply
comment. That is deliberate: replying would let anyone with a GitHub account
make the repo's bot post text on demand.

## What this does not do

It does not review anything. `.github/scripts/review-engine.sh` is an
explicitly unimplemented stub. Running `/review` today authorizes, dispatches,
reaches the stub, and posts a comment saying no review engine is configured.
That comment is the honest terminal state, not a bug.

Implementing the engine is a follow-up PR. The contract it must satisfy —
available inputs, required outputs, exit-status meaning, and the security
constraints of running in a privileged context — is documented in that
script's header. The workflow YAML should not need to change.

## If you are editing this

`.github/workflows/pr-review-dispatch.yml` runs in a **privileged** context:
`issue_comment` workflows execute from the default branch with a repo-scoped
token. That is what makes the authorization check meaningful, and it is why
the workflow never checks out PR head code. Keep it that way. The engine may
read the PR diff as data via the API; it must not execute it.
