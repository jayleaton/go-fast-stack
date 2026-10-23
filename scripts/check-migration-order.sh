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

readonly exit_misnumbered=1 # a migration needs a new number
readonly exit_usage=2        # a BASE git does not know

bases=("$@")
[ "${#bases[@]}" -gt 0 ] || bases=(origin/main)
for base in "${bases[@]}"; do
  if ! git rev-parse --verify --quiet "$base^{commit}" >/dev/null; then
    echo "check-migration-order: unknown base $base (git fetch origin, or pass a ref)" >&2
    exit "$exit_usage"
  fi
done
label=${bases[0]}
for base in "${bases[@]:1}"; do label+=" or $base"; done

migration_pattern='(^|/)migrations/[0-9]+_[^/]*\.sql$'
mapfile -t base_files < <(for base in "${bases[@]}"; do git ls-tree -r --name-only "$base"; done | grep -E "$migration_pattern" | sort -u || true)
mapfile -t tree_files < <(git ls-files --cached --others --exclude-standard | grep -E "$migration_pattern" | sort -u || true)

# version_prefix prints a migration's number as written ("00007").
version_prefix() {
  local name=${1##*/}
  echo "${name%%_*}"
}
# Per set (a migrations directory): the highest version on the bases, the
# width its numbers are written with, and the next free number.
declare -A on_base=() highest=() width=() next_free=() first_with=()
for file in "${base_files[@]}"; do
  [ -n "$file" ] || continue
  set_dir=${file%/*} prefix=$(version_prefix "$file")
  on_base[$file]=1 width[$set_dir]=${#prefix}
  if ((10#$prefix > ${highest[$set_dir]:-0})); then highest[$set_dir]=$((10#$prefix)); fi
done

# Added files, and per set the highest number among the bases and them.
added=()
for file in "${tree_files[@]}"; do
  [ -n "$file" ] && [ -e "$file" ] && [ -z "${on_base[$file]:-}" ] || continue
  set_dir=${file%/*} prefix=$(version_prefix "$file")
  added+=("$file")
  : "${width[$set_dir]:=${#prefix}}"
  top=${next_free[$set_dir]:-${highest[$set_dir]:-0}}
  if ((10#$prefix > top)); then top=$((10#$prefix)); fi
  next_free[$set_dir]=$top
done
for set_dir in "${!next_free[@]}"; do next_free[$set_dir]=$((next_free[$set_dir] + 1)); done

status=0
for file in "${added[@]}"; do
  set_dir=${file%/*} prefix=$(version_prefix "$file")
  version=$((10#$prefix)) set_highest=${highest[$set_dir]:-0}
  if ((version <= set_highest)); then
    printf '%s: version %d is not above the highest on %s (%d): renumber to %0*d\n' \
      "$file" "$version" "$label" "$set_highest" "${width[$set_dir]}" "${next_free[$set_dir]}" >&2
  elif [ -n "${first_with[$set_dir/$version]:-}" ]; then
    printf '%s: version %d is also used by %s: renumber to %0*d\n' \
      "$file" "$version" "${first_with[$set_dir/$version]}" "${width[$set_dir]}" "${next_free[$set_dir]}" >&2
  else
    first_with[$set_dir/$version]=$file
    continue
  fi
  status=$exit_misnumbered next_free[$set_dir]=$((next_free[$set_dir] + 1))
done
if [ "$status" -ne 0 ]; then
  echo "check-migration-order: number a new migration above every one on $label, per set (AGENTS.md \"Migrations\")" >&2
fi
exit "$status"
