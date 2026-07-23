# Config Refactor: migrate `internal/config` to caarlos0/env/v11

Status: DONE — `internal/config` migrated to struct tags; `config.go` is now
declaration-only, `load.go` holds `Load`/`buildEnvironment`/`renameParseErrors`/
`normalize`/`DeclaredVars`, and per-field range/enum checks live in
`validate.go`'s `checkFields`. Full suite + `make lint` green; strict-bool
`DOCKER_REQUIRE_DIGEST` verified via boot smoke.
Owner: coding agent
Scope: `internal/config/` only (plus `go.mod`/`go.sum`). No other package changes expected.

## Context

`internal/config/config.go` is 738 lines, dominated by ~60 hand-written setter
closures in a `declared []varDecl` table that each re-implement
bool/int/duration/CSV parsing with bespoke error messages. We are migrating to
`github.com/caarlos0/env/v11` struct tags so the `Config` struct becomes the
single declaration point. The library is MIT-licensed, has zero transitive
dependencies, and works with Go 1.26. Pin `v11.4.1`.

Everything behavior-visible must be preserved **except one agreed change**:
`DOCKER_REQUIRE_DIGEST` becomes strict `strconv.ParseBool` (drops `yes`/`no`
support; gains `t`/`T`/`True` like every other bool).

### Contracts that MUST survive (all test-enforced)

1. **Drift test**: `DeclaredVars()` returns every declared env name plus
   `<NAME>_FILE` for file-fallback vars; `TestEnvExample_has_no_drift` compares
   it against `.env.example` in both directions. `.env.example` must need no
   changes.
2. **`<NAME>_FILE` secret convention** (semantics differ from the library's
   `,file` tag option — do NOT use that option):
   - `_FILE` wins over the plain env var when both are set.
   - Unreadable file = hard `Load` error whose message contains `<NAME>_FILE`.
   - File content is trimmed with `strings.TrimRight(s, " \t\r\n")`.
   - Empty file content ≡ unset (so an empty `DATABASE_URL_FILE` still trips
     the required check).
   - File-fallback vars (exactly these 7): `DATABASE_URL`,
     `METRICS_AUTH_TOKEN`, `JWT_SIGNING_KEY`, `TWO_FACTOR_ENC_KEY`,
     `K3S_SA_TOKEN`, `K3S_CA_CERT_B64`, `RELAY_SHARED_SECRET`.
3. **Set-but-empty ≡ unset**: an env var set to `""` behaves as missing
   (default applies; `required` fails). The tests' `clearEnv` helper uses
   `t.Setenv(k, "")`, so this is load-bearing.
4. **Errors name the env var**: every load failure message contains the env
   var name (tests assert with `assert.Contains`).
5. **Public API unchanged**: `config.Load() (*Config, error)`,
   `(*Config).Validate() error`, `config.DeclaredVars() []string`. All field
   names and types unchanged — the four CSV fields stay `[]string` so
   `cmd/ggscale-server/main.go` (the only consumer) needs zero changes.
6. **Field doc comments preserved verbatim** — they are operator documentation.

### Library traps (verified by reading env v11.4.1 source — handle all four)

1. A var set to `""` counts as "exists" for `,required`, so a naive migration
   breaks `TestLoad_returns_error_when_required_var_missing`. Fix: pass a
   custom `env.Options{Environment: map}` built from `os.Environ()` with
   empty-valued entries **dropped**.
2. `env.ParseError` messages carry the **struct field name**
   (`DockerRequireDigest`), not the env key. Fix: post-parse error renaming
   (see TODO 4).
3. `[]string` fields split on `,` with **no trimming and empties kept**.
   Fix: post-parse normalization reproducing today's `splitCSV` (trim each
   element, drop empties, `nil` when nothing remains).
4. `env.Parse` returns an `env.AggregateError` reporting all bad vars at once
   (old loader stopped at the first). Acceptable delta; tests use `Contains`.

---

## TODOs

### 1. Dependency
- [x] `go get github.com/caarlos0/env/v11@v11.4.1`
- [x] `go mod tidy`
- [x] Confirm no new transitive deps appeared in `go.mod`.

