#!/usr/bin/env bash
set -euo pipefail

# Wrapper around gh CLI that only allows specific subcommands and flags.
# All commands are scoped to the current repository via GH_REPO or GITHUB_REPOSITORY.
#
# Usage:
#   ./scripts/gh.sh issue view 123
#   ./scripts/gh.sh issue view 123 --comments
#   ./scripts/gh.sh issue list --state open --limit 20
#   ./scripts/gh.sh search issues "search query" --limit 10
#   ./scripts/gh.sh label list --limit 100
#   ./scripts/gh.sh issue comment 123 --body-file /tmp/comment.md
#   ./scripts/gh.sh pr review-comment 123 --body-file -

export GH_HOST=github.com

REPO="${GH_REPO:-${GITHUB_REPOSITORY:-}}"
if [[ -z "$REPO" || "$REPO" == */*/* || "$REPO" != */* ]]; then
  echo "Error: GH_REPO or GITHUB_REPOSITORY must be set to owner/repo format (e.g., GITHUB_REPOSITORY=anthropics/claude-code)" >&2
  exit 1
fi
export GH_REPO="$REPO"

AUTO_REVIEW_START_MARKER="<!-- cubesandbox-auto-review:start -->"
AUTO_REVIEW_END_MARKER="<!-- cubesandbox-auto-review:end -->"
AUTO_REVIEW_BOT_LOGIN="cubesandboxbot[bot]"

ALLOWED_FLAGS=(--comments --state --limit --label --body-file)
FLAGS_WITH_VALUES=(--state --limit --label --body-file)

SUB1="${1:-}"
SUB2="${2:-}"
CMD="$SUB1 $SUB2"
case "$CMD" in
  "issue view"|"issue list"|"search issues"|"label list"|"issue comment"|"pr review-comment")
    ;;
  *)
    echo "Error: only 'issue view', 'issue list', 'search issues', 'label list', 'issue comment', 'pr review-comment' are allowed (e.g., ./scripts/gh.sh issue view 123)" >&2
    exit 1
    ;;
esac

shift 2

# Separate flags from positional arguments
POSITIONAL=()
FLAGS=()
skip_next=false
for arg in "$@"; do
  if [[ "$skip_next" == true ]]; then
    FLAGS+=("$arg")
    skip_next=false
  elif [[ "$arg" == -* ]]; then
    flag="${arg%%=*}"
    matched=false
    for allowed in "${ALLOWED_FLAGS[@]}"; do
      if [[ "$flag" == "$allowed" ]]; then
        matched=true
        break
      fi
    done
    if [[ "$matched" == false ]]; then
      echo "Error: only --comments, --state, --limit, --label, --body-file flags are allowed (e.g., ./scripts/gh.sh issue list --state open --limit 20)" >&2
      exit 1
    fi
    FLAGS+=("$arg")
    # If flag expects a value and isn't using = syntax, skip next arg
    if [[ "$arg" != *=* ]]; then
      for vflag in "${FLAGS_WITH_VALUES[@]}"; do
        if [[ "$flag" == "$vflag" ]]; then
          skip_next=true
          break
        fi
      done
    fi
  else
    POSITIONAL+=("$arg")
  fi
done

if [[ "$CMD" == "search issues" ]]; then
  QUERY="${POSITIONAL[0]:-}"
  QUERY_LOWER=$(echo "$QUERY" | tr '[:upper:]' '[:lower:]')
  if [[ "$QUERY_LOWER" == *"repo:"* || "$QUERY_LOWER" == *"org:"* || "$QUERY_LOWER" == *"user:"* ]]; then
    echo "Error: search query must not contain repo:, org:, or user: qualifiers (e.g., ./scripts/gh.sh search issues \"bug report\" --limit 10)" >&2
    exit 1
  fi
  gh "$SUB1" "$SUB2" "$QUERY" --repo "$REPO" "${FLAGS[@]}"
