.PHONY: build run db env test test-e2e lint boundaries up up-micro down demo keygen

build:
	go build -o bin/paygate ./cmd/paygate
	go build -o bin/edge ./cmd/edge

# Settings come from .env (create it with: make env). ENV_FILE=other.env to switch.
ENV_FILE ?= .env

env:
	@test -f .env && echo ".env already exists" || (cp .env.example .env && echo "created .env from .env.example: set DATABASE_URL")

run:
	@test -f $(ENV_FILE) || (echo "missing $(ENV_FILE): run 'make env' first" && exit 1)
	set -a && . ./$(ENV_FILE) && set +a && go run ./cmd/paygate

# Creates the database named in DATABASE_URL (tables are created by the app on start).
db:
	@test -f $(ENV_FILE) || (echo "missing $(ENV_FILE): run 'make env' first" && exit 1)
	set -a && . ./$(ENV_FILE) && set +a && \
	  name=$$(echo "$$DATABASE_URL" | sed -E 's#.*/([^/?]+)(\?.*)?$$#\1#') && \
	  psql "$$(echo "$$DATABASE_URL" | sed -E "s#/$$name(\?|$$)#/postgres\1#")" -c "CREATE DATABASE $$name" || true

test:
	go test -race ./...

# Requires a Postgres database it may wipe (and optionally NATS):
#   make test-e2e TEST_DATABASE_URL=postgres://postgres:postgres@localhost:5432/paygate_test?sslmode=disable
test-e2e:
	TEST_DATABASE_URL=$(TEST_DATABASE_URL) TEST_NATS_URL=$(TEST_NATS_URL) go test -race -count=1 -v ./internal/app/ ./internal/platform/bus/

lint:
	go vet ./...
	./scripts/check-boundaries.sh

boundaries:
	./scripts/check-boundaries.sh

up:
	docker compose --profile mono up --build

up-micro:
	docker compose --profile micro up --build

down:
	docker compose --profile mono --profile micro down -v

demo:
	ADMIN_EMAIL=admin@paygate.local ADMIN_PASSWORD=change-me-admin-password ./scripts/demo.sh

keygen:
	go run ./cmd/paygate keygen
