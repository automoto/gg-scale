.PHONY: build test test-integration test-plugins e2e e2e-docker e2e-agones lint vulncheck sqlc-gen templ-generate \
	proto build-example-plugin seed \
        up down logs psql migrate migrate-new \
        up-fleet-docker down-fleet-docker \
        up-fleet-agones down-fleet-agones agones-install \
        up-full down-full \
        docker-image docker-push \
        preflight preflight-k8s clean clean-full

FLEET_DOCKER_STACK := docker compose -f compose/fleet-docker.yml
FLEET_AGONES_STACK := docker compose -f compose/fleet-agones.yml
FULL_STACK         := docker compose -f compose/full.yml

# Docker Hub: buildwrangler/ggscale — use `make docker-push TAG=1.2.3` (requires `docker login`).
DOCKER_IMAGE ?= buildwrangler/ggscale
TAG          ?= latest
GIT_COMMIT   ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)

# ─── Go ─────────────────────────────────────────────────────────────────

build:
	go build ./...

test:
	go test -race ./...

test-integration:
	go test -race -tags=integration ./...

# Runs the e2e suite against an already-running compose stack.
# Use `make up-fleet-agones && make agones-install && make e2e`.
e2e:
	go test -race -tags=e2e -timeout=180s ./e2e/...

# Runs the docker fleet-backend integration test against the local Docker
# daemon. Requires a reachable daemon and network access to pull
# traefik/whoami on first run.
e2e-docker:
	go test -race -tags=integration -timeout=180s ./internal/fleet/docker/...

# Runs the agones fleet-backend smoke test against the local K3s+Agones
# cluster from `make up-fleet-agones && make agones-install`. Set AGONES_E2E=1.
e2e-agones:
	AGONES_E2E=1 go test -tags=agones_e2e -timeout=180s ./internal/fleet/agones/...

lint:
	golangci-lint run

vulncheck:
	govulncheck ./...

# Regenerates internal/db/sqlc/ from sqlc.yaml + internal/db/queries/.
# Uses the official sqlc Docker image so contributors don't need a host install.
sqlc-gen:
	docker run --rm -v $(PWD):/src -w /src sqlc/sqlc:latest generate

templ-generate:
	GOSUMDB=off go run github.com/a-h/templ/cmd/templ@v0.2.543 generate

# Regenerates internal/fleet/plugin/proto/*.pb.go from fleet.proto. The
# generated files are committed so CI does not need protoc; this target only
# runs when the .proto schema changes. Requires protoc (brew install protobuf)
# plus protoc-gen-go and protoc-gen-go-grpc on $PATH ($GOPATH/bin after
# `go install google.golang.org/protobuf/cmd/protoc-gen-go@latest` and
# `go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest`).
proto:
	PATH="$$PATH:$$(go env GOPATH)/bin" protoc \
		--go_out=. --go_opt=paths=source_relative \
		--go-grpc_out=. --go-grpc_opt=paths=source_relative \
		internal/fleet/plugin/proto/fleet.proto

# Builds the reference fleet plugin. Drop the result at
# $$GGSCALE_PLUGIN_DIR/ggscale-fleet-example and run core with
# FLEET_BACKEND=plugin:example to exercise the plugin path end-to-end.
build-example-plugin:
	go build -o bin/ggscale-fleet-example ./cmd/ggscale-fleet-example

seed:
	go run ./scripts/ggscale-seed -force

# Runs the plugin subprocess integration test (`internal/fleet/plugin/
# integration_test.go`). Builds the example plugin into a temp dir, then
# spawns + kills it under Supervisor. Already included in `make
# test-integration`; this target exists so the plugin path can be exercised
# in isolation while iterating on the supervisor.
test-plugins:
	go test -race -tags=integration -timeout=60s ./internal/fleet/plugin/...

# ─── Simple stack (self-hosting) ────────────────────────────────────────

up: preflight
	docker compose up -d --build --wait

down:
	docker compose down --remove-orphans

logs:
	docker compose logs -f --tail=200 ggscale-server

psql:
	docker compose exec postgres psql -U ggscale -d ggscale

migrate:
	docker compose run --rm migrate

migrate-new:
	@test -n "$(NAME)" || (echo "usage: make migrate-new NAME=<descriptor>" && exit 1)
	@next=$$(printf "%04d" $$(($$(ls db/migrations/*.up.sql 2>/dev/null | wc -l | tr -d ' ') + 1))); \
	  touch db/migrations/$${next}_$(NAME).up.sql db/migrations/$${next}_$(NAME).down.sql; \
	  echo "created db/migrations/$${next}_$(NAME).up.sql"; \
	  echo "created db/migrations/$${next}_$(NAME).down.sql"

clean:
	docker compose down -v --remove-orphans

# ─── Fleet feature: Docker backend ──────────────────────────────────────

up-fleet-docker: preflight
	$(FLEET_DOCKER_STACK) up -d --wait

down-fleet-docker:
	$(FLEET_DOCKER_STACK) down --remove-orphans

# ─── Fleet feature: k3s + Agones backend ────────────────────────────────

up-fleet-agones: preflight-k8s
	mkdir -p .k3s
	$(FLEET_AGONES_STACK) up -d --wait k3s

down-fleet-agones:
	$(FLEET_AGONES_STACK) down --remove-orphans
	rm -rf .k3s

agones-install:
	$(FLEET_AGONES_STACK) run --rm agones-install

# ─── Full dev stack (prometheus + docker fleet + stripe-mock) ───────────

up-full: preflight
	$(FULL_STACK) up -d --wait

down-full:
	$(FULL_STACK) down --remove-orphans

clean-full:
	$(FULL_STACK) down -v --remove-orphans

# ─── Docker Hub image (ggscale-server) ──────────────────────────────────

docker-image:
	docker build \
		--build-arg GIT_COMMIT=$(GIT_COMMIT) \
		-t $(DOCKER_IMAGE):$(TAG) \
		.

docker-push: docker-image
	docker push $(DOCKER_IMAGE):$(TAG)

# ─── Misc ───────────────────────────────────────────────────────────────

preflight:
	@bash scripts/preflight.sh

preflight-k8s:
	@bash scripts/preflight.sh k8s
