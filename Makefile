.PHONY: help build fmt test check-test-suites test-integration test-e2e test-plugins e2e e2e-agones \
	lint check sqlc-gen templ-generate openapi \
	proto build-example-plugin seed \
	up down logs psql migrate migrate-new \
	up-full down-full \
	docker-image docker-push \
	preflight clean clean-full

.DEFAULT_GOAL := help

FULL_STACK := docker compose -f compose/full.yml

# Docker Hub: buildwrangler/ggscale — use `make docker-push TAG=1.2.3` (requires `docker login`).
DOCKER_IMAGE ?= buildwrangler/ggscale
TAG          ?= latest
GIT_COMMIT   ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
# Platforms baked into the pushed manifest. amd64 is required — the relay VMs
# (and other x86 hosts) exec the binary directly; arm64 keeps local Apple-Silicon
# pulls working. A plain `docker build` only emits the builder's native arch, so
# an arm64 laptop would otherwise push an amd64-incompatible image.
PLATFORMS    ?= linux/amd64,linux/arm64
INTEGRATION_PARALLEL ?= 8
INTEGRATION_TIMEOUT  ?= 5m
END_TO_END_TIMEOUT   ?= 15m
SQLC_VERSION ?= 1.31.1

# Fast component integrations cover the common database and subprocess paths.
# Exhaustive cross-component scenarios and live-stack probes run separately so
# CI can execute both lanes concurrently after lint and unit tests pass.
INTEGRATION_TEST_PACKAGES := \
	./tests/integration/auth/... \
	./tests/integration/db/... \
	./tests/integration/fleet/... \
	./tests/integration/jobs/... \
	./tests/integration/players/... \
	./tests/integration/secretseal/... \
	./tests/integration/tenant/... \
	./tests/integration/twofactor/... \
	./tests/integration/verifycode/...

END_TO_END_TEST_PACKAGES := \
	./tests/integration/controlpanel/... \
	./tests/integration/httpapi/... \
	./tests/integration/matchmaker/... \
	./tests/integration/migrate/... \
	./tests/e2e/...

help: ## List available targets
	@grep -hE '^[a-zA-Z0-9_-]+:.*##' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  %-22s %s\n", $$1, $$2}'

# ─── Go ─────────────────────────────────────────────────────────────────

build: ## Compile all packages
	go build ./...

fmt: ## go fmt all packages
	go fmt ./...

test: ## Unit tests with -race
	go test -race ./...

check-test-suites: ## Verify every tagged test package belongs to a CI lane
	@actual="$$(go list -tags='integration e2e' ./tests/integration/... ./tests/e2e/... | LC_ALL=C sort)"; \
	integration="$$(go list -tags='integration e2e' $(INTEGRATION_TEST_PACKAGES) | LC_ALL=C sort)"; \
	end_to_end="$$(go list -tags='integration e2e' $(END_TO_END_TEST_PACKAGES) | LC_ALL=C sort)"; \
	configured="$$(printf '%s\n%s\n' "$$integration" "$$end_to_end" | LC_ALL=C sort -u)"; \
	overlap=""; \
	for package in $$integration; do \
		if printf '%s\n' "$$end_to_end" | grep -Fqx "$$package"; then overlap="$$overlap $$package"; fi; \
	done; \
	if [ -n "$$overlap" ]; then echo "tagged test package belongs to multiple suites:$$overlap"; exit 1; fi; \
	if [ "$$actual" != "$$configured" ]; then \
		echo "tagged test package is missing from a Makefile suite"; \
		echo "discovered:"; echo "$$actual"; \
		echo "configured:"; echo "$$configured"; \
		exit 1; \
	fi

test-integration: check-test-suites ## Fast integration tests (Postgres via Testcontainers; needs Docker)
	go test -race -tags=integration -parallel=$(INTEGRATION_PARALLEL) -timeout=$(INTEGRATION_TIMEOUT) $(INTEGRATION_TEST_PACKAGES)

test-e2e: check-test-suites ## Exhaustive and live-stack end-to-end tests; run after `make up`
	go test -race -tags='integration e2e' -parallel=$(INTEGRATION_PARALLEL) -timeout=$(END_TO_END_TIMEOUT) $(END_TO_END_TEST_PACKAGES)

e2e: test-e2e ## Alias for test-e2e

# Needs a live k3s+Agones cluster: run the bw-ops dev/fleet-agones stack
# first (the fleet feature is beta, not part of GA).
e2e-agones: ## Beta: Agones backend test against a live cluster
	AGONES_E2E=1 go test -tags=agones_e2e -timeout=180s ./tests/integration/fleet/agones/...

# Already included in `make test-integration`; exists so the plugin path can
# be exercised in isolation while iterating on the supervisor.
test-plugins: ## Plugin subprocess integration test in isolation
	go test -race -tags=integration -timeout=60s ./tests/integration/fleet/plugin/...

lint: ## golangci-lint
	golangci-lint run

check: lint test ## Local CI mirror: lint + unit tests

