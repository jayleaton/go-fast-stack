# Run `make help` for the list. Every target works from a fresh clone with Go
# (version in mise.toml) and Docker. Dev tools are pinned in tools/go.mod.

-include .env
export

GO        ?= go
TOOL      := $(GO) tool -modfile=tools/go.mod
COMPOSE   := docker compose
TEST_POSTGRES_PORT ?= 54329
TEST_DATABASE_URL  ?= postgres://postgres:postgres@127.0.0.1:$(TEST_POSTGRES_PORT)/postgres?sslmode=disable
BIN       := bin/server

.DEFAULT_GOAL := help

.env:
	cp .env.example .env

.PHONY: help
help: ## List targets
	@grep -hE '^[a-z-]+:.*## ' $(MAKEFILE_LIST) | awk -F ':.*## ' '{printf "  %-14s %s\n", $$1, $$2}'

.PHONY: dev
dev: generate ## Run the server (0.0.0.0, PORT or 3000) with .env
	$(GO) run ./apps/web/cmd/server serve

.PHONY: build
build: generate ## Build a static binary into bin/server
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags='-s -w' -o $(BIN) ./apps/web/cmd/server

.PHONY: generate
generate: ## Regenerate sqlc (and templ, once apps/web/ui exists)
	$(TOOL) sqlc generate -f packages/database/sqlc.yaml
	@if [ -d apps/web/ui ] && $(GO) tool -n templ >/dev/null 2>&1; then $(GO) tool templ generate -path apps/web/ui; fi

.PHONY: check-generated
check-generated: generate ## Fail if generated code is out of date
	@git diff --exit-code -- '*.go' || (echo 'generated code is stale: run make generate and commit' && exit 1)
	@test -z "$$(git status --porcelain --untracked-files=all -- '*.go')" || (git status --short -- '*.go'; echo 'untracked generated files: run make generate and commit'; exit 1)

.PHONY: lint
lint: ## gofmt + golangci-lint (incl. layering rules)
	@test -z "$$(gofmt -l .)" || (gofmt -l . && echo 'gofmt: run gofmt -w .' && exit 1)
	$(TOOL) golangci-lint run

.PHONY: lint-layering
lint-layering: ## Prove forbidden imports fail lint
	./scripts/check-layering.sh

.PHONY: test
test: test-db-up ## go test -race against a throwaway Postgres
	TEST_DATABASE_URL='$(TEST_DATABASE_URL)' $(GO) test -race ./...

.PHONY: ci
ci: ## Run the same checks as CI
	$(GO) build ./...
	$(MAKE) check-generated lint lint-layering test

.PHONY: migrate
migrate: ## Run auth then app migrations against DATABASE_URL
	$(GO) run ./apps/web/cmd/server migrate

.PHONY: db-up
db-up: ## Start Postgres + pgweb (127.0.0.1 only)
	$(COMPOSE) up -d --wait postgres pgweb

.PHONY: db-down
db-down: ## Stop Postgres + pgweb (keeps the data volume)
	$(COMPOSE) --profile test down

.PHONY: test-db-up
test-db-up:
	$(COMPOSE) --profile test up -d --wait postgres-test

.PHONY: docker
docker: ## Build the production image
	docker build -t ai-patterns-go .

.PHONY: actionlint
actionlint: ## Lint GitHub workflows
	$(GO) tool -modfile=tools/actionlint/go.mod actionlint
