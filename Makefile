# Run `make help` for the list. Every target works from a fresh clone with Go
# (version in mise.toml) and Docker. Dev tools are pinned in tools/go.mod.

-include .env
export

GO        ?= go
TOOL      := $(GO) tool -modfile=tools/go.mod
COMPOSE   := docker compose
BIN       := bin/server
ADMIN_BIN := bin/admin

# Every checkout (clone or git worktree) gets its own compose project and dev
# ports, derived from its path, so parallel worktrees never share containers.
# Override any of these in the environment or .env.
CHECKOUT_ID          := $(shell printf %s '$(CURDIR)' | cksum | cut -d' ' -f1)
SLOT                 := $(shell expr $(CHECKOUT_ID) % 1000)
COMPOSE_PROJECT_NAME ?= $(shell basename '$(CURDIR)' | tr 'A-Z' 'a-z' | tr -c 'a-z0-9\n' '-' | cut -c1-30)-$(shell printf %x $(CHECKOUT_ID) | cut -c1-6)
POSTGRES_PORT        ?= $(shell expr 25000 + $(SLOT))
PGWEB_PORT           ?= $(shell expr 26000 + $(SLOT))
DATABASE_URL         ?= postgres://postgres:postgres@127.0.0.1:$(POSTGRES_PORT)/app?sslmode=disable
# The admin app's own database: a second database on the same dev server
# (created by db-up). Hosted, it is a separate small Postgres.
ADMIN_DATABASE_URL   ?= postgres://postgres:postgres@127.0.0.1:$(POSTGRES_PORT)/admin?sslmode=disable
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
generate: ## Regenerate sqlc, go:generate (mocks) and templ (shared uikit + web + admin UIs)
	$(TOOL) sqlc generate -f packages/database/sqlc.yaml
	$(GO) generate ./...
	@for ui in packages/uikit apps/web/ui apps/admin/ui; do [ -d $$ui ] && $(GO) tool templ generate -path $$ui || true; done

.PHONY: css
css: ## Rebuild the web and admin app.css (Tailwind standalone CLI, pinned + checksum-verified)
	"$$(apps/web/ui/scripts/tailwind.sh)" -i apps/web/ui/styles/input.css -o apps/web/ui/static/app.css --minify
	@if [ -d apps/admin/ui ]; then "$$(apps/web/ui/scripts/tailwind.sh)" -i apps/admin/ui/styles/input.css -o apps/admin/ui/static/app.css --minify; fi

.PHONY: check-generated
check-generated: generate ## Fail if generated code is out of date
	@git diff --exit-code -- '*.go' || (echo 'generated code is stale: run make generate and commit' && exit 1)
	@test -z "$$(git status --porcelain --untracked-files=all -- '*.go')" || (git status --short -- '*.go'; echo 'untracked generated files: run make generate and commit'; exit 1)

.PHONY: lint
lint: ## gofmt + golangci-lint (incl. layering rules) + test file locations
	@test -z "$$(gofmt -l .)" || (gofmt -l . && echo 'gofmt: run gofmt -w .' && exit 1)
	$(TOOL) golangci-lint run
	./scripts/check-test-boundaries.sh

.PHONY: lint-layering
lint-layering: ## Prove forbidden imports (code and tests) fail lint
	./scripts/check-layering.sh

.PHONY: lint-tests
lint-tests: ## Prove misplaced test files and coverage drops fail
	./scripts/check-test-probes.sh

.PHONY: test
test: ## go test -race against this checkout's throwaway Postgres (or TEST_DATABASE_URL), then coverage floors
	@mkdir -p .cache
ifdef TEST_DATABASE_URL
	$(GO) test -race -coverprofile=.cache/coverage.out ./...
else
	@$(MAKE) --no-print-directory test-db-up
	@port=$$($(COMPOSE) port postgres-test 5432 | sed 's/.*://'); \
	  echo "test db: 127.0.0.1:$$port (project $(COMPOSE_PROJECT_NAME))"; \
	  TEST_DATABASE_URL="postgres://postgres:postgres@127.0.0.1:$$port/postgres?sslmode=disable" $(GO) test -race -coverprofile=.cache/coverage.out ./...
endif
	./scripts/check-coverage.sh .cache/coverage.out

.PHONY: ci
ci: ## Run the same checks as CI
	$(GO) build ./...
	$(MAKE) check-generated
	$(MAKE) lint test
	@# Alone: their temporary probe files would leak into the other checks under make -j.
	$(MAKE) lint-layering
	$(MAKE) lint-tests
	$(MAKE) deploy-check

.PHONY: affected
affected: ## Show what CI checks for this branch (packages affected vs origin/main)
	./scripts/affected.sh

.PHONY: migrate
migrate: ## Run auth then app migrations against DATABASE_URL
	$(GO) run ./apps/web/cmd/server migrate

