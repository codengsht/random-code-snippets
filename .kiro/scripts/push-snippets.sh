#!/usr/bin/env bash
set -euo pipefail

REMOTE_NAME="${KIRO_SNIPPET_REMOTE_NAME:-origin99}"
REMOTE_URL="${KIRO_SNIPPET_REMOTE_URL:-git@github.com:codengsht/random-code-snippets.git}"
DATE_DIR="${KIRO_SNIPPET_DATE_DIR:-$(date +%F)}"
BRANCH_NAME="${KIRO_SNIPPET_BRANCH:-$(date +%m/%d/%y)}"
COMMIT_MESSAGE="${KIRO_SNIPPET_COMMIT_MESSAGE:-Export snippets for ${DATE_DIR}}"

if git rev-parse --show-toplevel >/dev/null 2>&1; then
  PROJECT_ROOT="$(git rev-parse --show-toplevel)"
  cd "$PROJECT_ROOT"
else
  PROJECT_ROOT="$(pwd)"
  git init >/dev/null
fi

if [ ! -d "$DATE_DIR" ]; then
  echo "No ${DATE_DIR} directory exists; nothing to push."
  exit 0
fi

if ! find "$DATE_DIR" -type f | grep -q .; then
  echo "No files found in ${DATE_DIR}; nothing to push."
  exit 0
fi

if git remote get-url "$REMOTE_NAME" >/dev/null 2>&1; then
  current_remote_url="$(git remote get-url "$REMOTE_NAME")"
  if [ "$current_remote_url" != "$REMOTE_URL" ]; then
    git remote set-url "$REMOTE_NAME" "$REMOTE_URL"
  fi
else
  git remote add "$REMOTE_NAME" "$REMOTE_URL"
fi

git_user_name="$(git config --get user.name || true)"
git_user_email="$(git config --get user.email || true)"

export GIT_AUTHOR_NAME="${GIT_AUTHOR_NAME:-${git_user_name:-Kiro Snippet Export}}"
export GIT_AUTHOR_EMAIL="${GIT_AUTHOR_EMAIL:-${git_user_email:-kiro-snippets@example.local}}"
export GIT_COMMITTER_NAME="${GIT_COMMITTER_NAME:-$GIT_AUTHOR_NAME}"
export GIT_COMMITTER_EMAIL="${GIT_COMMITTER_EMAIL:-$GIT_AUTHOR_EMAIL}"

remote_ref="refs/remotes/${REMOTE_NAME}/${BRANCH_NAME}"
git fetch "$REMOTE_NAME" "+refs/heads/${BRANCH_NAME}:${remote_ref}" >/dev/null 2>&1 || true

tmp_index="$(mktemp "${TMPDIR:-/tmp}/kiro-snippet-index.XXXXXX")"
trap 'rm -f "$tmp_index"' EXIT
export GIT_INDEX_FILE="$tmp_index"

if git show-ref --verify --quiet "$remote_ref"; then
  parent_commit="$(git rev-parse "$remote_ref")"
  parent_tree="$(git rev-parse "${remote_ref}^{tree}")"
  git read-tree "$parent_tree"
else
  parent_commit=""
  parent_tree="$(git hash-object -t tree /dev/null)"
  git read-tree --empty
fi

find "$DATE_DIR" -type f -print0 | xargs -0 git add --

new_tree="$(git write-tree)"
if [ "$new_tree" = "$parent_tree" ]; then
  echo "No snippet changes to push for ${DATE_DIR} on branch ${BRANCH_NAME}."
  exit 0
fi

commit_body="$(cat <<EOF
${COMMIT_MESSAGE}

Directory: ${DATE_DIR}
Source project: $(basename "$PROJECT_ROOT")
EOF
)"

if [ -n "$parent_commit" ]; then
  new_commit="$(printf '%s\n' "$commit_body" | git commit-tree "$new_tree" -p "$parent_commit")"
else
  new_commit="$(printf '%s\n' "$commit_body" | git commit-tree "$new_tree")"
fi

current_branch="$(git branch --show-current 2>/dev/null || true)"
if [ "$current_branch" != "$BRANCH_NAME" ]; then
  git update-ref "refs/heads/${BRANCH_NAME}" "$new_commit"
fi

git push "$REMOTE_NAME" "${new_commit}:refs/heads/${BRANCH_NAME}"

echo "Pushed ${DATE_DIR} to ${REMOTE_NAME}/${BRANCH_NAME}."
