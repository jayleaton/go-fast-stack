#!/usr/bin/env bash
# Fails when a goose migration this checkout adds is numbered at or below the
# highest version a BASE already has in the same set. Goose refuses
# out-of-order migrations, so once a deployed database has applied 00007, a
# later-merged 00005 stops `migrate` (and every deploy) there:
#   missing (out-of-order) migration lower than database version (7): version 5
# The error names the fix: the number to renumber the file to.
#
# Usage: scripts/check-migration-order.sh [BASE...]   (default: origin/main)
#   Each BASE is a ref whose migrations count as deployed (its tip, not the
#   merge base, so a branch that BASE has moved past fails too). "Added" means
#   a migration in the working tree (committed, staged or untracked) that no
#   BASE has. A set is a directory named migrations holding NNNNN_*.sql files
#   (one goose version table each), so adding or deleting a set needs no change
#   here. Exit 0 ok, 1 a misnumbered migration, 2 an unknown BASE.
set -euo pipefail
# shellcheck disable=SC2046
unset $(git rev-parse --local-env-vars)
cd "$(dirname "$0")/.."

bases=("$@")
[ "${#bases[@]}" -gt 0 ] || bases=(origin/main)
for base in "${bases[@]}"; do
  if ! git rev-parse --verify --quiet "$base^{commit}" >/dev/null; then
    echo "check-migration-order: unknown base $base (git fetch origin, or pass a ref)" >&2
    exit 2
  fi
done
label=${bases[0]}
for base in "${bases[@]:1}"; do label+=" or $base"; done

migration='(^|/)migrations/[0-9]+_[^/]*\.sql$'
mapfile -t onbase < <(for base in "${bases[@]}"; do git ls-tree -r --name-only "$base"; done | grep -E "$migration" | sort -u || true)
mapfile -t here < <(git ls-files --cached --others --exclude-standard | grep -E "$migration" | sort -u || true)

prefix() { local n=${1##*/}; echo "${n%%_*}"; } # 00007
declare -A deployed=() highest=() width=() next=() seen=()
for f in "${onbase[@]}"; do
  [ -n "$f" ] || continue
  d=${f%/*} p=$(prefix "$f")
  deployed[$f]=1 width[$d]=${#p}
  if ((10#$p > ${highest[$d]:-0})); then highest[$d]=$((10#$p)); fi
done

# Added files, and per set the next free number above the bases and them.
added=()
for f in "${here[@]}"; do
  [ -n "$f" ] && [ -e "$f" ] && [ -z "${deployed[$f]:-}" ] || continue
  d=${f%/*} p=$(prefix "$f")
  added+=("$f")
  : "${width[$d]:=${#p}}"
  top=${next[$d]:-${highest[$d]:-0}}
  if ((10#$p > top)); then top=$((10#$p)); fi
  next[$d]=$top
done
for d in "${!next[@]}"; do next[$d]=$((next[$d] + 1)); done

status=0
for f in "${added[@]}"; do
  d=${f%/*} p=$(prefix "$f") h=${highest[$d]:-0}
  v=$((10#$p))
  if ((v <= h)); then
    printf '%s: version %d is not above the highest on %s (%d): renumber to %0*d\n' "$f" "$v" "$label" "$h" "${width[$d]}" "${next[$d]}" >&2
  elif [ -n "${seen[$d/$v]:-}" ]; then
    printf '%s: version %d is also used by %s: renumber to %0*d\n' "$f" "$v" "${seen[$d/$v]}" "${width[$d]}" "${next[$d]}" >&2
  else
    seen[$d/$v]=$f
    continue
  fi
  status=1 next[$d]=$((next[$d] + 1))
done
if [ "$status" -ne 0 ]; then
  echo "check-migration-order: number a new migration above every one on $label, per set (AGENTS.md \"Migrations\")" >&2
fi
exit "$status"