.PHONY: env
env: ## Print this checkout's compose project, ports and database URLs
	@echo "compose project: $(COMPOSE_PROJECT_NAME)"
	@echo "postgres:        127.0.0.1:$(POSTGRES_PORT)  (DATABASE_URL=$(DATABASE_URL))"
	@echo "admin database:  ADMIN_DATABASE_URL=$(ADMIN_DATABASE_URL)"
	@echo "pgweb:           http://127.0.0.1:$(PGWEB_PORT)"
	@echo "test postgres:   127.0.0.1:$(if $(TEST_POSTGRES_PORT),$(TEST_POSTGRES_PORT),<free port per start>)"

.PHONY: db-up
db-up: env ## Start Postgres + pgweb (127.0.0.1 only) and make sure the admin database exists
	$(COMPOSE) up -d --wait postgres pgweb
	@$(COMPOSE) exec -T postgres sh -c 'psql -U postgres -tAc "SELECT 1 FROM pg_database WHERE datname = '"'"'admin'"'"'" | grep -q 1 || createdb -U postgres admin'

.PHONY: db-down
db-down: ## Stop Postgres + pgweb (keeps the data volume)
	$(COMPOSE) --profile test down

.PHONY: test-db-up
test-db-up:
	$(COMPOSE) --profile test up -d --wait --quiet-pull postgres-test

.PHONY: docker
docker: ## Build the production image
	docker build -t ai-patterns-go .

# --- Admin app (optional; delete apps/admin and these targets to drop it) ---

.PHONY: admin-dev
admin-dev: db-up admin-migrate ## Run the admin dashboard (0.0.0.0, ADMIN_PORT or 3001) with .env
	$(GO) run ./apps/admin/cmd/admin serve

.PHONY: admin-migrate
admin-migrate: ## Migrate the admin database (auth + audit log) and seed ADMIN_BOOTSTRAP_EMAIL
	$(GO) run ./apps/admin/cmd/admin migrate

.PHONY: admin-invite
admin-invite: ## Send an admin set-password link: make admin-invite EMAIL=you@example.com
	@test -n "$(EMAIL)" || (echo 'usage: make admin-invite EMAIL=you@example.com' && exit 2)
	$(GO) run ./apps/admin/cmd/admin invite $(EMAIL)

.PHONY: admin-build
admin-build: ## Build the admin static binary into bin/admin
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags='-s -w' -o $(ADMIN_BIN) ./apps/admin/cmd/admin

.PHONY: admin-docker
admin-docker: ## Build the admin production image
	docker build -f apps/admin/Dockerfile -t ai-patterns-go-admin .

.PHONY: actionlint
actionlint: ## Lint GitHub workflows
	$(GO) tool -modfile=tools/actionlint/go.mod actionlint

# Railway: deploy/platform.yaml decides the project; deploy/ is its own Go
# module (Railway IaC SDK), so none of it is in the app's go.mod or binary.
# Never name a variable here RAILWAY_*: make exports it to the Railway CLI,
# and RAILWAY_ENV=staging points the CLI at Railway's own staging API.
PLATFORM_FILE ?= platform.yaml
DEPLOY_SETUP    := cd deploy && $(GO) run ./cmd/railway-setup

.PHONY: railway-plan
railway-plan: ## Show what railway-apply would change on Railway (read-only)
	$(DEPLOY_SETUP) plan -config $(PLATFORM_FILE)

.PHONY: railway-apply
railway-apply: ## Create/update the Railway project from deploy/platform.yaml (CONFIRM_DESTRUCTIVE=1 allows deletes)
	$(DEPLOY_SETUP) apply -config $(PLATFORM_FILE) $(if $(filter 1,$(CONFIRM_DESTRUCTIVE)),-confirm-destructive)

.PHONY: railway-secrets
railway-secrets: ## Add Doppler tokens (or RESEND_API_KEY) to Railway, sealed
	$(DEPLOY_SETUP) secrets -config $(PLATFORM_FILE)

.PHONY: railway-up
railway-up: ## Upload this checkout to Railway and deploy it (repo: "" in platform.yaml; PLATFORM_ENV=staging for one)
	$(DEPLOY_SETUP) up -config $(PLATFORM_FILE) $(if $(PLATFORM_ENV),-env $(PLATFORM_ENV))

.PHONY: railway-rotate-db
railway-rotate-db: ## New Postgres superuser passwords on Railway, then redeploy (PLATFORM_ENV=staging for one)
	$(DEPLOY_SETUP) rotate-db -config $(PLATFORM_FILE) $(if $(PLATFORM_ENV),-env $(PLATFORM_ENV))

.PHONY: railway-verify
railway-verify: ## GET /healthz on every public Railway service
	$(DEPLOY_SETUP) verify -config $(PLATFORM_FILE)

.PHONY: deploy-check
deploy-check: ## vet, lint and test the deploy/ module
	cd deploy && $(GO) vet ./... && (cd .railway && $(GO) vet railway.go) && $(GO) tool -modfile=../tools/go.mod golangci-lint run -c ../.golangci.yml ./... && $(GO) test ./...
