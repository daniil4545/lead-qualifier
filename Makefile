# Локальная база из docker-compose.yml; переопределяется окружением.
DATABASE_URL ?= postgres://lq:lq@localhost:55432/lq?sslmode=disable
export DATABASE_URL

.PHONY: demo db build vet fmt-check lint test test-fast check prompteval

# Демо в консоли: нужен только OPENROUTER_API_KEY. Логи уходят в demo.log,
# чтобы не мешать переписке.
demo: db
	go run ./cmd/lead-qualifier demo 2>>demo.log

db:
	docker compose up -d --wait postgres

build:
	go build ./...

vet:
	go vet ./...

fmt-check:
	@out=$$(gofmt -l .); test -z "$$out" || { echo "gofmt needed:"; echo "$$out"; exit 1; }

lint:
	golangci-lint run

# Полный набор на Postgres из compose.
test: db
	go test ./...

# Быстрый ярус без базы.
test-fast:
	go test -short ./...

check: fmt-check vet lint test build

# Эталоны каталога через живую модель: make prompteval ARGS="-n 3".
prompteval:
	go run ./cmd/prompteval $(ARGS)
