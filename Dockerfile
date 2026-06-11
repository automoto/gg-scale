FROM golang:1.26.4-bookworm AS builder

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG GIT_COMMIT=unknown
RUN CGO_ENABLED=0 go build \
    -ldflags="-s -w -X main.commit=${GIT_COMMIT}" \
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