### 2. TDD: red test first
- [x] In `internal/config/config_test.go`, add
      `TestLoad_docker_require_digest_is_strict_bool`:
      - `DOCKER_REQUIRE_DIGEST=yes` and `=no` → `Load` errors, message contains
        `DOCKER_REQUIRE_DIGEST` (use `clearEnv(t)` + `t.Setenv("DATABASE_URL", ...)`
        + `t.Setenv("DOCKER_REQUIRE_DIGEST", v)`; note `clearEnv` does not clear
        this var, so `t.Setenv` it explicitly).
      - `=1` and `=true` → `cfg.DockerRequireDigest == true`.
- [x] Run `go test ./internal/config/` and confirm the `yes`/`no` cases FAIL
      against the current loader (it accepts them). That is the red state.

### 3. Rewrite `internal/config/config.go` (struct + tags only)
- [x] Keep the package doc comment and every field doc comment verbatim.
- [x] Tag every field with `env:"NAME"`; add `envDefault:"..."` exactly where
      the current `declared` table has a non-empty `defval` (including
      `"false"`, `"0"`, `"1.0"` — mechanical translation). Vars with empty
      `defval` get no `envDefault`. The library has a built-in `time.Duration`
      parser, so duration fields need no custom handling.
- [x] `DatabaseURL string \`env:"DATABASE_URL,required" envFile:"true"\``.
- [x] Mark the 7 file-fallback fields with the custom tag `envFile:"true"`
      (foreign tag namespaces are ignored by the library; only unknown options
      inside the `env` tag error).
- [x] CSV fields stay `[]string` with plain tags: `CORSAllowedOrigins`,
      `DockerRegistryAllowlist`, `TrustedProxyCIDRs`.
- [x] CONTROL_PANEL_DISABLED inversion: add
      `ControlPanelDisabled bool \`env:"CONTROL_PANEL_DISABLED" envDefault:"false"\``
      and keep `ControlPanelEnabled bool \`env:"-"\`` (derived in `Load` as
      `!ControlPanelDisabled`). `main.go` and `validate_test.go` keep using
      `ControlPanelEnabled` untouched; `env:"-"` is skipped by both the parser
      and the reflective `DeclaredVars`, so the drift test still sees exactly
      `CONTROL_PANEL_DISABLED`.
- [x] Delete from this file: `varDecl`, `declared`, `Load`, `resolveValue`,
      `DeclaredVars`, `splitCSV`.

### 4. New `internal/config/load.go`
- [x] Implement `Load`:
      ```go
      func Load() (*Config, error) {
          envMap, err := buildEnvironment()
          if err != nil { return nil, err }
          cfg := &Config{}
          if err := env.ParseWithOptions(cfg, env.Options{Environment: envMap}); err != nil {
              return nil, renameParseErrors(err)
          }
          cfg.normalize()
          if err := cfg.checkFields(); err != nil { return nil, err }
          if err := cfg.Validate(); err != nil { return nil, err }
          return cfg, nil
      }
      ```
- [x] `buildEnvironment() (map[string]string, error)`:
      1. Convert `os.Environ()` to a map, splitting each entry on the FIRST
         `=` (values may contain `=`).
      2. Delete every entry whose value is `""` (restores set-empty ≡ unset).
      3. For each `envFile:"true"` field: if `path := m[name+"_FILE"]; path != ""`,
         read it with `os.ReadFile(path)` (keep the existing
         `//nolint:gosec // operator-supplied secret path is the documented contract`),
         on error return `fmt.Errorf("read %s_FILE %q: %w", name, path, err)`;
         otherwise trim with `strings.TrimRight(string(data), " \t\r\n")` and
         set `m[name]` to the content if non-empty, else `delete(m, name)`.
- [x] `renameParseErrors(err error) error`: unwrap `env.AggregateError`
      (`errors.As`), translate each `env.ParseError.Name` (struct field name)
      to its env key via a reflection-built `map[fieldName]envName`, emit
      `fmt.Errorf("%s: %w", envName, pe.Err)`; pass through non-ParseError
      entries (`env.VarIsNotSetError` already carries the env key); combine
      with `errors.Join`.
