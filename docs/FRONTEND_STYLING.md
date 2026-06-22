# Frontend Styling & Asset Conventions

How the dashboard and player surfaces are styled, the constraints every change
must respect, and where to look things up. This is the **design/CSS/asset**
companion to [`FRONTEND_GUIDELINES.md`](./FRONTEND_GUIDELINES.md), which covers
the templ/HTMX **behavior** review checklist. Read both before changing UI.

> TL;DR for agents: server-rendered Go + templ, **Pico CSS v2 dark theme** with
> minimal variable overrides, **htmx v2.0.10** for interactivity, a ~17-line
> vanilla `dashboard.js`. No build step, no CDN, no Tailwind, no Alpine, no
> inline `<script>`/`<style>`. A strict CSP enforces all of that — work with
> it, not around it.

---

## 1. Pinned versions & where they live

| Asset | Version | File (embedded) | Upstream docs |
|---|---|---|---|
| Pico CSS | **2.1.1** | `internal/dashboard/static/pico.min.css` | <https://picocss.com/docs> |
| htmx | **2.0.10** | `internal/dashboard/static/htmx.min.js` | <https://htmx.org/docs/> |
| dashboard theme | — | `internal/dashboard/static/dashboard.css` | this doc |
| dashboard JS | — | `internal/dashboard/static/dashboard.js` | this doc |
| fonts | — | `internal/dashboard/static/fonts/` (Inter Variable, JetBrains Mono) | self-hosted `woff2` |

- Assets are compiled into the binary via `//go:embed static` (`handler.go`)
  and served at `/v1/dashboard/assets/*` with
  `Cache-Control: public, max-age=31536000, immutable`.
- **We self-host on purpose — do not switch to a CDN.** Reasons: keeps the CSP
  `'self'`-only, works in air-gapped/self-hosted deployments, leaks no operator
  IP to a third party, and removes a supply-chain vector. (Browser cache
  partitioning since ~2020 means a CDN gives no real perf win anyway.)
- **Updating a version is a deliberate, reviewable commit:** replace the vendored
  `.min` file, note the new version here and in the table above, and re-test. Do
  not pull these from npm/CDN at build or runtime.

---

## 2. The CSP is the master constraint

Set in `internal/webutil/webutil.go`. Everything below follows from it.

**Dashboard** (`SecurityHeaders`):
```
default-src 'self'; script-src 'self'; script-src-attr 'none';
style-src 'self'; style-src-attr 'none'; img-src 'self' data:;
connect-src 'self'; base-uri 'none'; form-action 'self';
frame-ancestors 'none'; object-src 'none'
```
**Player site** (`PlayerSecurityHeaders`) is stricter — `default-src 'none'`,
no script, no stylesheet. Player pages render unstyled, semantic HTML by design.

Hard rules this imposes (a reviewer should block on any of these):

- **No inline `<script>`.** All JS is first-party files under `static/`.
  Consequently templ script templates / `templ.WithNonce` are unnecessary —
  don't introduce them.
- **No inline `<style>` and no `style="…"` attributes.** Style via classes and
  CSS variables in `dashboard.css` only. (`style-src-attr 'none'` will silently
  drop inline styles.)
- **No `hx-on:*` / inline event handlers.** `script-src-attr 'none'` blocks
  them. Use a delegated listener in `dashboard.js` instead — see the
  `data-confirm` pattern.
- **No third-party origins.** Fonts, CSS, JS, images are all same-origin
  (`img-src` also allows `data:`).
- **Never bypass templ auto-escaping** (`templ.Raw(userInput)`): injected HTML
  can carry `hx-*` attributes that fire requests, not just `<script>`.

htmx itself is hardened via the `<meta name="htmx-config">` tag in
`baseHead` (`templates.templ`): `selfRequestsOnly:true`, `allowEval:false`,
`allowScriptTags:false`, `includeIndicatorStyles:false` (the last keeps htmx
from injecting an inline `<style>`, which the CSP would reject). Keep these.

---

## 3. Pico CSS usage & constraints

Pico v2 is a **classless-first** framework: semantic elements (`<button>`,
`<input>`, `<table>`, `<article>`, `<nav>`) are styled with no classes. Our
theme layers on top via **CSS-variable rebinds**, not a parallel class system.

- **Lean on Pico defaults; override only when strictly needed.** Match the
  palette/tokens in `dashboard.css`; accept minor deviation elsewhere rather
  than fighting the framework.
- **Dark mode is fixed:** `<html data-theme="dark">` in both layouts and
  `<meta name="color-scheme" content="dark">` in `baseHead`. Do not add light
  mode paths; Pico's dark defaults handle everything not explicitly overridden.
