# Makefile
#
# Purpose:  Developer shortcuts for all common MakeMyTrade operations.
#           Run 'make help' to see all available commands.
#
# Requires: Go 1.22+, Docker Desktop, psql (brew install libpq)

# ── Variables ─────────────────────────────────────────────────────────────────
# Load DB_URL from .env for psql/redis targets
include .env
export

BINARY_SERVER     := bin/server
BINARY_WORKER     := bin/worker
BINARY_SUPERVISOR := bin/supervisor
MIGRATIONS_DIR    := $(shell pwd)/migrations
GO_FILES          := $(shell find . -name '*.go' -not -path './vendor/*')

.PHONY: help up down down-v logs ps \
        start stop app-logs \
        dev dev-down \
        server worker build \
        psql redis temporal-ui \
        test lint tidy \
        new-migration

# ── Help ──────────────────────────────────────────────────────────────────────
help: ## Show all available make targets
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2}'

# ── Docker infrastructure ─────────────────────────────────────────────────────
up: ## Start all Docker services (Postgres, Redis, Temporal)
	docker compose up -d
	@echo "Services started. Temporal UI: http://localhost:8088"

down: ## Stop all Docker services (data preserved)
	docker compose down

down-v: ## Stop all Docker services AND delete all data volumes
	@echo "WARNING: This will delete all database data."
	@read -p "Are you sure? [y/N] " ans && [ "$$ans" = "y" ] || exit 1
	docker compose down -v --remove-orphans

logs: ## Tail logs for all services (Ctrl+C to stop)
	docker compose logs -f

logs-%: ## Tail logs for a specific service: make logs-postgres
	docker compose logs -f $*

ps: ## Show status of all Docker services
	docker compose ps

# ── Supervisor: production long-running mode ─────────────────────────────────
# make start   — builds supervisor + server + worker, then runs in foreground.
#                Ctrl-C (or: make stop) for clean shutdown.
# make stop    — sends SIGTERM to supervisor; it stops server + worker first.
# make app-logs — tail server and worker log files (not Docker logs).
start: ## Build and start the supervisor (manages server + worker; Ctrl-C to stop)
	@go build -o $(BINARY_SUPERVISOR) ./cmd/supervisor/
	@./$(BINARY_SUPERVISOR)

stop: ## Stop the supervisor (and all managed processes)
	@PID=$$(cat logs/supervisor.pid 2>/dev/null) && \
	  kill "$$PID" 2>/dev/null && echo "Stopped supervisor (PID $$PID)" || \
	  echo "Supervisor not running (no logs/supervisor.pid)"

app-logs: ## Tail server and worker log files
	@tail -f logs/server.log logs/worker.log

# ── Dev: start everything with one command ───────────────────────────────────
# make dev  — starts Docker services, then runs server + worker in parallel.
# Logs from all processes stream to the same terminal, prefixed by name.
# Ctrl+C stops everything cleanly.
#
# Requires: brew install gum  (for coloured log prefixes)
# Falls back to plain output if gum is not installed.
dev: up ## Start Docker services + server + worker (everything in one command)
	@echo "Waiting for Postgres to be ready..."
	@until /opt/homebrew/Cellar/libpq/18.3/bin/pg_isready -q -h localhost -p 5432 -U makemytrade; do sleep 1; done
	@echo "Starting server and worker..."
	@trap 'kill 0' SIGINT SIGTERM; \
	go run ./cmd/server/ 2>&1 | sed 's/^/[server] /' & \
	go run ./cmd/worker/ 2>&1 | sed 's/^/[worker] /' & \
	wait

dev-down: down ## Stop everything (Docker services + kills server/worker if running)
	@pkill -f 'go run ./cmd/server' 2>/dev/null || true
	@pkill -f 'go run ./cmd/worker' 2>/dev/null || true
	@echo "All services stopped."

# ── Go binaries ───────────────────────────────────────────────────────────────
build: ## Build both server and worker binaries into bin/
	@mkdir -p bin
	go build -o $(BINARY_SERVER) ./cmd/server/
	go build -o $(BINARY_WORKER) ./cmd/worker/
	@echo "Built: $(BINARY_SERVER) $(BINARY_WORKER)"

server: ## Run the HTTP server (loads .env automatically)
	go run ./cmd/server/

worker: ## Run the Temporal worker (loads .env automatically)
	go run ./cmd/worker/

# ── Database ──────────────────────────────────────────────────────────────────
psql: ## Open a psql shell connected to the MakeMyTrade database
	psql $(DB_URL)

# ── Redis ─────────────────────────────────────────────────────────────────────
redis: ## Open a redis-cli shell
	redis-cli -u $(REDIS_URL)

# ── Temporal ─────────────────────────────────────────────────────────────────
temporal-ui: ## Open Temporal Web UI in browser
	open http://localhost:8088

# ── Migrations ────────────────────────────────────────────────────────────────
# Usage: make new-migration name=add_risk_score_column
new-migration: ## Create a new migration file pair: make new-migration name=<name>
	@if [ -z "$(name)" ]; then echo "Usage: make new-migration name=<migration_name>"; exit 1; fi
	@NEXT=$$(printf "%06d" $$(ls migrations/*.up.sql 2>/dev/null | wc -l | xargs expr 1 +)); \
	touch migrations/$${NEXT}_$(name).up.sql; \
	touch migrations/$${NEXT}_$(name).down.sql; \
	echo "Created:"; \
	echo "  migrations/$${NEXT}_$(name).up.sql"; \
	echo "  migrations/$${NEXT}_$(name).down.sql"

# ── Testing ───────────────────────────────────────────────────────────────────
test: ## Run all tests
	go test ./... -v -count=1

test-short: ## Run tests excluding integration tests
	go test ./... -short -count=1

# ── Code quality ──────────────────────────────────────────────────────────────
tidy: ## Run go mod tidy to clean up dependencies
	go mod tidy

lint: ## Run golangci-lint (install: brew install golangci-lint)
	@which golangci-lint > /dev/null || (echo "Install: brew install golangci-lint" && exit 1)
	golangci-lint run ./...

# ── Watchlist management (before admin API is built) ─────────────────────────
# Usage: make add-symbol ticker=NVDA name="NVIDIA Corporation" sector=Technology type=stock
add-symbol: ## Add a symbol directly to DB: make add-symbol ticker=X name="Y" sector=Z type=stock
	@if [ -z "$(ticker)" ]; then echo "Usage: make add-symbol ticker=X name='Y' sector=Z type=stock"; exit 1; fi
	psql $(DB_URL) -c "INSERT INTO symbols (ticker, name, exchange, sector, symbol_type) \
		VALUES ('$(ticker)', '$(name)', 'NASDAQ', '$(sector)', '$(type)') \
		ON CONFLICT (ticker) DO UPDATE SET is_active = TRUE, name = EXCLUDED.name;"
	@echo "Symbol $(ticker) added/activated."

deactivate-symbol: ## Deactivate a symbol (data kept): make deactivate-symbol ticker=X
	@if [ -z "$(ticker)" ]; then echo "Usage: make deactivate-symbol ticker=X"; exit 1; fi
	psql $(DB_URL) -c "UPDATE symbols SET is_active = FALSE WHERE ticker = '$(ticker)';"
	@echo "Symbol $(ticker) deactivated. Data preserved."
