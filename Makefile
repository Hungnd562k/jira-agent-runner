BIN := bin/runner
PKG := ./cmd/runner

.PHONY: build test vet lint run dry-run tidy index

build:
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o $(BIN) $(PKG)

test:
	go test -race ./...

vet:
	go vet ./...

lint: vet
	golangci-lint run

run:
	set -a; [ -f .env ] && . ./.env; set +a; go run $(PKG)

dry-run:
	set -a; [ -f .env ] && . ./.env; set +a; go run $(PKG) --once --dry-run

tidy:
	go mod tidy

# Refresh the GitNexus knowledge graph after code changes.
index:
	gitnexus analyze .