elif [[ "$CMD" == "issue view" ]]; then
  if [[ ${#POSITIONAL[@]} -ne 1 ]] || ! [[ "${POSITIONAL[0]}" =~ ^[0-9]+$ ]]; then
    echo "Error: issue view requires exactly one numeric issue number (e.g., ./scripts/gh.sh issue view 123)" >&2
    exit 1
  fi
  gh "$SUB1" "$SUB2" "${POSITIONAL[0]}" "${FLAGS[@]}"
elif [[ "$CMD" == "issue comment" ]]; then
  if [[ ${#POSITIONAL[@]} -ne 1 ]] || ! [[ "${POSITIONAL[0]}" =~ ^[0-9]+$ ]]; then
    echo "Error: issue comment requires exactly one numeric issue number (e.g., ./scripts/gh.sh issue comment 123 --body-file /tmp/comment.md)" >&2
    exit 1
  fi
  has_body_file=false
  for f in "${FLAGS[@]}"; do
    if [[ "$f" == "--body-file" || "$f" == --body-file=* ]]; then
      has_body_file=true
      break
    fi
  done
  if [[ "$has_body_file" == false ]]; then
    echo "Error: issue comment requires --body-file <path> (e.g., ./scripts/gh.sh issue comment 123 --body-file /tmp/comment.md)" >&2
    exit 1
  fi
  gh "$SUB1" "$SUB2" "${POSITIONAL[0]}" "${FLAGS[@]}"
elif [[ "$CMD" == "pr review-comment" ]]; then
  if [[ ${#POSITIONAL[@]} -ne 1 ]] || ! [[ "${POSITIONAL[0]}" =~ ^[0-9]+$ ]]; then
    echo "Error: pr review-comment requires exactly one numeric PR number (e.g., ./scripts/gh.sh pr review-comment 123 --body-file -)" >&2
    exit 1
  fi

  pr_number="${POSITIONAL[0]}"
  body_source=""
  i=0
  while [[ $i -lt ${#FLAGS[@]} ]]; do
    f="${FLAGS[$i]}"
    if [[ "$f" == "--body-file" ]]; then
      i=$((i + 1))
      body_source="${FLAGS[$i]:-}"
    elif [[ "$f" == --body-file=* ]]; then
      body_source="${f#--body-file=}"
    else
      echo "Error: pr review-comment only accepts --body-file -" >&2
      exit 1
    fi
    i=$((i + 1))
  done

  if [[ "$body_source" != "-" ]]; then
    echo "Error: pr review-comment only accepts review body from stdin via --body-file -" >&2
    exit 1
  fi

  body="$(cat)"

  if [[ "${GITHUB_ACTIONS:-}" == "true" ]]; then
    current_pr="$(
      python3 - <<'PY'
import json
import os

event_path = os.environ.get("GITHUB_EVENT_PATH")
if not event_path:
    print("")
    raise SystemExit

with open(event_path, encoding="utf-8") as f:
    payload = json.load(f)

print(payload.get("pull_request", {}).get("number", ""))
PY
    )"
    if [[ -z "$current_pr" || "$pr_number" != "$current_pr" ]]; then
      echo "Error: pr review-comment may only update the current pull_request event PR" >&2
      exit 1
    fi
  fi

  if [[ "$body" != *"$AUTO_REVIEW_START_MARKER"* || "$body" != *"$AUTO_REVIEW_END_MARKER"* ]]; then
    echo "Error: review comment body must contain the auto-review start and end markers" >&2
    exit 1
  fi

  comments_file="$(mktemp)"
  payload_file="$(mktemp)"
  trap 'rm -f "$comments_file" "$payload_file"' EXIT

  gh api "repos/$REPO/issues/$pr_number/comments?per_page=100" --paginate --slurp >"$comments_file"
  comment_id="$(
    python3 - "$comments_file" "$AUTO_REVIEW_START_MARKER" "$AUTO_REVIEW_BOT_LOGIN" <<'PY'
import json
import sys

comments_path = sys.argv[1]
marker = sys.argv[2]
bot_login = sys.argv[3]

with open(comments_path, encoding="utf-8") as f:
    pages = json.load(f)

matches = []

for page in pages:
    for comment in page:
        user = comment.get("user") or {}
        body = comment.get("body") or ""
        if (
            user.get("type") == "Bot"
            and user.get("login") == bot_login
            and marker in body
        ):
            matches.append(comment)

if matches:
    print(matches[-1]["id"])
PY
  )"

  printf '%s' "$body" | python3 -c 'import json, sys; body = sys.stdin.read(); open(sys.argv[1], "w", encoding="utf-8").write(json.dumps({"body": body}))' "$payload_file"

  if [[ -n "$comment_id" ]]; then
    gh api --method PATCH "repos/$REPO/issues/comments/$comment_id" --input "$payload_file" >/dev/null
  else
    gh api --method POST "repos/$REPO/issues/$pr_number/comments" --input "$payload_file" >/dev/null
  fi
else
  if [[ ${#POSITIONAL[@]} -ne 0 ]]; then
    echo "Error: issue list and label list do not accept positional arguments (e.g., ./scripts/gh.sh issue list --state open, ./scripts/gh.sh label list --limit 100)" >&2
    exit 1
  fi
  gh "$SUB1" "$SUB2" "${FLAGS[@]}"
fi
