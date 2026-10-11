.PHONY: build test lint generate run docker clean

VERSION ?= dev
ADDR ?= :8080
BIN := bin/hookinlet

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o $(BIN) ./cmd/hookinlet

test:
	go test -race ./...

lint:
	golangci-lint run ./...

# Rewrites internal/api from docs/openapi.json. Run it after editing the document.
generate:
	go generate ./internal/api

# Runs with throwaway development credentials. The master key is random on
# every run, so anything encrypted by a previous run becomes unreadable.
run:
	HOOKINLET_ADMIN_TOKEN=dev-admin-token-not-for-production-0123456789 \
	HOOKINLET_MASTER_KEY=$$(openssl rand -base64 32) \
	HOOKINLET_LISTEN_ADDR=$(ADDR) \
	HOOKINLET_LOG_FORMAT=text \
	go run ./cmd/hookinlet serve

docker:
	docker build --load --build-arg VERSION=$(VERSION) -t hookinlet:$(VERSION) .

clean:
	rm -rf bin
