# Stripe billing: separate service + neutral OSS entitlement hooks

Decision record + implementation plan, 2026-07-15. Executes the "automate
later" line in `docs/pricing-strategy.md` ("a separate service … reading the
same DB") without ever putting billing logic, plan names, or dollar amounts
in this repo. Trigger to build: manual class bumps after Stripe Payment Link
purchases become a chore — not before.

## Decisions

1. **Billing lives in a separate service (`ggscale-billing`), not in this
   repo.** Not a build-tagged `managed` package (keeps Stripe in OSS git
   history, doubles the build/test matrix, boundary erodes one import at a
   time) and not a go-plugin (billing is webhook-driven and asynchronous —
   it wants to be an HTTP service — and Stripe credentials stay out of the
   game-server process entirely).
2. **OSS's contribution is entitlements, not billing.** The server already
   models everything money can buy: tier class + feature grants, applied
   today by a platform admin approving a change request. Billing is just an
   automated actor doing the same thing, so OSS grows exactly two neutral,
   dormant-by-default hooks (below) and nothing else.
3. **Stripe is the source of truth for subscription state.** Prices and
   products live in the Stripe dashboard; Stripe metadata carries the
   mapping to entitlements (`tier=2`, or `feature=p2p_relay`). Repricing
   never touches code. Stripe-hosted Checkout + Customer Portal means card
   data never exists in our infrastructure.
4. **The entitlement API is declarative and idempotent.** "Tenant X should
   be tier N with features […]" — not "upgrade X". Webhook retries and the
   reconcile job then converge instead of double-applying.
5. **The `stripe_customer_id ↔ tenant_id` mapping lives in the billing
   service's own storage.** The OSS schema stays dollar- and Stripe-free.
6. **The change-request workflow survives self-serve billing.** It remains
   the path for tier_3 custom deals and any feature you want a human
   conversation on; billing-driven changes ride the same apply/audit/email
   code path with a "billing-service" audit actor instead of an admin user.

## Current state (verified 2026-07-15)

- Entitlement mechanisms shipped (`docs/temp/tier-rework.md`): `tenants.tier`
  int 0..3, `enforce_quotas`, `feature_grants`, change-request approve path
  that auto-applies + audits + emails (`internal/controlpanel/change_requests.go`).
- Downgrade grace already specced: set class 0 → over-cap tenant gets
  notify+grace, never an instant wall (`docs/pricing-strategy.md`).
- Secret bootstrap pattern to copy: auto-generated into `server_secrets`,
  env var = optional override (2FA key pattern, `twofactor.Load`).
- Launch commercial layer (already decided, already possible): Stripe
  Payment Link + a human bumping class in the control panel. This plan
  changes nothing until its trigger fires.

## Stage 1 — OSS hooks (this repo, TDD; both dormant unless configured)

Shipped 2026-07-18 per the detailed TODO in `docs/temp/billing.md` (Part 1
A–G): `internal/entitlement` (handler + token bootstrap), `internal/billing`
(HMAC handoff signer + key bootstrap), migration 0015 (`actor_service` audit
columns), `BILLING_PORTAL_URL`/`BILLING_UPGRADE_URL` link-outs, mounted via
`httpapi.Deps.EntitlementAPIToken`.

### T1 — Billing portal link-out

- [x] `BILLING_PORTAL_URL` env (default empty = feature off). When set, the
      tenant settings page renders "Upgrade / Manage billing" linking to it
      (target: Stripe-hosted Checkout / Customer Portal). When empty,
      nothing renders — self-host default, zero-config guarantee intact.
- [x] Optionally shown next to (not replacing) the change-request forms;
      copy stays plan- and price-free in this repo.
- [x] Validate as URL in `internal/config/validate.go`; templ/HTMX per
      `docs/FRONTEND_GUIDELINES.md`.

### T2 — Entitlement API

