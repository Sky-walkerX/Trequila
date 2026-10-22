.DEFAULT_GOAL := help
COMPOSE ?= docker compose
# Stopping a stack must work even when .env is missing or stale, and compose
# refuses to interpolate the required secrets without values.
STOP := JWT_SECRET=x ENCRYPTION_KEY=x POSTGRES_PASSWORD=x $(COMPOSE)

.PHONY: help env up down reset logs psql build test vet fmt check

help: ## List the available targets
	@grep -E '^[a-z-]+:.*## ' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  %-8s %s\n", $$1, $$2}'

env: ## Write .env with freshly generated local secrets (never overwrites)
	@test ! -f .env || { echo ".env already exists; leaving it alone"; exit 0; }
	@printf 'JWT_SECRET=%s\nENCRYPTION_KEY=%s\nPOSTGRES_PASSWORD=%s\n' "$$(openssl rand -hex 32)" "$$(openssl rand -base64 32)" "$$(openssl rand -hex 16)" > .env
	@echo "wrote .env"

up: ## Build and start Postgres, NATS, Redis and the API
	$(COMPOSE) up --build -d
	@echo "API: http://localhost:8080  (make logs to follow, make down to stop)"

down: ## Stop the stack, keeping data
	$(STOP) down

reset: ## Stop the stack and delete its data volumes
	$(STOP) down -v

logs: ## Follow the API logs
	$(COMPOSE) logs -f api

psql: ## Open a psql shell on the local database
	$(COMPOSE) exec db psql -U trequila -d trequila

build: ## Compile every package
	go build ./...

test: ## Run the Go test suite
	go test ./...

vet: ## Run go vet
	go vet ./...

fmt: ## Fail if any file is not gofmt-formatted
	@test -z "$$(gofmt -l .)" || { gofmt -l .; exit 1; }

check: fmt vet test ## Everything CI runs
