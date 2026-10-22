.PHONY: frontend build test check generate postgres
frontend:
	docker compose -f compose.test.yaml run --rm node sh -c 'corepack pnpm install --frozen-lockfile && corepack pnpm test:auth && corepack pnpm build'
build: frontend
	go build -o bin/astovia ./cmd/astovia
postgres:
	docker compose -f compose.test.yaml up -d --wait postgres
test: frontend postgres
	docker compose -f compose.test.yaml run --rm go go test -race -count=1 -v ./...
check: test
	docker compose -f compose.test.yaml run --rm go go vet ./...
generate:
	sh scripts/generate.sh
