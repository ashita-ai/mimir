.PHONY: build test lint migrate-up migrate-down generate

DATABASE_URL ?= postgres://mimir:mimir@localhost:5432/mimir?sslmode=disable
MIGRATIONS_DIR := internal/store/migrations

build:
	go build -o bin/mimir ./cmd/mimir

test:
	go test -race -count=1 ./...

lint:
	golangci-lint run

migrate-up:
	goose -dir $(MIGRATIONS_DIR) postgres "$(DATABASE_URL)" up

migrate-down:
	goose -dir $(MIGRATIONS_DIR) postgres "$(DATABASE_URL)" down

generate:
	go generate ./...
