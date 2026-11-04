BINARY  := saksama
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/saksama/saksama/internal/cli.Version=$(VERSION)

# Integration test database. Override to point at your own server.
IT_IMAGE := mysql:8.0
IT_NAME  := saksama-it
IT_PORT  := 33306
IT_DSN   := root:@tcp(127.0.0.1:$(IT_PORT))/

.PHONY: all build test test-race integration-up integration-down test-integration vet fmt lint install clean demo cover

all: build

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/$(BINARY) ./cmd/saksama

test:
	go test ./...

test-race:
	go test -race ./...

vet:
	go vet ./...

lint: vet
	@test -z "$$(gofmt -l .)" || { echo "unformatted files:"; gofmt -l .; exit 1; }

fmt:
	gofmt -w .

install:
	CGO_ENABLED=0 go install -trimpath -ldflags "$(LDFLAGS)" ./cmd/saksama

clean:
	rm -rf bin

# Per-package coverage. A single merged profile needs the covdata tool, which is
# not always installed, so this reports package by package instead.
cover:
	@go test -cover ./... 2>&1 | grep -E "coverage:|no test files"

# --- integration ------------------------------------------------------------
# Spins a disposable MySQL 8.0 container, runs the integration suite against it,
# and removes the container. This is the only way to prove the analyzer's claims
# match MySQL's real behaviour.

# The wait probes TCP, not the unix socket: while mysqld initialises, the
# entrypoint runs a socket-only server, so a socket ping succeeds before the
# published port is listening, and every test would silently skip.
integration-up:
	@docker rm -f $(IT_NAME) >/dev/null 2>&1 || true
	@docker run -d --name $(IT_NAME) \
		-e MYSQL_ALLOW_EMPTY_PASSWORD=1 \
		-e MYSQL_DATABASE=shop \
		-p $(IT_PORT):3306 $(IT_IMAGE) --max-connections=200 >/dev/null
	@printf "waiting for MySQL on TCP"
	@for i in $$(seq 1 120); do \
		if docker exec $(IT_NAME) mysql -h127.0.0.1 -P3306 -uroot --silent -e "SELECT 1" >/dev/null 2>&1; then \
			echo " ready"; exit 0; \
		fi; \
		printf "."; sleep 1; \
	done; \
	echo " timed out"; exit 1

integration-down:
	@docker rm -f $(IT_NAME) >/dev/null 2>&1 || true

test-integration: integration-up
	@SAKSAMA_TEST_DSN='$(IT_DSN)' go test -tags=integration -count=1 ./test/integration/ -v

# A quick end-to-end demonstration of the analyzer with no database.
demo: build
	@./bin/saksama plan --ddl testdata/orders.sql \
		--alter "ADD INDEX ix_created (created_at)" \
		--mysql-version 8.0.36 --host db.prod --production \
		--rows 59000000 --data-bytes 18000000000 --index-bytes 4000000000 \
		--free-disk 100000000000 || true
	@echo ""
	@echo "--- same change, but it needs a rebuild: refused because of foreign keys ---"
	@./bin/saksama plan --ddl testdata/orders.sql \
		--alter "MODIFY COLUMN status varchar(64) NOT NULL" \
		--mysql-version 8.0.36 --host db.prod \
		--rows 59000000 --data-bytes 18000000000 --index-bytes 4000000000 || true