- [x] `(c *Config) normalize()`:
      - `c.MetricsAuthToken = strings.TrimSpace(c.MetricsAuthToken)`
      - `c.ControlPanelEnabled = !c.ControlPanelDisabled`
      - `normalizeCSV` on the four `[]string` fields: trim each element, drop
        empties, result `nil` when nothing remains (must keep the existing
        tests green: `"node-1:3322, node-2:3322 ,node-3:3322"` → 3 trimmed
        elements; empty → nil).
- [x] `DeclaredVars() []string`: reflect over `Config{}` fields; env name is
      the `env` tag before the first comma; skip `""` and `"-"`; append
      `name+"_FILE"` when `envFile == "true"`.
- [x] One shared reflection helper (field → {envName, fileFallback}) feeds
      `DeclaredVars`, `renameParseErrors`, and the `_FILE` pre-pass.

### 5. Extend `internal/config/validate.go` with per-field checks
- [x] Add unexported `(c *Config) checkFields() error`, called ONLY from
      `Load` — NOT from `Validate`. (`validate_test.go` builds sparse configs
      with zero-valued durations that would fail positivity checks.)
- [x] Table-driven where uniform; every message contains the env var name and
      the offending value:
      - `> 0` durations: `MATCHMAKER_INTERVAL`, `MATCHMAKER_CLAIM_TTL`,
        `MATCHMAKER_SWEEP_INTERVAL`, `RELAY_CRED_TTL`, `DB_MAX_CONN_LIFETIME`,
        `DB_STATEMENT_TIMEOUT`
      - `>= 0` durations: `MATCHMAKER_RELAX_AFTER`,
        `MATCHMAKER_REGION_RELAX_AFTER`, `MATCHMAKER_TICKET_TTL`
      - `> 0` ints: `MATCHMAKER_MAX_ATTEMPTS`, `MATCHMAKER_WORKER_COUNT`,
        `DB_MAX_CONNS`, `STORAGE_MAX_VALUE_BYTES`
      - `>= 0`: `REALTIME_MAX_PER_TENANT`, `REALTIME_MAX_PER_PLAYER`,
        `DB_MIN_CONNS`, `DOCKER_DEFAULT_MEMORY`, `DOCKER_DEFAULT_PIDS`,
        `DOCKER_DEFAULT_CPUS` (float)
      - `RELAY_UDP_PORT` in 1..65535
      - enum: `SMTP_TLS` ∈ off|starttls|implicit (`MAIL_PROVIDER` stays in
        `Validate` as today)
      - `TWO_FACTOR_ENC_KEY`: empty OK; else `hex.DecodeString` + exactly
        32 bytes (keep current message shape — test-covered)
      - `TRUSTED_PROXY_CIDRS`: `net.ParseCIDR` per element, error quoting the
        bad element (test-covered)
- [x] Leave the cross-field `Validate()` body untouched.

### 6. Leave untouched
- [x] `.env.example` — declared set is identical. (Optional: note strict
      true/false on the `DOCKER_REQUIRE_DIGEST` comment line.)
- [x] `cmd/ggscale-server/main.go` — zero changes.
- [x] `internal/config/validate_test.go` — should pass unmodified.

### 7. Verification
- [x] `go test ./internal/config/...` — all existing tests plus the new
      strict-bool test green; the drift test proves `DeclaredVars` parity.
- [x] `go build ./...`
- [x] `go test ./...`
- [x] `make lint` clean (keep the `//nolint:gosec` on the secret-file read;
      exported funcs keep their doc comments for revive).
- [x] Boot smoke (bounded, never a hanging foreground run): compose deps up,
      then `DATABASE_URL=... MIGRATIONS_DIR=./db/migrations go run ./cmd/ggscale-server`
      with a timeout — confirm clean startup log. Negative check:
      `DOCKER_REQUIRE_DIGEST=yes ...` exits with an error naming the var.

## Accepted behavior deltas (do not "fix" these)

1. `DOCKER_REQUIRE_DIGEST` drops `yes`/`no` (agreed, deliberate); now also
   accepts `t`/`T`/`True` etc. like every other bool.
2. Error wording changes throughout (env var names retained — tests use
   `Contains`); multiple bad vars report together (`errors.Join` /
   aggregate) instead of first-failure-wins.
3. `int` fields parse as 32-bit via the library (values above 2^31 fail);
   nonsensical inputs only.
