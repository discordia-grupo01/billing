.PHONY: run test test-integration build migrate migrate-down

-include .env
export

DATABASE_HOST ?= localhost
DATABASE_PORT ?= 5432
DATABASE_NAME ?= discordia_servicio
DATABASE_USER ?= discordia
DATABASE_PASSWORD ?= discordia
DATABASE_SSLMODE ?= disable

MIGRATE = docker run --rm -v $(shell pwd)/migrations:/migrations --net=host migrate/migrate \
	-path=/migrations \
	-database "postgres://$(DATABASE_USER):$(DATABASE_PASSWORD)@$(DATABASE_HOST):$(DATABASE_PORT)/$(DATABASE_NAME)?sslmode=$(DATABASE_SSLMODE)"

run:
	go run ./cmd/api

test:
	go test ./... -coverpkg=./internal/... -cover

# Levanta un Postgres descartable y corre todos los tests, incluidos los de
# integracion. Nunca usa la base del .env.
test-integration:
	@docker run -d --rm --name billing-test-pg -e POSTGRES_PASSWORD=test -e POSTGRES_DB=billing_test -p 55432:5432 postgres:16-alpine >/dev/null
	@until docker exec billing-test-pg pg_isready -U postgres >/dev/null 2>&1; do sleep 1; done; sleep 1
	@TEST_DATABASE_URL="postgres://postgres:test@localhost:55432/billing_test?sslmode=disable" \
		go test ./... -p 1 -count=1 -coverpkg=./internal/... -cover; \
		status=$$?; docker stop billing-test-pg >/dev/null; exit $$status

build:
	go build -o bin/api ./cmd/api

migrate:
	@echo "Migrando $(DATABASE_HOST):$(DATABASE_PORT)/$(DATABASE_NAME)"
	@$(MIGRATE) up

migrate-down:
	@echo "Revirtiendo la ultima migracion en $(DATABASE_HOST):$(DATABASE_PORT)/$(DATABASE_NAME)"
	@$(MIGRATE) down 1
