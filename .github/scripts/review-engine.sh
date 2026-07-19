#!/usr/bin/env bash
#
# ===================================================================
# STUB — THE REVIEW ENGINE EXTENSION POINT. NOT IMPLEMENTED.
# ===================================================================
#
# This script is the single seam between the dispatch plumbing (trigger,
# authorization, acknowledgement, status reporting — all implemented and
# working) and an actual PR review implementation (not written). A future PR
# implements the engine *here*; nothing in the workflow YAML should need to
# change.
#
# It deliberately produces no findings. A stub that emitted plausible-looking
# output would be worse than no stub at all, because a maintainer could
# mistake placeholder text for a real review. If this runs unimplemented, the
# workflow says plainly that no engine is configured.
#
# -------------------------------------------------------------------
# CONTRACT FOR THE FUTURE IMPLEMENTATION
# -------------------------------------------------------------------
#
# Inputs available in the environment:
#   GH_TOKEN    repo-scoped token; the calling job grants pull-requests:write
#   REPO        "owner/name"
#   PR_NUMBER   numeric PR to review (already validated as numeric)
#
# Anything else the engine needs it must fetch itself via `gh api`, e.g.:
#   gh api "repos/${REPO}/pulls/${PR_NUMBER}"        PR metadata
#   gh api "repos/${REPO}/pulls/${PR_NUMBER}/files"  changed files + patches
#
# Required outputs (to $GITHUB_OUTPUT):
#   engine_configured=true    MUST be set when a real engine ran. The
#                             workflow's terminal-status step keys off this
#                             to decide between "review completed" and "no
#                             review engine is configured". Leaving it false
#                             while posting findings would report the run as
#                             a no-op.
#
# Exit status:
#   0        engine ran to completion (whether or not it found anything)
#   non-zero engine failed; the workflow reports failure and states that no
#            findings were produced. Do not exit 0 on a partial run.
#
# Output responsibility:
#   The engine owns its own findings output — a PR review, a review comment,
#   or a check run, as it sees fit. The workflow only posts a short terminal
#   status marker afterwards and will not format findings on the engine's
#   behalf.
#
# HARD CONSTRAINTS the implementation must respect:
#   - This job runs in a privileged context on the default branch. Do NOT
#     check out or execute PR head code here. Treat the diff, titles, and
#     comment bodies as inert untrusted data — never interpolate them into a
#     shell command line.
#   - Do not widen the calling job's `permissions:` block beyond what the
#     engine provably needs.
#   - Zero LLM API usage is a current project constraint, not an oversight.
#     Adding an AI provider here is a product decision, not an
#     implementation detail — raise it as an issue first (CONTRIBUTING.md).

set -euo pipefail

: "${REPO:?REPO must be set}"
: "${PR_NUMBER:?PR_NUMBER must be set}"
: "${GITHUB_OUTPUT:?GITHUB_OUTPUT must be set}"

echo "engine_configured=false" >>"$GITHUB_OUTPUT"

# Not an error: the dispatch path itself succeeded, and that is exactly what
# this workflow currently promises. Failing here would misreport working
# plumbing as broken.
cat <<EOF
No review engine is configured for ${REPO}.

Dispatch for PR #${PR_NUMBER} was authorized and reached the extension point
at .github/scripts/review-engine.sh, which is an unimplemented stub.

NO REVIEW WAS PERFORMED. NO FINDINGS WERE PRODUCED.
EOF
