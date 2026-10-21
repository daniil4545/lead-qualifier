# Local database from docker-compose.yml; overridden by the environment.
DATABASE_URL ?= postgres://lq:lq@localhost:55432/lq?sslmode=disable
export DATABASE_URL

.PHONY: demo demo-offline db build vet fmt-check lint vuln test test-fast check prompteval

# Console demo: only OPENROUTER_API_KEY is needed. Logs go to demo.log
# so they do not interfere with the conversation.
demo: db
	go run ./cmd/lead-qualifier demo 2>>demo.log

# The same demo with a scripted stand-in for the model: needs Docker, no key and no network.
demo-offline: db
	go run ./cmd/lead-qualifier demo --offline 2>>demo.log

db:
	docker compose up -d --wait postgres

build:
	go build ./...

vet:
	go vet ./...

fmt-check:
	@out=$$(gofmt -l .); test -z "$$out" || { echo "gofmt needed:"; echo "$$out"; exit 1; }

# gosec runs inside golangci-lint (.golangci.yml).
lint:
	golangci-lint run

# Known vulnerabilities in the module graph and the standard library.
vuln:
	go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...

# Full suite on Postgres from compose.
test: db
	go test ./...

# Fast tier without a database.
test-fast:
	go test -short ./...

check: fmt-check vet lint test build

# Golden fixtures of the catalog through a live model: make prompteval ARGS="-n 3".
prompteval:
	go run ./cmd/prompteval $(ARGS)
