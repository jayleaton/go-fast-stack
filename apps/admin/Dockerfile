# syntax=docker/dockerfile:1
# The admin dashboard: its own static binary and image, deployed as its own
# Railway service (admin: true in deploy/platform.yaml). Build from the repo root:
#   docker build -f apps/admin/Dockerfile .
# No RUN --mount=type=cache: Railway's builder rejects cache mounts without
# an id tied to one service ID, and the go.mod layer below caches modules.
FROM golang:1.26.10-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/admin ./apps/admin/cmd/admin

FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=build /out/admin /admin
ENV APP_ENV=production PORT=3000
EXPOSE 3000
# No ENTRYPOINT: Railway replaces CMD with "/admin migrate" for pre-deploy.
CMD ["/admin", "serve"]
