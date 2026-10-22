BIN := bin
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
TEST_DATABASE_URL ?= postgres://files:files@localhost:5471/files_test?sslmode=disable
COMMANDS := server migrate clients

.PHONY: build build-linux run test test-integration cover lint vuln migrate fmt db

build:
	@for command in $(COMMANDS); do go build -ldflags="-X main.version=$(VERSION)" -o $(BIN)/$$command ./cmd/$$command || exit 1; done

build-linux:
	@for command in $(COMMANDS); do GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags="$(LDFLAGS)" -o $(BIN)/linux/$$command ./cmd/$$command || exit 1; done

run:
	set -a && . ./.env && set +a && go run -ldflags="-X main.version=$(VERSION)" ./cmd/server

db:
	docker compose up -d

migrate:
	set -a && . ./.env && set +a && go run ./cmd/migrate -command=up

test:
	go test -race ./...

test-integration:
	FILES_TEST_DATABASE_URL=$(TEST_DATABASE_URL) go test -race -count=1 ./...

cover:
	FILES_TEST_DATABASE_URL=$(TEST_DATABASE_URL) go test -count=1 -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -1

lint:
	golangci-lint run ./...

vuln:
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...

fmt:
	golangci-lint fmt ./...
