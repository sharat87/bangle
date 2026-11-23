.DEFAULT_GOAL := dev

.PHONY: dev dev-server test build build-frontend lint vet fmt

# `make` (re)starts Bangle on 127.0.0.1:7060 in the "daemons" tmux server
# (scripts/serve.sh), with `go run`. It reads the frontend from frontend/dist
# on disk, so a frontend-only change needs just `make build-frontend` and a
# reload. Log: .logs/server.log.
dev: fmt build-frontend
	@bash scripts/serve.sh

# A throwaway instance on 7061, with its own DB and key in .data/, for trying
# changes without touching the real logins. Never connect real accounts to it.
dev-server: build-frontend
	@mkdir -p .data
	@test -f .data/dev.key || openssl rand -base64 32 > .data/dev.key
	DB_PATH=.data/dev.db BASE_URL=http://127.0.0.1:7061 SECRETS_KEY="$$(cat .data/dev.key)" \
		go run -buildvcs=true ./cmd/server -listen 127.0.0.1:7061

fmt:
	@gofmt -w .
	@go vet ./...

test: vet build-frontend
	@if [ -f .env ]; then set -a; source .env; set +a; fi; go test ./...
	@pnpm test

build: build-frontend
	go build -o bin/bangle ./cmd/server

build-frontend:
	pnpm install --config.confirm-modules-purge=false
	node build.mjs

lint:
	@test -z "$$(gofmt -l .)" || (echo "Run gofmt on:" && gofmt -l . && exit 1)

vet: lint
	go vet ./...
