# syntax=docker/dockerfile:1
# Static binary on distroless: no shell, runs as nonroot. Generated code
# (sqlc, templ) is committed, so the build needs only the Go toolchain.
FROM golang:1.26.8-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/server ./apps/web/cmd/server

FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=build /out/server /server
ENV APP_ENV=production PORT=3000
EXPOSE 3000
# No ENTRYPOINT: Railway replaces CMD with "/server migrate" for pre-deploy.
CMD ["/server", "serve"]
