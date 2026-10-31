#!/bin/sh
# Fails when a goose migration this checkout adds is numbered at or below the
# highest version a BASE already has in the same set. Goose refuses
# out-of-order migrations, so once a deployed database has applied 00007, a
# later-merged 00005 stops `migrate` (and every deploy) there:
#   missing (out-of-order) migration lower than database version (7): version 5
# The error names the fix: the number to renumber the file to.
#
# Usage: scripts/check-migration-order.sh [BASE...]
#   Each BASE is a ref whose migrations count as deployed (its tip, not the
#   merge base, so a branch that BASE has moved past fails too). Default: the
#   deployed branch (origin/main, scripts/branches.sh). "Added" means
#   a migration in the working tree (committed, staged or untracked) that no
#   BASE has. A set is a directory named migrations holding NNNNN_*.sql files
#   (one goose version table each), so adding or deleting a set needs no change
#   here. Exit 0 ok, 1 a misnumbered migration, 2 an unknown BASE.
#
# The check is `cilocal migration-order` (scripts/cilocal/verify/
# migrations.go, tested there), built rather than run so its exit code
# survives (go run turns every failure into 1). Plain sh: no bash 4 needed.
set -eu
# shellcheck disable=SC2046
unset $(git rev-parse --local-env-vars)
cd "$(dirname "$0")/.."
mkdir -p .cache
"${GO:-go}" build -o .cache/cilocal ./scripts/cilocal
exec .cache/cilocal migration-order "$@"
