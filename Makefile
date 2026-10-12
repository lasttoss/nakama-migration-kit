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
