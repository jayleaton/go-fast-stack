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
dev: ## Run the server (0.0.0.0, PORT or 3000) with .env (USE_DOPPLER=1: secrets from Doppler's dev config)
	$(if $(filter 1,$(USE_DOPPLER)),doppler run --) $(GO) run ./apps/web/cmd/server serve

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

.PHONY: check-migrations
check-migrations: ## Fail if a new migration is numbered at or below the highest on origin/dev or origin/main (check-fast and CI run it too)
	./scripts/check-migration-order.sh

.PHONY: lint-layering
lint-layering: ## Prove forbidden imports (code and tests) fail lint
	./scripts/check-layering.sh

.PHONY: lint-tests
lint-tests: ## Prove misplaced test files, coverage drops and slow tests fail (PROFILE=<coverprofile> reuses a test run)
	./scripts/check-test-probes.sh

# go test -json through scripts/testbudget: plain go test output, the slowest
# tests, and a failure when a package or test is over its time budget.
GOTEST := set -o pipefail; $(GO) test -race -json -coverprofile=.cache/coverage.out ./... | $(GO) run ./scripts/testbudget

.PHONY: test
test: ## go test -race against this checkout's throwaway Postgres (or TEST_DATABASE_URL), then time budgets and coverage floors
	@mkdir -p .cache
ifdef TEST_DATABASE_URL
	bash -c '$(GOTEST)'
else
	@$(MAKE) --no-print-directory test-db-up
	@port=$$($(COMPOSE) port postgres-test 5432 | sed 's/.*://'); \
	  echo "test db: 127.0.0.1:$$port (project $(COMPOSE_PROJECT_NAME))"; \
	  TEST_DATABASE_URL="postgres://postgres:postgres@127.0.0.1:$$port/postgres?sslmode=disable" bash -c '$(GOTEST)'
endif
	./scripts/check-coverage.sh .cache/coverage.out

.PHONY: ci
ci: ## Run the same checks as CI
	$(GO) build ./...
	$(MAKE) check-generated
	$(MAKE) lint test
	@# Alone: their temporary probe files would leak into the other checks under make -j.
	$(MAKE) lint-layering
	$(MAKE) lint-tests PROFILE=.cache/coverage.out # the probes reuse make test's profile
	$(MAKE) deploy-check

.PHONY: check-fast
check-fast: ## Before every push: gofmt, vet, golangci-lint, plain tests for only what your changes vs origin/dev (else origin/main) affect (seconds)
	./scripts/check-fast.sh

.PHONY: release-smoke
release-smoke: db-up ## Build every Dockerfile's binary as its image does (fail if dynamic), migrate, boot web + admin in production mode (CI runs it too)
	@$(COMPOSE) exec -T postgres sh -c 'for database in smoke_app smoke_admin; do dropdb -U postgres --if-exists --force $$database && createdb -U postgres $$database; done'
	SMOKE_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:$(POSTGRES_PORT)/smoke_app?sslmode=disable \
	  SMOKE_ADMIN_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:$(POSTGRES_PORT)/smoke_admin?sslmode=disable \
	  ./scripts/release-smoke.sh

.PHONY: hooks
hooks: ## Install a git pre-push hook that runs make check-fast (all worktrees; make hooks-remove to undo)
	./scripts/install-hooks.sh

.PHONY: hooks-remove
hooks-remove: ## Remove the pre-push hook make hooks installed
	./scripts/install-hooks.sh --uninstall

.PHONY: affected
affected: ## Show what CI checks for this branch (packages affected vs origin/dev, else origin/main)
	./scripts/affected.sh

.PHONY: bench-devloop
bench-devloop: ## Print dev-loop timings: build, generate, lint, probes, tests per package (BENCH_COLD=1: also cold caches)
ifdef TEST_DATABASE_URL
	./scripts/bench-devloop.sh
else
	@$(MAKE) --no-print-directory test-db-up
	@port=$$($(COMPOSE) port postgres-test 5432 | sed 's/.*://'); \
	  TEST_DATABASE_URL="postgres://postgres:postgres@127.0.0.1:$$port/postgres?sslmode=disable" ./scripts/bench-devloop.sh
endif

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

