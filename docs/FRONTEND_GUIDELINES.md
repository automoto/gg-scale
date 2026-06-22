# Frontend Guidelines

A code-review checklist for the dashboard (Go + Templ + HTMX, Pico CSS).
Each rule is a thing a reviewer should actually block on. Nits go elsewhere.

> For styling/CSS conventions, Pico constraints, pinned asset versions
> (Pico/htmx), the CSP rules, and doc links, see the companion
> [`FRONTEND_STYLING.md`](./FRONTEND_STYLING.md).

---

## 1. Templ components

- **PascalCase = exported, camelCase = file-local.** Matches Go visibility, so a reviewer reads the casing once and knows the scope.
- **Co-locate a parent view and its HTMX sub-fragments in the same `.templ` file.** A directory of single-fragment files quickly becomes meaningless in isolation.
- Compose with `children...` and component parameters. If you find yourself branching with deeply nested `if`s inside one giant component, split it instead.

## 2. Data flow into templates

- **Default: prop-drill an explicit `View` struct** (see `types.go` — `APIKeysView`, `LoginView`, etc.). Compile-time safety, and the function signature documents the dependencies.
- **`context.Context` only for cross-cutting state**: session user, CSRF token, locale, theme. Domain data via context bypasses the compiler and hides dependencies.
- **Never** pass raw DB rows (sqlc/GORM structs) into a template. Use a View struct as the translation layer — it's the existing pattern, keep it.

## 3. HTMX request lifecycle

- If a URL serves both a full page and an HTMX fragment, branch on the `HX-Request: true` header **in one middleware/helper**, not in every handler.
- Any endpoint with dual rendering must set `Vary: HX-Request`. Without it the browser back-button can serve a cached fragment as a "page".
- **Default to single-target swaps.** Reach for `hx-swap-oob` only when there's no cleaner option — chained OOB fragments couple the handler to the whole page DOM.
- To update something elsewhere on the screen, prefer the `HX-Trigger` response header + a small listener over piling on OOB swaps.

## 4. Redirects after mutations  *(easiest thing to get wrong)*

- **Never return a plain `302`/`303` to an HTMX request.** `fetch` follows it transparently and you'll swap an entire page into a small target div.
- Use **`HX-Redirect`** for hard navigations (login success, logout, deleting the resource currently being viewed).
- Use **`HX-Location`** for in-place transitions without a full reload.

## 5. Form validation

- On validation failure, return **`422 Unprocessable Entity`** with the form fragment containing inline error messages.
- `400` / `409` are fine for "malformed request" / "conflict" — but a *form-validation* failure is `422`. (Existing handlers use `400`/`409`; migrate as new forms land.)
- Render error messages inline next to the offending field, not only as a global banner.

## 6. CSRF & security

- Every state-changing endpoint goes through `requireCSRF`. No exceptions.
- HTMX requests carry the token via `hx-headers` (use the existing `csrfHeaders()` helper); plain forms use a hidden `_csrf` field. The middleware accepts either.
- **Do not bypass Templ's auto-escaping.** No `templ.Raw(userInput)` — injected HTML can carry `hx-*` attributes that fire requests, not just `<script>`.
- Session cookies stay `Secure`, `HttpOnly`, `SameSite=Lax`. Don't regress this.

## 7. UX polish that pays for itself

- Debounce/throttle live triggers: `hx-trigger="keyup changed delay:500ms"`. Otherwise every keystroke hits the server.
- Add `htmx-indicator` (or `hx-indicator`) for any request slower than ~150ms.
- Use `hx-sync="closest form:abort"` on an input that does live validation **inside** a submittable form, so the submit doesn't race the validation request.

## 8. Form decoding (Go side)

- Always declare explicit `form:"field_name"` struct tags on form-bound structs. Renaming a Go field should never silently break a posted form.

## 9. List page + dedicated `/new` form

For any resource the user can create (tenant, project, API key, etc.):

- **List page** owns the table and a small `+ New <thing>` pill button on the upper-right of `.page-header` (`role="button" class="btn-inline"`). No inline create form on the list page.
- The pill button links to a sibling `/new` route (e.g. `/v1/dashboard/tenants/{id}/projects/new`).
- **`/new` page** is a full `appLayout` page with a `.breadcrumb`, `.page-header` (eyebrow + H1 + subtitle), and a single `<section class="card">` containing the form. End the form with a `Cancel` link (`secondary outline btn-inline`) + `Create <thing>` submit (`btn-inline`) inside `.form-actions`.
- POST handler re-renders the same `New<Thing>Page` view on validation/conflict error (422 / 409) with `FieldErrors` and the user's input preserved. On success, plain `303` redirect back to the list page; pass a one-shot success message via a `?created=…` query param the list handler reads into `View.Message` and renders as `class="flash-success"`.
- If the create produces a one-time secret (e.g. an API key plaintext), redirect to a dedicated success page in `appLayout` (see `APIKeyCreatedPage` / `SignupSuccessPage`) instead of swapping a fragment in place.

The tenants list (`HomePage`) is the reference. Match its header structure verbatim so all list pages feel like the same page across resources.

## 10. Testing components

- Render the component into a `bytes.Buffer` (or `httptest.ResponseRecorder`) and assert against the HTML with `strings.Contains` — see `internal/dashboard/templates_test.go`. No new dependencies, no browser. Don't pull in `goquery` or another HTML parser unless the assertions actually need DOM traversal — for the small surface here, plain string checks are enough.
- Worth writing for: role-gated nav, conditional error rendering, anything where a regression would be silent.

---

## What we deliberately don't require

- We don't use **Tailwind** or **Alpine.js**. Don't sneak them in via a code-review comment — that's a stack-level decision.
- The dashboard is a single Go package today. **Feature-slicing** is fine to pursue later, but it isn't a review criterion now.
- File-name casing: whatever the Go toolchain is happy with. Don't bikeshed.
