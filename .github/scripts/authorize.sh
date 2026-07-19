#!/usr/bin/env bash
#
# Authorization gate for the `/review` dispatch workflow. This is the
# security boundary of the whole feature: everything downstream assumes this
# script said yes.
#
# Required environment:
#   GH_TOKEN  token for the API call (the workflow's GITHUB_TOKEN)
#   REPO      "owner/name"
#   ACTOR     login of the user who triggered the dispatch
#
# Writes to $GITHUB_OUTPUT:
#   authorized  "true" only when ACTOR holds write/maintain/admin on REPO
#   permission  the resolved role, for the failure message
#
# WHY a live API call and not the event payload's `author_association`:
#
#   `author_association` is a *display* label describing the commenter's
#   social relationship to the repository, not an access-control decision.
#   It is wrong for this purpose in both directions:
#
#     - Too permissive: MEMBER means "member of the owning organization",
#       which says nothing about access to *this* repo — an org member with
#       no grant here still reports MEMBER. CONTRIBUTOR merely means "has a
#       merged commit", which a one-time drive-by contributor keeps forever.
#     - Too coarse: COLLABORATOR does not distinguish read/triage from
#       write, so it would hand the command to read-only collaborators.
#
#   It is also a snapshot taken when the comment was created, so a revoked
#   collaborator's old label does not change. The collaborator-permission
#   endpoint is evaluated at dispatch time against current access.
#
# Fail-closed: every non-affirmative path — API error, unexpected payload,
# unknown role — leaves `authorized=false`. There is no branch that defaults
# to allow.

set -euo pipefail

: "${GH_TOKEN:?GH_TOKEN must be set}"
: "${REPO:?REPO must be set (owner/name)}"
: "${ACTOR:?ACTOR must be set}"
: "${GITHUB_OUTPUT:?GITHUB_OUTPUT must be set}"

# ACTOR is interpolated into an API path. GitHub logins are alphanumeric with
# single hyphens, so anything else means the payload is not what we think it
# is — refuse rather than issue a request we cannot predict the meaning of.
if ! printf '%s' "$ACTOR" | grep -Eq '^[A-Za-z0-9][A-Za-z0-9-]{0,38}$'; then
  printf 'authorized=false\npermission=invalid-actor\n' >>"$GITHUB_OUTPUT"
  echo "Refusing to authorize: actor '${ACTOR}' is not a well-formed login." >&2
  exit 0
fi

# The legacy `.permission` field collapses roles onto read/write/admin, and
# it collapses them exactly where we want the line drawn: `maintain` reports
# as "write", `triage` reports as "read". So admin|write is precisely
# "write, maintain, or admin". `maintain` is accepted too in case the field
# ever stops collapsing.
if ! resolved=$(gh api "repos/${REPO}/collaborators/${ACTOR}/permission" \
                  --jq '.permission' 2>/dev/null); then
  # Includes the 404 returned for non-collaborators, and any transport or
  # token-scope failure. All of them are "not proven authorized".
  printf 'authorized=false\npermission=lookup-failed\n' >>"$GITHUB_OUTPUT"
  echo "Permission lookup for @${ACTOR} on ${REPO} failed or returned 404." >&2
  exit 0
fi

case "$resolved" in
  admin | maintain | write)
    authorized=true
    ;;
  *)
    authorized=false
    ;;
esac

printf 'authorized=%s\npermission=%s\n' "$authorized" "$resolved" >>"$GITHUB_OUTPUT"
echo "Actor @${ACTOR} resolved to '${resolved}' -> authorized=${authorized}"