# ─── Codegen ────────────────────────────────────────────────────────────

# Regenerates internal/db/sqlc/ from sqlc.yaml + internal/db/queries/.
# Runs sqlc in Docker so contributors don't need a host install.
sqlc-gen: ## Regenerate sqlc queries (Docker, pinned version)
	docker run --rm -v $(PWD):/src -w /src sqlc/sqlc:$(SQLC_VERSION) generate

templ-generate: ## Regenerate *_templ.go control panel templates
	go run github.com/a-h/templ/cmd/templ@v0.2.543 generate

# Regenerates openapi.yaml (the /v1 JSON API spec, used for SDK generation)
# directly from the huma-registered /v1 operations — the spec is emitted from
# the handlers themselves, so it cannot drift. See docs/openapi-generation.md.
openapi: ## Regenerate openapi.yaml from the /v1 routes
	@go run ./cmd/openapi-dump openapi.yaml

# Regenerates internal/fleet/plugin/proto/*.pb.go from fleet.proto. The
# generated files are committed so CI does not need protoc; this target only
# runs when the .proto schema changes. Requires protoc (brew install protobuf)
# plus protoc-gen-go and protoc-gen-go-grpc on $PATH ($GOPATH/bin after
# `go install google.golang.org/protobuf/cmd/protoc-gen-go@latest` and
# `go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest`).
proto: ## Regenerate fleet plugin gRPC stubs (needs protoc)
	PATH="$$PATH:$$(go env GOPATH)/bin" protoc \
		--go_out=. --go_opt=paths=source_relative \
		--go-grpc_out=. --go-grpc_opt=paths=source_relative \
		internal/fleet/plugin/proto/fleet.proto

# Drop the result at $$GGSCALE_PLUGIN_DIR/ggscale-fleet-example and run core
# with FLEET_BACKEND=plugin:example to exercise the plugin path end-to-end.
build-example-plugin: ## Build the reference fleet plugin
	go build -o bin/ggscale-fleet-example ./examples/ggscale-fleet-example

seed: ## Seed dev data (destructive: -force)
	go run ./scripts/ggscale-seed -force

# ─── Simple stack (self-hosting) ────────────────────────────────────────

up: preflight ## Basic stack: server + Postgres + Mailpit
	docker compose up -d --build --wait

down: ## Stop the basic stack
	docker compose down --remove-orphans

logs: ## Tail ggscale-server logs
	docker compose logs -f --tail=200 ggscale-server

psql: ## psql shell into the dev Postgres
	docker compose exec postgres psql -U ggscale -d ggscale

migrate: ## Run pending DB migrations
	docker compose run --rm migrate

migrate-new: ## New migration pair: make migrate-new NAME=<descriptor>
	@test -n "$(NAME)" || (echo "usage: make migrate-new NAME=<descriptor>" && exit 1)
	@last=$$(ls db/migrations/*.up.sql | sed -E 's|.*/([0-9]+)_.*|\1|' | sort -n | tail -1); \
	  next=$$(printf "%04d" $$((10#$$last + 1))); \
	  touch db/migrations/$${next}_$(NAME).up.sql db/migrations/$${next}_$(NAME).down.sql; \
	  echo "created db/migrations/$${next}_$(NAME).up.sql"; \
	  echo "created db/migrations/$${next}_$(NAME).down.sql"

clean: ## Stop the basic stack and delete its volumes
	docker compose down -v --remove-orphans

# ─── Fleet feature (beta, not part of GA) ───────────────────────────────
# The k3s + Agones fleet stack and its e2e tests live in the bw-ops repo
# (dev/fleet-agones/) — they depend on external manifests and clusters.

# ─── Full dev stack (base + prometheus) ─────────────────────────────────

up-full: preflight ## Contributor stack: base + Prometheus
	$(FULL_STACK) up -d --wait

down-full: ## Stop the full stack
	$(FULL_STACK) down --remove-orphans

clean-full: ## Stop the full stack and delete its volumes
	$(FULL_STACK) down -v --remove-orphans

# ─── Docker Hub image (ggscale-server) ──────────────────────────────────

docker-image: ## Build $(DOCKER_IMAGE):$(TAG) locally (host arch only)
	docker build \
		--build-arg GIT_COMMIT=$(GIT_COMMIT) \
		-t $(DOCKER_IMAGE):$(TAG) \
		.

docker-push: ## Build and push a multi-arch ($(PLATFORMS)) manifest to Docker Hub
	docker buildx inspect ggscale-builder >/dev/null 2>&1 \
		|| docker buildx create --name ggscale-builder --driver docker-container --use
	docker buildx build \
		--builder ggscale-builder \
		--platform $(PLATFORMS) \
		--build-arg GIT_COMMIT=$(GIT_COMMIT) \
		-t $(DOCKER_IMAGE):$(TAG) \
		--push .

# ─── Misc ───────────────────────────────────────────────────────────────

preflight: ## Verify docker daemon + .env before `up`
	@bash scripts/preflight.sh
