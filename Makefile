# Everything a reviewer needs, in one minute, with no credentials.
SHELL := /bin/bash
GO ?= go
# The database tests skip unless this is set; `make test-db` starts a throwaway server and sets it.
DSN_ENV := MIGRATION_TEST_DSN
PG_PORT ?= 15434
DSN ?= postgres://postgres:mkit@127.0.0.1:$(PG_PORT)/mkit?sslmode=disable

.PHONY: help build test test-db pg pg-stop lint fmt check

help:
	@grep -E '^[a-z-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "  %-10s %s\n", $$1, $$2}'

build: ## compile everything
	$(GO) build ./...

test: ## the unit tests; the database ones skip and say so
	$(GO) test ./...

test-db: pg ## every test, against a real PostgreSQL
	$(DSN_ENV)='$(DSN)' $(GO) test ./... -count=1

pg: ## start a throwaway PostgreSQL for the database tests
	@docker start mkit-test-pg 2>/dev/null || docker run -d --name mkit-test-pg -p $(PG_PORT):5432 \
		-e POSTGRES_PASSWORD=mkit -e POSTGRES_DB=mkit postgres:16-alpine >/dev/null
	@until docker exec mkit-test-pg pg_isready -U postgres -d mkit >/dev/null 2>&1; do sleep 0.3; done
	@echo "postgres ready on $(PG_PORT)"

pg-stop: ## remove the throwaway server
	@docker rm -f mkit-test-pg >/dev/null 2>&1 || true

lint: ## gofmt and vet
	@test -z "$$($(GO) fmt ./...)" && echo "gofmt: clean"
	$(GO) vet ./...

fmt: ## write gofmt changes
	$(GO) fmt ./... >/dev/null

check: lint test ## what CI runs

# The runtime module is built by the plugin builder image, not by the Go toolchain here.
runtime: ## build the Nakama plugin with the toolchain that built the server
	docker run --rm -v "$$(PWD)/runtime":/app -w /app --entrypoint sh heroiclabs/nakama-pluginbuilder:3.41.0 \
		-c "GOFLAGS=-mod=mod go build --trimpath --buildmode=plugin -o modules/backend.so ./cmd/server"

smoke: ## the runtime module against a real Nakama: idempotent, resumable, dry-run
	./scripts/runtime-smoke.sh

demo: ## the whole migration from nothing: 10k players, then a second run that writes nothing
	./scripts/demo.sh

demo-crash: ## a run killed mid-batch, and the run that finishes it
	./scripts/demo-crash.sh

image: ## build the image and check it runs as nonroot without a shell
	./scripts/image.sh

up: ## start the lab (database, fake title, Nakama) without running the import
	docker compose up -d --wait postgres faketenant nakama

down: ## stop the lab and remove its volumes
	docker compose down -v --remove-orphans
