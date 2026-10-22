#!/usr/bin/env bash
# Decides what CI must check for a change: the Go packages whose own files
# changed plus every package whose code or tests import them, transitively
# (Go's package graph via `go list -test -deps`). Changes to shared inputs
# (go.mod/go.sum, tooling, workflows, lint config, migrations, generator
# config) mean a full run.
#
# Usage: scripts/affected.sh [<base>]   (default: merge-base with origin/main)
#        AFFECTED_FILES="a.go b.md" scripts/affected.sh   (explicit file list)
# Prints key=value lines (and appends them to $GITHUB_OUTPUT when set):
#   full=true|false     run everything (./...)
#   packages=<pkgs>     import paths to build/lint/test (empty: no Go changes)
#   dirs=<dirs>         the same packages as ./relative dirs (for golangci-lint)
#   sqlc=true|false     sqlc inputs or output changed
#   templ=true|false    .templ files or their output changed
#   generate=<dirs>     packages whose //go:generate inputs or outputs changed
set -euo pipefail
cd "$(dirname "$0")/.."

if [ -n "${AFFECTED_FILES:-}" ]; then
  read -r -a changed <<<"$AFFECTED_FILES"
else
  base=${1:-$(git merge-base origin/main HEAD)}
  mapfile -t changed < <(git diff --name-only --no-renames "$base" HEAD --)
fi

out() {
  printf '%s=%s\n' "$1" "$2"
  if [ -n "${GITHUB_OUTPUT:-}" ]; then printf '%s=%s\n' "$1" "$2" >>"$GITHUB_OUTPUT"; fi
}

# Shared inputs: anything here can change every package's result.
# AFFECTED_FULL=1 forces a full run (pushes to main, merge queue, manual runs).
full=false
[ -n "${AFFECTED_FULL:-}" ] && full=true && echo "full run: AFFECTED_FULL" >&2
[ "$full" = true ] || for f in "${changed[@]}"; do
  case $f in
  go.mod | go.sum | mise.toml | Makefile | .golangci.yml | tools/* | scripts/* | .github/* | \
    */migrations/* | packages/database/sqlc.yaml)
    full=true
    echo "full run: $f" >&2
    break
    ;;
  esac
done

if [ "$full" = true ]; then
  out full true
  out packages ./...
  out dirs ./...
  out sqlc true
  out templ true
  out generate ./...
  exit 0
fi

# sqlc inputs and outputs: every queries:/out: path in sqlc.yaml (schemas are
# migrations, which force a full run above).
sqlcdir=packages/database
mapfile -t sqlcpaths < <(sed -nE 's/^[[:space:]]*(queries|out):[[:space:]]*([^[:space:]#]+).*/\2/p' "$sqlcdir/sqlc.yaml" |
  while read -r p; do echo "$sqlcdir/${p%/}/"; done)
sqlc=false templ=false
for f in "${changed[@]}"; do
  for p in "${sqlcpaths[@]}"; do
    if [[ $f == "$p"* ]]; then sqlc=true; fi
  done
  case $f in
  *.templ | *_templ.go) templ=true ;;
  esac
done

mod=$(go list -m)
# file (repo-relative) -> package, for every file go list knows about.
filemap=$(go list -e -f '{{$d := .Dir}}{{$p := .ImportPath}}{{range .GoFiles}}{{$d}}/{{.}} {{$p}}
{{end}}{{range .CgoFiles}}{{$d}}/{{.}} {{$p}}
{{end}}{{range .TestGoFiles}}{{$d}}/{{.}} {{$p}}
{{end}}{{range .XTestGoFiles}}{{$d}}/{{.}} {{$p}}
{{end}}{{range .IgnoredGoFiles}}{{$d}}/{{.}} {{$p}}
{{end}}{{range .EmbedFiles}}{{$d}}/{{.}} {{$p}}
{{end}}{{range .TestEmbedFiles}}{{$d}}/{{.}} {{$p}}
{{end}}{{range .XTestEmbedFiles}}{{$d}}/{{.}} {{$p}}
{{end}}{{range .IgnoredOtherFiles}}{{$d}}/{{.}} {{$p}}
{{end}}' ./... | sed "s|^$PWD/||")
# package dir -> package, for files go list does not list (deleted files, testdata, ...).
dirmap=$(go list -e -f '{{.Dir}} {{.ImportPath}}' ./... | sed "s|^$PWD/||")

declare -A seeds=()
for f in "${changed[@]}"; do
  p=$(awk -v f="$f" '$1 == f { print $2; exit }' <<<"$filemap")
  if [ -z "$p" ]; then
    # Nearest enclosing package directory, if any (docs/, apps/mobile-app/ have none).
    d=$(dirname "$f")
    while [ "$d" != . ] && [ -z "$p" ]; do
      p=$(awk -v d="$d" '$1 == d { print $2; exit }' <<<"$dirmap")
      d=$(dirname "$d")
    done
  fi
  if [ -n "$p" ]; then seeds[$p]=1; fi
done

declare -A affected=()
if [ "${#seeds[@]}" -gt 0 ]; then
  # Each line: the package a (test) build belongs to, then everything it
  # compiles against, with test variants ("p [p.test]") and the generated
  # test main ("p.test") folded into their package.
  while read -r owner deps; do
    if [ -n "${seeds[$owner]:-}" ]; then affected[$owner]=1 && continue; fi
    for d in $deps; do
      if [ -n "${seeds[$d]:-}" ]; then affected[$owner]=1 && break; fi
    done
  done < <(go list -e -test -deps=false -f '{{.ImportPath}} {{join .Deps " "}}' ./... |
    sed -E 's/ \[[^]]*\]//g; s/^([^ ]*)_test /\1 /; s/^([^ ]*)\.test /\1 /' |
    awk -v m="$mod" '$1 == m || index($1, m "/") == 1')
fi

pkgs=$(printf '%s\n' "${!affected[@]}" | sed '/^$/d' | sort)
dirs=$(sed "s|^$mod|.|" <<<"$pkgs")

# go:generate: packages whose directive source or -destination output changed.
generate=""
while IFS=: read -r file _ directive; do
  [ -n "$file" ] || continue
  dir=$(dirname "$file")
  dest=$(grep -oE -- '-destination=[^ ]+' <<<"$directive" | cut -d= -f2)
  for f in "${changed[@]}"; do
    if [ "$f" = "$file" ] || { [ -n "$dest" ] && [ "$f" = "$dir/$dest" ]; }; then
      generate+="./$dir "
      break
    fi
  done
done < <(git grep -n '^//go:generate' -- '*.go' || true)

out full false
out packages "$(tr '\n' ' ' <<<"$pkgs" | sed 's/ *$//')"
out dirs "$(tr '\n' ' ' <<<"$dirs" | sed 's/ *$//')"
out sqlc "$sqlc"
out templ "$templ"
out generate "$(sed 's/ *$//' <<<"$generate")"
