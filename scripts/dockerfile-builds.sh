#!/usr/bin/env bash
# Prints how each Dockerfile builds its Go binary, read from the Dockerfile
# itself so nothing copies its flags. One line per tracked Dockerfile with a
# `RUN ... go build ...` in its build stage (before the second FROM), fields
# separated by the ASCII unit separator (0x1f: an empty field survives `read`,
# which collapses repeated tabs):
#   <dockerfile> <env> <run> <output> <package>
#   env      the build stage's ENV assignments (ENV GOFLAGS=-tags=x ...)
#   run      the whole RUN command (go build and any guard chained after it)
#   output   the -o path (/out/server)
#   package  the package built (./apps/web/cmd/server)
# Used by scripts/release-smoke.sh (build exactly like the image) and
# scripts/affected.sh (when a build input changed, the smoke runs).
set -euo pipefail
cd "$(dirname "$0")/.."

field_separator=$'\x1f'

git ls-files -- Dockerfile '*/Dockerfile' | while read -r dockerfile; do
  # Join continuation lines, keep the build stage only.
  stage=$(sed -e ':join' -e '/\\$/N; s/\\\n//; t join' "$dockerfile" | awk '/^FROM /{ stages++ } stages <= 1')
  run=$(sed -n 's/^RUN \(.*go build .*\)$/\1/p' <<<"$stage" | head -n 1)
  [ -n "$run" ] || continue
  env=$(sed -n 's/^ENV \(.*=.*\)$/\1/p' <<<"$stage" | tr '\n' ' ' | sed 's/ *$//')
  build=${run#*go build } && build=${build%%&&*}
  out=$(grep -oE -- '-o +[^ ]+' <<<"$build" | awk '{print $2}')
  pkg=$(grep -oE '(^| )\./[^ ]+' <<<"$build" | tail -n 1 | tr -d ' ')
  printf '%s\n' "$dockerfile$field_separator$env$field_separator$run$field_separator$out$field_separator$pkg"
done