- [x] Auth: static bearer token auto-generated into `server_secrets`
      (`ENTITLEMENT_API_TOKEN` env = optional override, 2FA-key pattern).
      Endpoint disabled entirely when no token exists and env
      `ENTITLEMENT_API_ENABLED=false` (default) — dormant on self-host.
- [x] `PUT /internal/entitlements/{tenantID}`: declarative body
      `{tier: int, features: [string]}`. Applies by **reusing the
      change-request approve code paths** (same tier UPDATE, same
      feature_grants INSERT/disable, same audit actions with actor
      `billing-service`, same decision emails), so human and automated
      changes are indistinguishable in behavior and audit. The apply only
      manages the requestable umbrella features (`p2p_relay`,
      `dedicated_servers`); grants outside that set (fleet backends, custom
      human deals) are never touched by a declarative apply.
- [x] Idempotent: applying the current state is a no-op (no duplicate
      grants, no repeat emails).
- [x] Downgrades set the class and rely on the existing grace+notify path;
      never drop data or connections (grants are disabled in place, rows
      survive).
- [x] `GET /internal/entitlements/{tenantID}` returning current tier +
      grants — the reconcile read side.
- [x] Not part of `/v1` / `openapi.yaml` (internal surface, no SDK impact).
      Prod deploy binds/firewalls it tailnet-only.
- [x] Observability: counter per apply outcome (changed / no-op / rejected):
      `ggscale_entitlement_applies_total{outcome}`.

## Stage 2 — `ggscale-billing` service (separate repo, closed-able)

- [ ] Minimal Go service, own small DB (SQLite or a schema on the existing
      Postgres — its data is just customer↔tenant mapping + webhook
      dedupe): holds the Stripe SDK and secrets.
- [ ] Checkout entry: signed link from the tenant dashboard
      (`BILLING_PORTAL_URL` target) → creates a Checkout Session carrying
      `tenant_id` in metadata; Customer Portal for self-serve manage/cancel.
- [ ] Webhooks (verified signatures, event-ID dedupe):
      `checkout.session.completed`, `customer.subscription.updated`,
      `customer.subscription.deleted` → compute desired entitlements from
      price/product metadata → `PUT` the entitlement API.
- [ ] Nightly reconcile: list active Stripe subscriptions, compare against
      `GET` entitlements, converge diffs, **alert on any drift** (missed
      webhooks are a when, not an if).
- [ ] Deploy: one more Dokku app; public ingress for `/webhook` and the
      checkout redirect only; entitlement API calls over the tailnet.
- [ ] Failure posture: Stripe down or billing service down ⇒ tenants keep
      current entitlements (fail-open on service absence; the reconcile
      heals afterward).

## Explicitly out of scope

- Metered/usage-based billing (pricing doc: no per-request/per-GB meters).
- Invoicing, tax, dunning customization beyond Stripe defaults.
- Any Stripe column on OSS tables; any plan name or dollar amount in this
  repo (including tests and fixtures).
- Replacing the change-request workflow (stays for tier_3 / human-loop
  grants).

## Test checklist (write first, per TDD convention)

All landed 2026-07-18 (`internal/config/billing_test.go`,
`internal/config/entitlement_test.go`, `internal/controlpanel/billing_test.go`,
`internal/entitlement/entitlement_test.go` + `_integration_test.go`,
`internal/httpapi/entitlements_mount_test.go`).

- [x] `BILLING_PORTAL_URL` unset ⇒ settings page unchanged; set ⇒ link renders;
  invalid URL rejected at boot.
- [x] Entitlement API: disabled by default; 401 without/with wrong token;
  declarative apply is idempotent (same body twice ⇒ one audit entry, one
  email); downgrade triggers grace path, not instant enforcement; tier and
  feature changes land atomically.
- [x] Audit rows carry the billing-service actor and are distinguishable from
  human approvals (`entitlement.apply` action, NULL `actor_user_id`,
  `actor_service = 'billing-service'` — migration 0015).
- [x] Reconcile contract: GET reflects exactly what PUT applied.
