# syntax=docker/dockerfile:1
# The backup job (backups: in deploy/platform.yaml): a small Railway service
# that sleeps until BACKUP_SCHEDULE, then backs up and prunes. Not distroless
# like the apps: it needs pg_dump/pg_restore, and the ones from the server's
# own image always match its major version. Build from the repo root:
#   docker build -f apps/backup/Dockerfile .
# No RUN --mount=type=cache: Railway's builder rejects cache mounts without
# an id tied to one service ID, and the go.mod layer below caches modules.
FROM golang:1.26.8-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/backup ./apps/backup/cmd/backup

FROM postgres:18-alpine
COPY --from=build /out/backup /usr/local/bin/backup
ENV APP_ENV=production
# Each database's age scrypt step (packages/backup, work factor 18) allocates
# ~256 MiB that is garbage right after. Without a soft limit the GC lets the
# next database's allocation stack on it, and the 512 MiB container cap
# (deploy/platform/graph.go) kills the run on the second database. The limit
# makes the GC reclaim first; keep it below that cap.
ENV GOMEMLIMIT=384MiB
USER postgres
WORKDIR /tmp
# The postgres image's entrypoint would start a server: reset it. People run
# `backup list | run | restore` inside the service with `railway ssh`.
ENTRYPOINT []
CMD ["backup", "serve"]
