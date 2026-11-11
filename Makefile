.PHONY: run test test-integration mutation build migrate openapi migrate-down republish-premium

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

# Levanta un Postgres y un RabbitMQ descartables y corre todos los tests,
# incluidos los de integracion. Nunca usa la base ni el broker del .env.
test-integration:
	@docker run -d --rm --name billing-test-pg -e POSTGRES_PASSWORD=test -e POSTGRES_DB=billing_test -p 55432:5432 postgres:16-alpine >/dev/null
	@docker run -d --rm --name billing-test-rabbit -p 55672:5672 rabbitmq:3-alpine >/dev/null
	@until docker exec billing-test-pg pg_isready -U postgres >/dev/null 2>&1; do sleep 1; done; sleep 1
	@until docker exec billing-test-rabbit rabbitmq-diagnostics -q ping >/dev/null 2>&1; do sleep 1; done
	@TEST_DATABASE_URL="postgres://postgres:test@localhost:55432/billing_test?sslmode=disable" \
		TEST_RABBITMQ_URL="amqp://guest:guest@localhost:55672/" \
		go test ./... -p 1 -count=1 -coverpkg=./internal/... -cover; \
		status=$$?; docker stop billing-test-pg billing-test-rabbit >/dev/null; exit $$status

# Mutation testing de internal/service con gremlins (ver docs/mutation-testing).
# Tarda alrededor de un minuto; no corre en el CI.
mutation:
	gremlins unleash ./internal/service --timeout-coefficient 50 --workers 6 \
		--invert-assignments --invert-logical --invert-loopctrl --remove-self-assignments \
		--invert-bitwise --invert-bwassign --output docs/mutation-testing/after.json

build:
	go build -o bin/api ./cmd/api

openapi:
	npx --yes swagger-cli@4.0.4 validate openapi.yaml

migrate:
	@echo "Migrando $(DATABASE_HOST):$(DATABASE_PORT)/$(DATABASE_NAME)"
	@$(MIGRATE) up

migrate-down:
	@echo "Revirtiendo la ultima migracion en $(DATABASE_HOST):$(DATABASE_PORT)/$(DATABASE_NAME)"
	@$(MIGRATE) down 1

# Vuelve a publicar el estado Premium de todos los usuarios, para los consumers
# cuya cola no existia cuando salieron los eventos. Escribe en el outbox de esta
# base: los publica el relay del billing que este corriendo contra ella.
republish-premium:
	@echo "Republicando Premium en $(DATABASE_HOST):$(DATABASE_PORT)/$(DATABASE_NAME)"
	@go run ./cmd/republish-premium
