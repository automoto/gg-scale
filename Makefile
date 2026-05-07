.PHONY: build test test-integration e2e lint vulncheck sqlc-gen templ-generate \
        up down logs psql migrate migrate-new \
        up-dev down-dev \
        up-k8s agones-install \
        up-gameserver down-gameserver \
        docker-image docker-push \
        preflight preflight-k8s clean clean-dev

FULL_STACK       := docker compose -f ops/full-stack-docker-compose.yml
GAMESERVER_STACK := docker compose -f docker-compose.yml -f ops/docker-compose.gameserver.yml

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
# Use `make up-k8s && make agones-install && make e2e`.
e2e:
	go test -race -tags=e2e -timeout=180s ./e2e/...

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

# ─── Simple stack (self-hosting) ────────────────────────────────────────

up: preflight
	docker compose up -d --wait

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

# ─── Game server stack (tier-0 self-hosting, no k8s) ────────────────────

up-gameserver: preflight
	$(GAMESERVER_STACK) up -d --wait

down-gameserver:
	$(GAMESERVER_STACK) down --remove-orphans

# ─── Full dev stack (prometheus, stripe-mock, dashboard, k8s) ───────────

up-dev: preflight
	$(FULL_STACK) up -d --wait

down-dev:
	$(FULL_STACK) --profile k8s down --remove-orphans

up-k8s: preflight-k8s
	mkdir -p .k3s
	$(FULL_STACK) --profile k8s up -d --wait k3s

agones-install:
	$(FULL_STACK) --profile k8s run --rm agones-install

clean-dev:
	$(FULL_STACK) --profile k8s down -v --remove-orphans
	rm -rf .k3s

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
