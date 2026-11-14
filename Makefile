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
	go run $(PKG)

dry-run:
	go run $(PKG) --once --dry-run

tidy:
	go mod tidy

# Refresh the GitNexus knowledge graph after code changes.
index:
	gitnexus analyze .

# Docker (see docker-compose.yml). Stop any local `make run` first: one bot token = one poller.
.PHONY: up down logs
up: repos.json
	docker compose up -d --build

down:
	docker compose down

logs:
	docker compose logs -f --no-log-prefix

# Bind mount needs the file to exist; start with no repos (Fix it just approves).
repos.json:
	echo '{}' > repos.json

# Agent worker runs on the host (needs claude CLI, git SSH access, repo toolchains).
.PHONY: worker worker-local
worker: build
	./bin/runner --worker

# Safe first run: commit on a local branch only, no push / no MR.
worker-local: build
	./bin/runner --worker --no-push
