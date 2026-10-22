# Run `make help` for the list. Every target works from a fresh clone with Go
# (version in mise.toml) and Docker. Dev tools are pinned in tools/go.mod.

-include .env
export

GO        ?= go
TOOL      := $(GO) tool -modfile=tools/go.mod
COMPOSE   := docker compose
BIN       := bin/server

# Every checkout (clone or git worktree) gets its own compose project and dev
# ports, derived from its path, so parallel worktrees never share containers.
# Override any of these in the environment or .env.
CHECKOUT_ID          := $(shell printf %s '$(CURDIR)' | cksum | cut -d' ' -f1)
SLOT                 := $(shell expr $(CHECKOUT_ID) % 1000)
COMPOSE_PROJECT_NAME ?= $(shell basename '$(CURDIR)' | tr 'A-Z' 'a-z' | tr -c 'a-z0-9\n' '-' | cut -c1-30)-$(shell printf %x $(CHECKOUT_ID) | cut -c1-6)
POSTGRES_PORT        ?= $(shell expr 25000 + $(SLOT))
PGWEB_PORT           ?= $(shell expr 26000 + $(SLOT))
DATABASE_URL         ?= postgres://postgres:postgres@127.0.0.1:$(POSTGRES_PORT)/app?sslmode=disable
# Empty: the test server gets a free port from Docker on every start.
TEST_POSTGRES_PORT   ?=

.DEFAULT_GOAL := help

.env:
	cp .env.example .env

.PHONY: help
help: ## List targets
	@grep -hE '^[a-z-]+:.*## ' $(MAKEFILE_LIST) | awk -F ':.*## ' '{printf "  %-14s %s\n", $$1, $$2}'

.PHONY: dev
dev: ## Run the server (0.0.0.0, PORT or 3000) with .env
	$(GO) run ./apps/web/cmd/server serve

.PHONY: build
build: ## Build a static binary into bin/server
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
test: ## go test -race against this checkout's throwaway Postgres (or TEST_DATABASE_URL)
ifdef TEST_DATABASE_URL
	$(GO) test -race ./...
else
	@$(MAKE) --no-print-directory test-db-up
	@port=$$($(COMPOSE) port postgres-test 5432 | sed 's/.*://'); \
	  echo "test db: 127.0.0.1:$$port (project $(COMPOSE_PROJECT_NAME))"; \
	  TEST_DATABASE_URL="postgres://postgres:postgres@127.0.0.1:$$port/postgres?sslmode=disable" $(GO) test -race ./...
endif

.PHONY: ci
ci: ## Run the same checks as CI
	$(GO) build ./...
	$(MAKE) check-generated
	$(MAKE) lint test
	@# Alone: its temporary probe packages would leak into the other checks under make -j.
	$(MAKE) lint-layering

.PHONY: migrate
migrate: ## Run auth then app migrations against DATABASE_URL
	$(GO) run ./apps/web/cmd/server migrate

.PHONY: env
env: ## Print this checkout's compose project, ports and DATABASE_URL
	@echo "compose project: $(COMPOSE_PROJECT_NAME)"
	@echo "postgres:        127.0.0.1:$(POSTGRES_PORT)  (DATABASE_URL=$(DATABASE_URL))"
	@echo "pgweb:           http://127.0.0.1:$(PGWEB_PORT)"
	@echo "test postgres:   127.0.0.1:$(if $(TEST_POSTGRES_PORT),$(TEST_POSTGRES_PORT),<free port per start>)"

.PHONY: db-up
db-up: env ## Start Postgres + pgweb (127.0.0.1 only)
	$(COMPOSE) up -d --wait postgres pgweb

.PHONY: db-down
db-down: ## Stop Postgres + pgweb (keeps the data volume)
	$(COMPOSE) --profile test down

.PHONY: test-db-up
test-db-up:
	$(COMPOSE) --profile test up -d --wait --quiet-pull postgres-test

.PHONY: docker
docker: ## Build the production image
	docker build -t ai-patterns-go .

.PHONY: actionlint
actionlint: ## Lint GitHub workflows
	$(GO) tool -modfile=tools/actionlint/go.mod actionlint
# scratch (do not merge)
