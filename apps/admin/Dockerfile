# syntax=docker/dockerfile:1
# The admin dashboard: its own static binary and image, deployed as its own
# Railway service (apps/admin/railway.json). Build from the repo root:
#   docker build -f apps/admin/Dockerfile .
FROM golang:1.26.8-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/admin ./apps/admin/cmd/admin

FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=build /out/admin /admin
ENV APP_ENV=production PORT=3000
EXPOSE 3000
# No ENTRYPOINT: Railway replaces CMD with "/admin migrate" for pre-deploy.
CMD ["/admin", "serve"]
