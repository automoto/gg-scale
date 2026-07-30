FROM golang:1.26.5-bookworm AS builder

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

# GIT_COMMIT comes from the Makefile targets; GIT_REV is injected
# automatically by Dokku on git-push deploys. Either one stamps the binary.
ARG GIT_COMMIT=
ARG GIT_REV=
RUN CGO_ENABLED=0 go build \
    -ldflags="-s -w -X main.commit=${GIT_COMMIT:-${GIT_REV:-unknown}}" \
    -o /out/ggscale-server \
    ./cmd/ggscale-server

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /out/ggscale-server /ggscale-server
# Migrations are applied at startup; ggscale-server reads from
# MIGRATIONS_DIR (default /migrations). Forward-only.
COPY db/migrations /migrations

USER nonroot:nonroot
EXPOSE 8080

ENTRYPOINT ["/ggscale-server"]
