#!/usr/bin/env bash
# Installs (or with --uninstall removes) a git pre-push hook that runs
# scripts/check-fast.sh in the checkout being pushed. Hooks live in the
# repository's shared git directory, so one install covers every linked
# worktree; a checkout without scripts/check-fast.sh (an older branch) skips
# the check. A pre-push hook that this script did not write is left alone.
# Skip the check for one push with `git push --no-verify`.
set -euo pipefail
cd "$(dirname "$0")/.."

marker='# installed by scripts/install-hooks.sh'
hooks=$(git rev-parse --path-format=absolute --git-path hooks)
hook=$hooks/pre-push

ours() { [ -f "$hook" ] && grep -qxF "$marker" "$hook"; }

if [ "${1:-}" = --uninstall ]; then
  if ours; then rm -f "$hook" && echo "removed $hook"; else echo "no pre-push hook of ours at $hook"; fi
  exit 0
fi
if [ -e "$hook" ] && ! ours; then
  echo "$hook exists and was not installed by this script; leaving it alone" >&2
  echo "chain it by hand: add scripts/check-fast.sh to it" >&2
  exit 1
fi

mkdir -p "$hooks"
cat >"$hook" <<EOF
#!/bin/sh
$marker
# Runs scripts/check-fast.sh (make check-fast) before every push from any
# checkout of this repository. Skip once: git push --no-verify.
top=\$(git rev-parse --show-toplevel) || exit 1
# Git exports repository env (GIT_DIR, GIT_INDEX_FILE, ...) to hooks; drop it
# so the check, and the git and go commands it runs, see only this checkout.
for v in \$(git rev-parse --local-env-vars); do unset "\$v"; done
cd "\$top" || exit 1
if [ ! -x scripts/check-fast.sh ]; then
  echo "pre-push: no scripts/check-fast.sh in this checkout; skipping the check"
  exit 0
fi
exec scripts/check-fast.sh </dev/null
EOF
chmod +x "$hook"
echo "installed $hook (runs make check-fast before every push; skip once with git push --no-verify)"
