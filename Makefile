.PHONY: build test lint generate migrate-up migrate-down migrate-status dev clean

DATABASE_URL ?= postgres://mimir:mimir@localhost:5433/mimir?sslmode=disable

build:
	CGO_ENABLED=0 go build -ldflags="-s -w" -o bin/mimir ./cmd/mimir

test:
	go test -race -count=1 ./...

lint:
	golangci-lint run

generate:
	go generate ./...

# Migrations go through the mimir binary so River tables are included.
migrate-up: build
	./bin/mimir migrate up

migrate-down: build
	./bin/mimir migrate down

migrate-status: build
	./bin/mimir migrate status

# One-command local dev setup: start PG, run migrations.
dev:
	docker compose up -d
	@echo "Waiting for PostgreSQL…"
	@until docker compose exec -T postgres pg_isready -U mimir > /dev/null 2>&1; do sleep 0.5; done
	$(MAKE) migrate-up

clean:
	rm -rf bin/
