.PHONY: run test build migrate migrate-down

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

build:
	go build -o bin/api ./cmd/api

migrate:
	@echo "Migrando $(DATABASE_HOST):$(DATABASE_PORT)/$(DATABASE_NAME)"
	$(MIGRATE) up

migrate-down:
	@echo "Revirtiendo la ultima migracion en $(DATABASE_HOST):$(DATABASE_PORT)/$(DATABASE_NAME)"
	$(MIGRATE) down 1