- **Minimal `--pico-*` overrides:** `dashboard.css` rebinds only fonts (Inter
  Variable, JetBrains Mono) and the primary accent (indigo `#6366f1` family).
  Everything else — backgrounds, borders, card surfaces, text colour — comes
  from Pico's built-in dark theme. Do **not** add new `--pico-*` rebinds unless
  a Pico default is genuinely wrong for this UI. Reference:
  <https://picocss.com/docs/css-variables>.
- **Pico classes we use** (don't reinvent): `role="button"` to style a link as
  a button; `secondary`, `outline`, `contrast` button variants; `grid` for
  multi-column form rows.
- **Pill buttons via element selector:** `button, [role="button"], input[type="submit"]`
  get `border-radius: 50px` in `dashboard.css`, which wins the cascade because
  `dashboard.css` loads after `pico.min.css`. Do not set `border-radius` inline.
- Keep custom CSS minimal. Watch `dashboard.css` for override sprawl — the file
  is intentionally ~350 lines; if you need many overrides for one component,
  reconsider the approach.

---

## 4. Custom classes

`dashboard.css` defines a small set of **layout/semantic classes** on top of
Pico's dark defaults. Reuse these; don't invent near-duplicates.

| Class | Purpose |
|---|---|
| `.app-header` / `.app-main` | authenticated shell chrome (`appLayout`) |
| `.auth-shell` | centered card shell for sign-in/setup (`authLayout`) |
| `.page-header` | eyebrow + `<h1>` + subtitle block; actions pill on the right |
| `.eyebrow` / `.subtitle` / `.caption` | small label above H1 / lede / fine print |
| `.breadcrumb` | `Tenants / … / Current` trail under the header |
| `.card` (use `<section class="card">`) | primary content surface |
| `.data-table` | the standard list/table styling |
| `.badge`, `.badge-role`, `.badge-active`, `.badge-revoked` | status pills |
| `.btn-inline` | compact button (the `+ New …` action) |
| `.form-actions` | trailing Cancel/Submit row inside a form |
| `.field-error` / `.flash-success` | inline field error / one-shot success banner |
| `.empty-state` | "nothing here yet" placeholder inside a card |
| `.color-block` | indigo-tinted highlight panel (e.g. API-key reveal) — no colour variants |
| `.muted` / `.muted-id` | de-emphasized text / monospace IDs |
| `.kv` (`<dl>`) | key/value detail lists |

Shared error/flash rendering goes through the `errorAlert` and `flashSuccess`
templ components — don't re-inline `<p role="alert">` / `<p class="flash-success">`.

---

## 5. Standard page anatomy

Full pages wrap in `appLayout(title, userEmail, csrfToken)` (authenticated) or
`authLayout(title)` (pre-auth). The canonical content shape, in order:

```
.breadcrumb                      (on sub-pages)
.page-header
  .eyebrow + <h1> + .subtitle
  actions-cell                   (e.g. + New …, role="button" class="btn-inline")
@flashSuccess(vm.Message)        (renders nothing when empty)
@errorAlert(vm.Error)            (renders nothing when empty)
<section class="card"> … </section>
```

`HomePage` (tenants list) is the reference layout — match it so every list page
reads as the same page across resources. For the list → `/new` → success-page
flow, see §9 of `FRONTEND_GUIDELINES.md`.

Conditional attributes use templ's `selected?={…}` / `checked?={…}` rather than
duplicating whole `<option>`/`<input>` branches in an `if/else`.

---

## 6. JavaScript

- One file: `internal/dashboard/static/dashboard.js`, loaded `defer`. It's a
  single delegated `submit` listener implementing `data-confirm` (confirm()
  before destructive form posts).
- **Add behavior as delegated listeners in this file**, keyed off `data-*`
  attributes — never inline handlers (CSP forbids them).
- Prefer doing it on the server with htmx (fragment swap, `HX-Trigger` header)
  before adding client JS. If a feature genuinely needs rich *client* state,
  raise it as a stack-level decision — don't reach for Alpine reflexively (it
  would force loosening the CSP). See the "deliberately don't require" note in
  `FRONTEND_GUIDELINES.md`.

---

## 7. Reference docs for lookups

- **Pico CSS v2** — docs <https://picocss.com/docs> · CSS variables
  <https://picocss.com/docs/css-variables> · color schemes
  <https://picocss.com/docs/color-schemes>
- **htmx 2.x** — docs <https://htmx.org/docs/> · attribute reference
  <https://htmx.org/reference/> · config <https://htmx.org/docs/#config> ·
  security essay <https://htmx.org/essays/web-security-basics-with-htmx/>
- **templ** — elements/attributes
  <https://templ.guide/syntax-and-usage/elements> · injection attacks
  <https://templ.guide/security/injection-attacks> · CSP
  <https://templ.guide/security/content-security-policy/>
- **In-repo** — behavior checklist `docs/FRONTEND_GUIDELINES.md`; brand/design
  intent `docs/ggscale-design.md`; CSP source `internal/webutil/webutil.go`;
  theme `internal/dashboard/static/dashboard.css`.
