.PHONY: help build fmt test test-integration test-plugins e2e e2e-docker e2e-agones \
	lint vulncheck check sqlc-gen templ-generate openapi \
	proto build-example-plugin seed \
	up down logs psql migrate migrate-new \
	up-fleet-docker down-fleet-docker \
	up-fleet-agones down-fleet-agones agones-install \
	up-full down-full \
	docker-image docker-push \
	preflight preflight-k8s clean clean-full

.DEFAULT_GOAL := help

FLEET_DOCKER_STACK := docker compose -f compose/fleet-docker.yml
FULL_STACK         := docker compose -f compose/full.yml
GGSCALE_INFRA_DIR ?= infra
GGSCALE_INFRA_ABS := $(abspath $(GGSCALE_INFRA_DIR))
FLEET_AGONES_STACK := GGSCALE_INFRA_DIR=$(GGSCALE_INFRA_ABS) docker compose -f compose/fleet-agones.yml

# Docker Hub: buildwrangler/ggscale — use `make docker-push TAG=1.2.3` (requires `docker login`).
DOCKER_IMAGE ?= buildwrangler/ggscale
TAG          ?= latest
GIT_COMMIT   ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
INTEGRATION_PARALLEL ?= 8
SQLC_VERSION ?= 1.31.1

help: ## List available targets
	@grep -hE '^[a-zA-Z0-9_-]+:.*##' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  %-22s %s\n", $$1, $$2}'

# ─── Go ─────────────────────────────────────────────────────────────────

build: ## Compile all packages
	go build ./...

fmt: ## go fmt all packages
	go fmt ./...

test: ## Unit tests with -race
	go test -race ./...

test-integration: ## Integration tests (Postgres via testcontainers; needs Docker)
	go test -race -tags=integration -parallel=$(INTEGRATION_PARALLEL) ./...

e2e: ## End-to-end suite; run after the relevant `make up-*`
	go test -race -tags=e2e -timeout=180s ./tests/e2e/...

# Requires a reachable Docker daemon and network access to pull
# traefik/whoami on first run.
e2e-docker: ## Docker fleet-backend test against the local daemon
	go test -race -tags=integration -timeout=180s ./internal/fleet/docker/...

e2e-agones: ## Agones smoke test; after `make up-fleet-agones agones-install`
	AGONES_E2E=1 go test -tags=agones_e2e -timeout=180s ./internal/fleet/agones/...

# Already included in `make test-integration`; exists so the plugin path can
# be exercised in isolation while iterating on the supervisor.
test-plugins: ## Plugin subprocess integration test in isolation
	go test -race -tags=integration -timeout=60s ./internal/fleet/plugin/...

lint: ## golangci-lint
	golangci-lint run

# Wrapper suppresses the accepted-vuln allowlist in scripts/govulncheck.sh.
# Requires govulncheck and jq on $PATH.
vulncheck: ## govulncheck with the accepted-vuln allowlist
	@bash scripts/govulncheck.sh

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

up: preflight ## Basic stack: server + Postgres + MailHog
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

# ─── Fleet feature: Docker backend ──────────────────────────────────────

up-fleet-docker: preflight ## Basic stack + FLEET_BACKEND=docker
	$(FLEET_DOCKER_STACK) up -d --wait

down-fleet-docker: ## Stop the Docker-fleet stack
	$(FLEET_DOCKER_STACK) down --remove-orphans

# ─── Fleet feature: k3s + Agones backend ────────────────────────────────

up-fleet-agones: preflight-k8s ## Basic stack + k3s (macOS: Colima required)
	mkdir -p .k3s
	$(FLEET_AGONES_STACK) up -d --wait k3s

down-fleet-agones: ## Stop the Agones stack and delete .k3s state
	$(FLEET_AGONES_STACK) down --remove-orphans
	rm -rf .k3s

agones-install: ## Install the Agones controller into the k3s cluster
	$(FLEET_AGONES_STACK) run --rm agones-install

# ─── Full dev stack (prometheus + docker fleet) ─────────────────────────

up-full: preflight ## Contributor stack: Docker fleet + Prometheus
	$(FULL_STACK) up -d --wait

down-full: ## Stop the full stack
	$(FULL_STACK) down --remove-orphans

clean-full: ## Stop the full stack and delete its volumes
	$(FULL_STACK) down -v --remove-orphans

# ─── Docker Hub image (ggscale-server) ──────────────────────────────────

docker-image: ## Build $(DOCKER_IMAGE):$(TAG) locally
	docker build \
		--build-arg GIT_COMMIT=$(GIT_COMMIT) \
		-t $(DOCKER_IMAGE):$(TAG) \
		.

docker-push: docker-image ## Build and push to Docker Hub
	docker push $(DOCKER_IMAGE):$(TAG)

# ─── Misc ───────────────────────────────────────────────────────────────

preflight: ## Verify docker daemon + .env before `up`
	@bash scripts/preflight.sh

preflight-k8s: ## Preflight plus Agones-profile checks (macOS: Colima)
	@GGSCALE_INFRA_DIR=$(GGSCALE_INFRA_ABS) bash scripts/preflight.sh k8s