# Backups (README "Backups and restore"): the backup job in a
# postgres:18-alpine container on this checkout's compose network, so its
# pg_dump matches the server on every OS. Encrypted with BACKUP_ENCRYPTION_KEY
# (.env); stored in BACKUP_DIR, or in the S3 bucket BACKUP_S3_* names in .env.
BACKUP_DIR      ?= .backups
BACKUP_NETWORK  := $(COMPOSE_PROJECT_NAME)_default
BACKUP_DB       := postgres://postgres:postgres@postgres:5432
backup_job       = CGO_ENABLED=0 GOOS=linux GOARCH="$$(docker version -f '{{.Server.Arch}}')" $(GO) build -trimpath -o bin/linux/backup ./apps/backup/cmd/backup && \
	mkdir -p '$(BACKUP_DIR)' && docker run --rm --network '$(BACKUP_NETWORK)' --user "$$(id -u):$$(id -g)" \
	  -v '$(CURDIR)/bin/linux/backup:/usr/local/bin/backup:ro' -v '$(abspath $(BACKUP_DIR)):/backups' \
	  -e DATABASE_URL='$(BACKUP_DB)/app?sslmode=disable' $(1) $(if $(BACKUP_ADMIN),-e ADMIN_DATABASE_URL='$(BACKUP_DB)/admin?sslmode=disable') \
	  -e BACKUP_ENCRYPTION_KEY -e BACKUP_ENCRYPTION_KEY_PREVIOUS -e BACKUP_KEEP_DAILY -e BACKUP_KEEP_WEEKLY \
	  $(if $(BACKUP_S3_ENDPOINT),-e BACKUP_S3_ENDPOINT -e BACKUP_S3_BUCKET -e BACKUP_S3_REGION -e BACKUP_S3_PATH_STYLE -e BACKUP_S3_ACCESS_KEY_ID -e BACKUP_S3_SECRET_ACCESS_KEY,-e BACKUP_DIR=/backups) \
	  postgres:18-alpine backup

.PHONY: db-backup
db-backup: ## Back up this checkout's Postgres (encrypted pg_dump; BACKUP_ADMIN=1 adds the admin database), then prune
	$(call backup_job) run

.PHONY: db-backups
db-backups: ## List backups, newest first
	$(call backup_job) list

.PHONY: db-restore
# ID, YES and FORCE reach the recipe through the environment (export above),
# never spliced into the shell text; YES/FORCE count only as exactly 1.
db-restore: ## Restore ID=<backup id> into this checkout's database it came from: YES=1 required, FORCE=1 replaces a non-empty one
	@test -n "$$ID" || (echo 'usage: make db-restore ID=app/<time>.dump.age YES=1 [FORCE=1] (ids: make db-backups)' && exit 2)
	db="$${ID%%/*}"; case "$$db" in app|admin) ;; *) echo "not a backup id: $$ID"; exit 2;; esac; \
	$(call backup_job,-e RESTORE_DATABASE_URL="$(BACKUP_DB)/$$db?sslmode=disable") \
	  restore "$$ID" $(if $(filter 1,$(YES)),--yes) $(if $(filter 1,$(FORCE)),--force)

.PHONY: test-db-up
test-db-up:
	$(COMPOSE) --profile test up -d --wait --quiet-pull postgres-test

.PHONY: docker
docker: ## Build the production image
	docker build -t go-fast-stack .

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

.PHONY: admin-reset-2fa
admin-reset-2fa: ## Remove an admin's 2FA when nobody else can (audited): make admin-reset-2fa EMAIL=you@example.com
	@test -n "$(EMAIL)" || (echo 'usage: make admin-reset-2fa EMAIL=you@example.com' && exit 2)
	$(GO) run ./apps/admin/cmd/admin reset-2fa $(EMAIL)

.PHONY: admin-build
admin-build: ## Build the admin static binary into bin/admin
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags='-s -w' -o $(ADMIN_BIN) ./apps/admin/cmd/admin

.PHONY: admin-docker
admin-docker: ## Build the admin production image
	docker build -f apps/admin/Dockerfile -t go-fast-stack-admin .

.PHONY: actionlint
actionlint: ## Lint GitHub workflows
	$(GO) tool -modfile=tools/actionlint/go.mod actionlint

