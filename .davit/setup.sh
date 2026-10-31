#!/usr/bin/env bash
# The Davit project image (`davit box image build`): modules downloaded and
# the Go build, test and lint caches warmed for this tree, so a new box's
# first make lint / make test reuses them instead of compiling everything
# (docs/development.md, "Remote boxes"). Davit runs it from a copy, in the synced tree
# (the working directory). Rebuild the image (`davit box image update`) when
# go.mod or tools/go.mod moves on: a stale cache still helps, it warms less.
set -euo pipefail
go mod download
go mod download -modfile=tools/go.mod
go build ./...
go test -race -count=1 -run '^$' ./... >/dev/null # race-built test binaries, nothing run
# Fills the lint cache; findings are make lint's concern, not the image's.
go tool -modfile=tools/go.mod golangci-lint run ./... >/dev/null || true
go tool templ version >/dev/null # the generators, built once
go tool mockgen -version >/dev/null
go tool -modfile=tools/go.mod sqlc version >/dev/null
