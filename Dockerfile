# syntax=docker/dockerfile:1
# Static binary on distroless: no shell, runs as nonroot. Generated code
# (sqlc, templ) is committed, so the build needs only the Go toolchain.
# No RUN --mount=type=cache: Railway's builder rejects cache mounts without
# an id tied to one service ID, and the go.mod layer below caches modules.
FROM golang:1.26.8-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/server ./apps/web/cmd/server

FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=build /out/server /server
ENV APP_ENV=production PORT=3000
EXPOSE 3000
# No ENTRYPOINT: Railway replaces CMD with "/server migrate" for pre-deploy.
CMD ["/server", "serve"]
