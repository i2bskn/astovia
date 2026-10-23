.DEFAULT_GOAL := help
COMPOSE_TEST := docker compose -f compose.test.yaml
WEB_SOURCES := $(shell find web/src web/public -type f) web/index.html web/package.json web/pnpm-lock.yaml web/tsconfig.json web/vite.config.ts

.PHONY: help env migrate up stop logs user-create user-reset-password user-disable test-web frontend test-go test check generate postgres build check-user-email

help: ## Show this list
	@awk 'BEGIN { FS = ":.*## " } /^[A-Za-z0-9_.\/-]+:.*## / { printf "  %-24s %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

env: ## Create .env from .env.example with a new JWT secret and your UID/GID (never overwrites)
	@if [ -e .env ] || [ -L .env ]; then echo '.env already exists; nothing changed'; exit 0; fi; \
	umask 077; \
	tmp=".env.tmp.$$$$"; \
	trap 'rm -f "$$tmp"' EXIT; \
	secret=$$(openssl rand -base64 32) || { echo 'openssl rand failed; .env was not created' >&2; exit 1; }; \
	if [ -z "$$secret" ]; then echo 'empty ASTOVIA_JWT_SECRET; .env was not created' >&2; exit 1; fi; \
	uid=$$(id -u) && gid=$$(id -g) || { echo 'id failed; .env was not created' >&2; exit 1; }; \
	ENV_SECRET="$$secret" ENV_UID="$$uid" ENV_GID="$$gid" awk ' \
		/^ASTOVIA_JWT_SECRET=/ { print "ASTOVIA_JWT_SECRET=" ENVIRON["ENV_SECRET"]; s++; next } \
		/^LOCAL_UID=/ { print "LOCAL_UID=" ENVIRON["ENV_UID"]; u++; next } \
		/^LOCAL_GID=/ { print "LOCAL_GID=" ENVIRON["ENV_GID"]; g++; next } \
		{ print } \
		END { if (s != 1 || u != 1 || g != 1) exit 1 }' .env.example > "$$tmp" \
		|| { echo 'could not fill .env.example; .env was not created' >&2; exit 1; }; \
	mv "$$tmp" .env; \
	echo 'Created .env'

migrate: ## Apply db/sqlite/schema.hcl to data/astovia.db with the Atlas container (asks for approval)
	mkdir -p data
	$(COMPOSE_TEST) run --rm atlas schema apply --url 'sqlite://data/astovia.db' --to 'file://db/sqlite/schema.hcl' --dev-url 'sqlite://file?mode=memory'

up: ## Start the local development containers in the background
	docker compose up -d

stop: ## Stop the local development containers
	docker compose stop

logs: ## Follow the app and web logs
	docker compose logs -f app web

# USER_EMAIL must be given on the make command line; a value inherited from the
# shell environment is rejected so that a stale variable cannot pick the user.
# The value is read from the recipe environment, not expanded into the shell
# script, so quotes in it cannot break the check. The check only keeps out
# characters that would break the shell quoting of the docker compose command
# below; the address format itself is validated by the CLI (auth.Email).
# Make's own expansion of the value (for example $(shell ...)) is input from
# the operator and is outside the scope of this shell check.
check-user-email:
	@if [ "$(origin USER_EMAIL)" != "command line" ] || [ -z "$${USER_EMAIL:-}" ]; then \
		echo 'USER_EMAIL is required (e.g. make user-create USER_EMAIL=alice@example.com)' >&2; exit 1; \
	fi; \
	case "$$USER_EMAIL" in \
		*[!A-Za-z0-9._%+@-]*) echo 'invalid USER_EMAIL' >&2; exit 1 ;; \
	esac; \
	case "$$USER_EMAIL" in \
		*@*) ;; \
		*) echo 'invalid USER_EMAIL' >&2; exit 1 ;; \
	esac; \
	if [ "$${#USER_EMAIL}" -lt 3 ] || [ "$${#USER_EMAIL}" -gt 254 ]; then echo 'invalid USER_EMAIL' >&2; exit 1; fi

user-create: check-user-email ## Create a user from the current source (make user-create USER_EMAIL=alice@example.com)
	docker compose run --rm app go run ./cmd/astovia users create --email '$(USER_EMAIL)'

user-reset-password: check-user-email ## Reset a user's password (make user-reset-password USER_EMAIL=alice@example.com)
	docker compose run --rm app go run ./cmd/astovia users reset-password --email '$(USER_EMAIL)'

user-disable: check-user-email ## Disable a user (make user-disable USER_EMAIL=alice@example.com)
	docker compose run --rm app go run ./cmd/astovia users disable --email '$(USER_EMAIL)'

web/dist/index.html: $(WEB_SOURCES) ## Build the embedded UI only when its sources changed
	$(COMPOSE_TEST) run --rm node sh -c 'corepack pnpm install --frozen-lockfile && corepack pnpm build'

test-web: ## Install, run the frontend auth tests and build the UI
	$(COMPOSE_TEST) run --rm node sh -c 'corepack pnpm install --frozen-lockfile && corepack pnpm test:auth && corepack pnpm build'

frontend: test-web ## Alias of test-web

postgres: ## Start the test PostgreSQL
	$(COMPOSE_TEST) up -d --wait postgres

test-go: web/dist/index.html postgres ## Run the Go tests against SQLite and PostgreSQL (-race -v)
	$(COMPOSE_TEST) run --rm go go test -race -count=1 -v ./...

test: ## Run test-web, then test-go
	$(MAKE) test-web && $(MAKE) test-go

check: test ## Run all tests and go vet
	$(COMPOSE_TEST) run --rm go go vet ./...

generate: ## Regenerate schema.sql and the sqlc packages from the HCL
	sh scripts/generate.sh

build: web/dist/index.html ## Build bin/astovia with the host Go (no tests)
	go build -o bin/astovia ./cmd/astovia
