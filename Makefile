.PHONY: build test test-integration e2e lint vulncheck sqlc-gen \
        up down logs psql migrate migrate-new \
        up-k8s agones-install \
        preflight preflight-k8s clean

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

# ─── Compose lite stack ─────────────────────────────────────────────────

up: preflight
	docker compose up -d --wait

down:
	docker compose --profile k8s down --remove-orphans

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

# ─── K8s profile (k3s + Agones) ─────────────────────────────────────────

up-k8s: preflight-k8s
	mkdir -p .k3s
	docker compose --profile k8s up -d --wait k3s

agones-install:
	docker compose --profile k8s run --rm agones-install

# ─── Misc ───────────────────────────────────────────────────────────────

preflight:
	@bash scripts/preflight.sh

preflight-k8s:
	@bash scripts/preflight.sh k8s

clean:
	docker compose --profile k8s down -v --remove-orphans
	rm -rf .k3s
