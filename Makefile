VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
NVM     := . $$HOME/.nvm/nvm.sh >/dev/null 2>&1 && nvm use 22 >/dev/null 2>&1;
GOBUILD := CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)"
BIN     := go run ./cmd/docker-manager

.PHONY: build web go test test-go test-web gen-api dev dev-config fixtures e2e image deploy clean

# web must finish before go embeds web/dist (sequential even under make -j)
build:
	$(MAKE) web
	$(MAKE) go

web:
	cd web && ($(NVM) npm ci --no-audit --no-fund && npm run build)
	touch web/dist/.keep

go:
	$(GOBUILD) -o bin/docker-manager ./cmd/docker-manager

test:
	$(MAKE) test-go
	$(MAKE) test-web

test-go:
	go vet ./... && go test ./...

test-web:
	cd web && ($(NVM) npm run typecheck && npm test)

# Regenerate web/src/api/schema.d.ts from the Go handlers' OpenAPI document.
gen-api:
	$(BIN) openapi > web/src/api/openapi.json
	cd web && ($(NVM) npm run gen-api)

# Go side on 127.0.0.1:8090 against the local Docker and dev/apps, Vite with hot reload on :5173.
dev: dev/config.yaml
	@echo "UI: http://localhost:5173  (password: docker-manager-dev)"
	$(BIN) serve --config dev/config.yaml & \
	cd web && ($(NVM) npm run dev); kill %1

dev-config: dev/config.yaml

dev/config.yaml:
	@mkdir -p dev/data
	@printf 'listen: "127.0.0.1:8090"\napps_root: %s/dev/apps\ndata_dir: %s/dev/data\ndev: true\nexec_enabled: true\npassword_hash: "%s"\nsecret_key: "%s"\n' \
		"$(CURDIR)" "$(CURDIR)" "$$(echo docker-manager-dev | $(BIN) password)" "$$($(BIN) gen-key)" > $@
	@echo "wrote $@ (password: docker-manager-dev)"

# Start the fixture stacks in dev/apps (local Docker) that dev mode and the e2e tests use.
fixtures:
	for d in web worker cache group/built-app; do docker compose --project-directory dev/apps/$$d up -d --quiet-pull 2>&1 | tail -n 1; done

# Browser tests against a fresh instance on :8091 (real Docker, fixture stacks, empty database).
e2e: build dev/config.yaml fixtures
	rm -rf dev/data-e2e && mkdir -p dev/data-e2e
	DM_LISTEN=127.0.0.1:8091 DM_DATA_DIR=$(CURDIR)/dev/data-e2e bin/docker-manager serve --config dev/config.yaml > dev/data-e2e/serve.log 2>&1 & \
	pid=$$!; sleep 2; \
	cd web && ($(NVM) npx playwright test); rc=$$?; kill $$pid; exit $$rc

# The container image, and shipping it to the server (deploy/deploy.sh, see README).
image:
	docker build -f deploy/Dockerfile --build-arg VERSION=$(VERSION) -t docker-manager:$(VERSION) -t docker-manager:latest .

deploy:
	deploy/deploy.sh

clean:
	rm -rf bin web/dist/assets web/dist/index.html web/dist/favicon.svg
