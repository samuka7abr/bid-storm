# .env carries the credentials the compose interpolates. The Makefile reads the
# same file so that host-side targets talk to the same database, without any
# literal password living in a versioned file.
-include .env
export

AUCTIONS      ?= 1
ENDS_IN       ?= 5m
MIN_INCREMENT ?= 100
STARTING_BID  ?= 0
OUT           ?= bench/auctions.json
STRATEGY      ?= optimistic
POSTGRES_PORT ?= 5432

# postgres:5432 resolves inside the compose network; localhost:$(POSTGRES_PORT)
# from the host, which is where cmd/seed runs.
COMPOSE_DB_URL = postgres://$(POSTGRES_USER):$(POSTGRES_PASSWORD)@postgres:5432/$(POSTGRES_DB)?sslmode=disable
HOST_DB_URL    = postgres://$(POSTGRES_USER):$(POSTGRES_PASSWORD)@localhost:$(POSTGRES_PORT)/$(POSTGRES_DB)?sslmode=disable

SEED_SOURCES = $(shell find cmd/seed internal -name '*.go') go.mod

.PHONY: up down migrate migrate-down seed run logs test fmt lint

up:
	docker compose up -d --build

down:
	docker compose down -v --remove-orphans

migrate:
	docker compose run --rm migrate -path=/migrations -database="$(COMPOSE_DB_URL)" up

migrate-down:
	docker compose run --rm migrate -path=/migrations -database="$(COMPOSE_DB_URL)" down -all

bin/seed: $(SEED_SOURCES)
	go build -o bin/seed ./cmd/seed

# Seeding must be negligible next to a benchmark cell, so the binary is built
# once and reused instead of going through `go run` on every call.
seed: bin/seed
	DATABASE_URL="$(HOST_DB_URL)" ./bin/seed \
		-auctions=$(AUCTIONS) \
		-ends-in=$(ENDS_IN) \
		-min-increment=$(MIN_INCREMENT) \
		-starting-bid=$(STARTING_BID) \
		$(if $(TRUNCATE),-truncate,) \
		-out=$(OUT)

run:
	STRATEGY=$(STRATEGY) docker compose up -d --build auctiond

logs:
	docker compose logs -f

test:
	go test ./...

fmt:
	gofmt -w .

lint:
	go vet ./...
