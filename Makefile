.PHONY: build test lint generate licenses run docker clean web

VERSION ?= dev
ADDR ?= :8080
BIN := bin/hookinlet

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o $(BIN) ./cmd/hookinlet

test:
	go test -race -timeout 8m ./...

lint:
	golangci-lint run ./...

# Rewrites THIRD_PARTY_LICENSES after a dependency changes.
licenses:
	scripts/third-party-licenses.sh

# Rebuilds the admin console from web/ into internal/ui/dist (which is committed), and
# the files that depend on it. Needs Node (see web/.nvmrc); a Go build does not.
web:
	cd web && npm ci --ignore-scripts && npm run types && npm test && npm run build && node licenses.mjs
	scripts/third-party-licenses.sh

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