# Railway: deploy/platform.yaml decides the project; deploy/ is its own Go
# module (Railway IaC SDK), so none of it is in the app's go.mod or binary.
# Never name a variable here RAILWAY_*: make exports it to the Railway CLI,
# and RAILWAY_ENV=staging points the CLI at Railway's own staging API.
PLATFORM_FILE ?= platform.yaml
# Built, not go run: go run turns every failure into exit 1, and scripts and
# agents read railway-setup's exit codes (AGENTS.md). ARGS passes flags:
# make railway-setup ARGS="--yes --json --workspace acme".
DEPLOY_SETUP    := cd deploy && $(GO) build -o ../bin/railway-setup ./cmd/railway-setup && ../bin/railway-setup

.PHONY: railway-doctor
railway-doctor: ## Check everything Railway setup needs (login, workspace, SSH key, GitHub, Doppler) and print fixes
	@$(DEPLOY_SETUP) doctor -config $(PLATFORM_FILE) $(ARGS)

.PHONY: railway-setup
railway-setup: railway-apply ## From nothing to a live Railway project (same as railway-apply)

.PHONY: doppler-setup
doppler-setup: ## Move the Railway app secrets into Doppler: configs, seeded values, sealed read-only tokens (also run by railway-setup)
	@$(DEPLOY_SETUP) doppler -config $(PLATFORM_FILE) $(ARGS)

.PHONY: railway-plan
railway-plan: ## Show what railway-apply would change on Railway (read-only; exit 2 when changes are pending)
	@$(DEPLOY_SETUP) plan -config $(PLATFORM_FILE) $(ARGS)

.PHONY: railway-apply
railway-apply: ## Create/update the Railway project from deploy/platform.yaml, deploy, wait until healthy (CONFIRM_DESTRUCTIVE=1 allows deletes)
	@$(DEPLOY_SETUP) apply -config $(PLATFORM_FILE) $(if $(filter 1,$(CONFIRM_DESTRUCTIVE)),-confirm-destructive) $(ARGS)

.PHONY: railway-secrets
railway-secrets: ## Add Doppler tokens (or RESEND_API_KEY) to Railway, sealed
	@$(DEPLOY_SETUP) secrets -config $(PLATFORM_FILE) $(ARGS)

.PHONY: railway-up
railway-up: ## Upload this checkout to Railway and deploy it (PLATFORM_ENV=staging for one)
	@$(DEPLOY_SETUP) up -config $(PLATFORM_FILE) $(if $(PLATFORM_ENV),-env $(PLATFORM_ENV)) $(ARGS)

.PHONY: railway-rotate-db
railway-rotate-db: ## New Postgres superuser passwords on Railway, then redeploy (PLATFORM_ENV=staging for one)
	@$(DEPLOY_SETUP) rotate-db -config $(PLATFORM_FILE) $(if $(PLATFORM_ENV),-env $(PLATFORM_ENV)) $(ARGS)

.PHONY: railway-backups
railway-backups: ## List the Railway backups of PLATFORM_ENV (railway ssh)
	@$(DEPLOY_SETUP) backup -config $(PLATFORM_FILE) -env '$(PLATFORM_ENV)' $(ARGS) list

.PHONY: railway-backup
railway-backup: ## Back up PLATFORM_ENV's Railway databases now (the cron does it nightly)
	@$(DEPLOY_SETUP) backup -config $(PLATFORM_FILE) -env '$(PLATFORM_ENV)' $(ARGS) run

.PHONY: railway-restore
railway-restore: ## Restore ID=<backup id> into PLATFORM_ENV's database it came from (railway ssh): YES=1 required, FORCE=1 replaces a non-empty one
	@test -n "$$ID" || (echo 'usage: make railway-restore PLATFORM_ENV=production ID=app/<time>.dump.age YES=1 [FORCE=1] (ids: make railway-backups)' && exit 2)
	@$(DEPLOY_SETUP) backup -config $(PLATFORM_FILE) -env '$(PLATFORM_ENV)' $(ARGS) restore "$$ID" $(if $(filter 1,$(YES)),--yes) $(if $(filter 1,$(FORCE)),--force)

.PHONY: railway-verify
railway-verify: ## GET /healthz on every public Railway service
	@$(DEPLOY_SETUP) verify -config $(PLATFORM_FILE) $(ARGS)

.PHONY: deploy-check
deploy-check: ## vet and lint the deploy/ module (it has no tests)
	cd deploy && $(GO) vet ./... && (cd .railway && $(GO) vet railway.go) && $(GO) tool -modfile=../tools/go.mod golangci-lint run -c ../.golangci.yml ./...
