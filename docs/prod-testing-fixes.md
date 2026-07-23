# Prod-testing fixes — triage & implementation plan

Source: `docs/prod-testing.md` (issues found during prod testing on 2026-07-22).
This is a self-contained TODO doc for a coding agent. Work top-to-bottom within each repo.

## Scope: three repos

| Repo | Path | Items |
|------|------|-------|
| **app** (this repo) | `/Users/mydev/code/ggscale` | UI 1–8, 10, 11 + Billing 2 |
| **billing** | `/Users/mydev/code/ggscale-billing` | UI 9 |
| **landing** | `/Users/mydev/code/ggscale-landing` | ggscale.com favicon + logo |

## Conventions (app repo)

- Frontend is **templ + HTMX + Pico CSS**, **dark-theme only** (`<meta color-scheme=dark>`, `data-theme="dark"`; `app.css` has no light theme). No new light-mode work needed.
- Edit `*.templ` sources, **not** the generated `*_templ.go`. After any `.templ` edit, **regenerate** via the repo's templ codegen (Docker), then `make lint` + `make test`.
- Self-hosted assets only (no CDN, no inline `<script>`/`<style>`) per `docs/FRONTEND_*`.
- No milestone/task numbers in code identifiers, comments, or filenames (docs only).
- Prefer named byte constants (`bytesPerMiB = 1 << 20`) over bare literals.
- Do **not** `git commit` — leave that to the user.

## Decisions taken (from grill, 2026-07-22)

1. Approve/Deny → both neutral: Approve `secondary` (filled), Deny `secondary outline`.
2. Help → remove nav link only; leave `/help` route unlinked for later reuse.
3. Nav → **single always-present "Menu"** dropdown; contents change by context; top-level label always "Menu" (never "Tenant"/"Tenants").
4. API-limit card → **rate-limits page only**, editable for platform admins; removed from settings.
5. Upgrade UX → **either/or**: billing configured ⇒ external links only (Upgrade + Manage subscription), in-app tier **and** feature request forms hidden; not configured (self-host) ⇒ in-app request forms only.
6. Storage → input in **MB (decimals allowed)**, converted to bytes server-side; add a **read-only total tenant storage (GB)** line.
7. Logo → drop in `logo.svg` wordmark (dark-only app, no variant needed).
8. Manage subscription → link shown when **paid tier (`TierClass > 0`) AND `BILLING_PORTAL_URL` set**; "upgraded" ≡ paid tier.
9. Billing page → **emphasize both** Indie and Studio.

## Confirmed refinements (2026-07-22)

- **C1 — nav section headers: REVISED to plain dividers + distinct labels (no names).** Showing the name everywhere would mean threading it through ~38 nav sites + ~15 handlers (view models are sparse; the auth middleware resolves only the tenant *ID*, not the row). Not worth it — the URL and page `<h1>` already establish context. The Menu uses plain dividers between sections, and the two "Settings" entries are disambiguated as "Settings" (tenant) vs "Project settings". Name machinery removed.
- **C2 — single-tenant landing: DECIDED = auto-scope.** When a non-platform admin belongs to exactly one tenant, pre-populate nav tenant context on `/v1/control-panel` so tenant items appear in the Menu on the landing page.
- **C3 — billing emphasis: DECIDED = emphasize both.** Add `Featured: true` to the Indie card so both cards carry the highlight.
- **C4 — landing favicon: DECIDED = SVG only for now.** Drop in `favicon.svg`; skip raster/`.ico` generation (revisit as optional polish).

---

## Progress log

**Batch 1 + nav cluster complete** (app repo) — templ regen + `go build ./...` + `go test ./internal/controlpanel/...` + `golangci-lint run ./internal/controlpanel/...` all green.

- ✅ **A1** heading dedupe (dropped the duplicate eyebrow)
- ✅ **A2** neutral approve/deny (`secondary` filled / `secondary outline`)
- ✅ **A3** empty-state CTA un-dimmed (container `opacity:0.4` → muted `color`; button back to full strength)
- ✅ **A4** Help nav link removed (route + `HelpPage` left unlinked for later reuse)
- ✅ **A5** single always-present "Menu" with plain dividers between sections + an "Admin" label; **C2 auto-scope** wired via `HomeView.nav()` (+2 tests). **C1** resolved with dividers + a "Project settings" label (no per-page names — see Confirmed refinements); name machinery removed after weighing the cost.
- ✅ **A7** crooked inputs — real cause was `select` missing `margin-bottom:0` (one-line fix, simpler than the planned flex rework)
- ✅ **A6** HTTP API limit card removed from settings (stays editable on the rate-limits page); dead `apiLimitCardView`/`API*` fields/`tenantSettingsPathTpl` + a redundant per-load override query cleaned up
- ✅ **A8** either/or upgrade UX — billing configured ⇒ external Upgrade + gated "Manage subscription" (paid tier only), in-app tier+feature forms hidden; self-host ⇒ in-app forms (+4 new billing tests)
- ✅ **A9** storage in MB (decimals, MiB math) + read-only "Total tenant storage: N GB"; form field renamed `max_value_bytes`→`max_value_mb`, parser converts MB→bytes; per-value cap relabeled so it no longer reads as a total
- ✅ **A10** logo wordmark swapped in (`.brand-logo`, dark-only app so no light variant needed)

**App repo complete** — all of A1–A10 done; templ regen + `go build ./...` + `go test ./internal/controlpanel/...` + `golangci-lint` green after each batch.

- ✅ **B1** (billing) — `Featured: true` on the Indie card so both plans are emphasized equally; `go build` + `go test ./internal/web/...` green.
- ✅ **Landing** (ggscale-landing) — `favicon.svg` swapped in; navbar logo enabled with light (`logo.svg`, dark "scale" text) + dark (`logo-dark.svg`) variants, `displayTitle:false` to avoid a duplicate wordmark. `hugo --minify` builds clean; built HTML references `/favicon.svg` and both logo `<img>`s (28×31). (A stale read-only `public/images/logo.svg` from a prior local build was cleared; Cloudflare builds fresh regardless.)

**All tasks complete.**

**Deferred / optional follow-ups:**
- **C4 raster favicons** (marketing) — the SVG covers modern browsers; `.ico`/PNG fallbacks would need a rasterizer.

If per-tenant naming in the Menu is ever wanted, the clean way is to centralize the nav — resolve the tenant/project name once in middleware→context and build `AppNav` from a single `h.nav(r, active)` helper — rather than threading a name field through every page.

---

# Repo A — app (`/Users/mydev/code/ggscale`)

## A1 (UI 1) — De-duplicate "Tenant sign-ups" heading
- **File:** `internal/controlpanel/signup_templates.templ`
- The phrase renders 3× in-body: breadcrumb (`:55`), eyebrow (`:58`), h1 (`:59`). Remove the **eyebrow** `<p class="eyebrow">Tenant sign-ups</p>` at `:58`. Keep breadcrumb + h1.
- **Accept:** page shows `Tenants / Tenant sign-ups` breadcrumb + one `<h1>`; no third repeat.

## A2 (UI 2) — Neutral Approve/Deny buttons
- **File:** `internal/controlpanel/signup_templates.templ`
- Approve (`:109`) is currently a default **primary** (filled indigo) button → change to `class="secondary btn-inline"`.
- Deny (`:117`) is `contrast outline btn-inline` → change to `class="secondary outline btn-inline"`.
- Result: matched neutral pair (grey filled vs grey outline), neither emphasized, still distinguishable to avoid mis-clicks on the irreversible approve.
- **Accept:** neither button uses primary/contrast; equal visual weight.

## A3 (UI 3) — Empty-state CTA no longer looks disabled
- **File:** `internal/webassets/static/app.css` (`.empty-state`, ~`:494`)
- Root cause: `.empty-state { opacity: 0.4 }` cascades onto the `[role="button"]` CTA. The button itself is a normal `btn-inline` (no `disabled`, no variant).
- Fix: **remove `opacity: 0.4`** from `.empty-state`; instead mute only the helper text via `color: var(--pico-muted-color)`. Leave the CTA at full strength. (Alternative: keep opacity but add `.empty-state [role="button"] { opacity: 1 }` — prefer the color approach.)
- **Accept:** "Create your first API key" / "Create your first project" render as normal buttons; surrounding text still secondary.

## A4 (UI 4) — Remove Help from nav
- **File:** `internal/controlpanel/templates.templ` (`:72`) — delete the `@navLink(".../help", "Help", …)` line.
- Leave the `/v1/control-panel/help` route + `HelpPage` template in place (unlinked) for later reuse; keeps bookmarks alive. (Do not remove `navHelp` if still referenced by `HelpPage`.)
- **Accept:** no Help item in nav; visiting `/help` directly still works.

## A5 (UI 5) — Nav redesign: one always-present "Menu"
- **Files:** `internal/controlpanel/templates.templ` (`appLayout`, `:24–73`); `internal/controlpanel/types.go` (`AppNav`, `:151`); `internal/webassets/static/app.css` (nav section-label style); every page that builds `AppNav` (for C1 names — incremental).
- **Problem being fixed:** the "Tenant" dropdown was gated on `nav.TenantID > 0`, which `/v1/control-panel` (HomePage) leaves at 0 → menu vanished there but showed on tenant-scoped pages. Also "Tenant" (dropdown) vs "Tenants" (link) was confusing, and a **second** "Menu" dropdown already existed for platform admins (`:56–70`) — a rename would have collided.
- **Change:** replace the top-level "Tenants" link + the three conditional dropdowns (Tenant / Project / admin "Menu") with **one** `<details class="dropdown nav-menu"><summary>Menu</summary>` that is **always rendered**. Contents adapt by context:

  ```
  <details class="dropdown nav-menu">
    <summary>Menu</summary>
    <ul>
      <li>@navLink("/v1/control-panel", "All tenants", nav.IsActive(navTenants))</li>

      if nav.TenantID > 0 {
        // C1: name header, else divider
        if nav.TenantName != "" { <li class="nav-section">{ nav.TenantName }</li> } else { <li><hr/></li> }
        Projects · API keys · Team · Settings · Limits   // existing links, unchanged targets
      }

      if nav.TenantID > 0 && nav.ProjectID > 0 {
        if nav.ProjectName != "" { <li class="nav-section">{ nav.ProjectName }</li> } else { <li><hr/></li> }
        Players · Leaderboards · Settings
        if nav.FleetEnabled { Fleets · Allocations · Matchmaker }
      }

      if nav.IsPlatformAdmin {
        <li class="nav-section">Admin</li>
        Control panel users · Tenant sign-ups · Change requests · Player accounts · Platform admins · Server settings
        if nav.PluginsEnabled { Plugins }
      }
    </ul>
  </details>
  ```
  Preserve every existing link target from the current dropdowns (`:33–37`, `:45–52`, `:60–68`). The two "Settings" entries are disambiguated by their section (tenant-name vs project-name). Keep the right-aligned account (`userEmail`) dropdown untouched.
- **AppNav:** add `TenantName string` and `ProjectName string` fields (C1). Populate opportunistically from each page's view model (`vm.TenantName`, `vm.ProjectName`); the defensive `if != ""` fallback means unpopulated pages still render (divider only).
- **C2 (confirmed):** on `/v1/control-panel`, if the signed-in user is a non-platform admin with exactly one tenant, set `AppNav.TenantID` (+ `TenantName`) from their membership so tenant items appear on the landing page.
- **CSS:** add `.nav-menu .nav-section { font-size: 0.75rem; text-transform: uppercase; letter-spacing: 0.04em; opacity: 0.6; padding: 0.3rem 0.5rem; }`.
- **Accept:** exactly one "Menu" dropdown, always visible; no "Tenant"/"Tenants"/duplicate "Menu"; contents change with tenant/project/admin context; platform-admin items under an "Admin" section; tenant/project name shown as section header when available.

## A6 (UI 6) — API-limit card off the settings page
- **Files:** `internal/controlpanel/templates.templ` (`TenantSettingsPage`, remove `@apiLimitCard(...)` at `:2472`); check `settings.go` / `types.go` for now-dead wiring (`apiLimitCardView`, `APIDefaultRate/Burst`) and remove if unused **only there**.
- The `apiLimitCard` component (`:647–679`) stays on the **rate-limits** page, where it is already editable for platform admins (`if vm.IsPlatformAdmin`) and read-only otherwise. Verify it renders on `RateLimitsPage`; position it lower in the card order if needed.
- **Accept:** settings page has no "HTTP API limit" card; rate-limits page still shows it (editable for platform admins, read-only text + "Only platform admins can change the API ceiling" for others).

## A7 (UI 7) — Fix crooked "note" inputs
- **File:** `internal/webassets/static/app.css` (`.settings-form`, `:271–293`)
- Cause: `align-items: end` bottom-aligns labels of unequal height (the "Note (optional)" label wraps its `(optional)` span; `select` vs `input` differ), and only `input` gets `margin-bottom: 0` (not `select`).
- Fix: make each `.settings-form label` a `display:flex; flex-direction:column`, set the row to `align-items: stretch` so fields share a top/bottom baseline, and zero bottom margin on **both** `input` and `select`.
- Note: A8 removes the stray "Upgrade" link from this section, resolving the "out of place" half of the complaint.
- **Accept:** at wide width the select + note input align on one baseline; no vertical stagger.

## A8 (UI 8 + Billing 2) — Either/or upgrade & billing links
- **Files:** `internal/controlpanel/templates.templ` (Plan & feature section `:2513–2583`); `internal/controlpanel/settings.go` (`tenantSettingsView`, `billingLinks`); `internal/controlpanel/types.go` (`TenantSettingsView`).
- Define **hosted mode** = `vm.BillingUpgradeURL != "" && vm.BillingUpgradeToken != ""` (billing service wired via `BILLING_UPGRADE_URL` + `BILLING_HANDOFF_KEY`).
- **Hosted mode:**
  - Show external **"Upgrade →"** link (`:2517`).
  - Show **"Manage subscription →"** (rename from "Manage billing →", `:2520`) **only when** `vm.TierClass > 0` **and** `vm.BillingPortalURL != ""`.
  - **Hide** the in-app tier-upgrade form (`:2522–2540`) **and** the in-app feature-request form (`:2541–2559`) — upgrades and features both route through Stripe.
- **Self-host (not hosted mode):**
  - Hide external upgrade/portal links.
  - Show in-app tier-upgrade form (if `CanRequestUpgrade`) and feature-request form (if `FeatureOptions`).
- Keep the "Your requests" history table (`:2561–2581`) in both modes.
- Ensure `TierClass` (int) is on the view for the `> 0` gate (present on `TenantSettingsView`; confirm). Optional: rename section heading to "Plan & billing" in hosted mode.
- **Billing-repo follow-up (handoff, not app code):** Stripe must offer feature purchases; entitlements apply via the existing `ENTITLEMENT_API` inbound path.
- **Accept:**
  - Hosted + free tier: only "Upgrade →"; no in-app forms; no manage-subscription.
  - Hosted + paid tier: "Upgrade →" + "Manage subscription →"; no in-app forms.
  - Self-host: in-app tier + feature forms; no external links.

## A9 (UI 10) — Storage: MB input + total GB
- **Files:** `internal/controlpanel/templates.templ` (`RateLimitsPage` storage card `:733–763`); `internal/controlpanel/rate_limits.go`; `internal/controlpanel/storage_limits.go`; `internal/controlpanel/types.go` (`RateLimitsView`).
- **Reality check:** the `max_value_bytes` field is the cap on **a single stored value** (platform default **1 MiB** = `1<<20`), *not* a total. The "Tenant limit: N bytes" / "Platform default: N bytes" text is this per-value cap — it currently reads like a total, which is the actual confusion. The real **total** tenant storage quota (Tier0 5 / Tier1 25 / Tier2 100 / Tier3 500 GB) lives in `internal/quota/quota.go` and is shown nowhere.
- **Unit math:** base-1024 (MiB/GiB) so the 1 MiB default shows as a clean "1". Add named constants `bytesPerMiB = 1 << 20`, `bytesPerGiB = 1 << 30`.
- **Per-value input → MB:**
  - Inputs at `:743–745` and `:757–759`: relabel to "Max value size (MB)", `step="0.01" min="0"`, value via new `storageMBValue(bytes)` (bytes→MB, trim trailing zeros, blank when ≤0), placeholder = platform default in MB. Update the card subtitle to say MB and clarify it is **per single value**.
  - `storage_limits.go parseStorageBytes`: parse the field as a float MB, convert `bytes = round(mb * bytesPerMiB)`; blank ⇒ 0 (clear); reject `< 0`; keep the per-project clamp to the tenant ceiling (`:93–103`).
- **Add total storage (GB, read-only):**
  - `RateLimitsView`: add e.g. `StorageTotalBytes int64` (or a preformatted `StorageTotalGB string`).
  - `rate_limits.go`: the tenant tier is already fetched (`GetTenantTier`, ~`:71`); resolve the tier's total quota via `quota` (confirm exact API, e.g. `quota.LimitsForClass(tier).StorageBytes`) and populate the field.
  - Template: add a read-only line "Total tenant storage: {N} GB" in the storage card.
- **Accept:** input accepts/renders MB with decimals ("1" ⇒ 1,048,576 bytes; "0.25" ⇒ 262,144); a read-only "Total tenant storage: X GB" reflects the tier quota; per-value text no longer reads as a total. Verify a round-trip submit writes the correct byte count.

## A10 (UI 11) — App logo upper-left
- **Files:** `internal/controlpanel/templates.templ` (`appLayout` `:26`); `internal/webassets/static/app.css` (`.app-header .brand`).
- Replace the text brand with the existing (currently unused) wordmark:
  ```
  <a class="brand" href="/v1/control-panel"><img src={ webassets.URL("logo.svg") } alt="ggscale" class="brand-logo"/></a>
  ```
- CSS: `.app-header .brand-logo { height: 28px; width: auto; display: block; }` (logo `viewBox` 88×79 → ~31px wide). Drop the text-only font rules on `.brand` if unused.
- App is dark-only and the logo colors (`#818cf8` gg, `#f1f5f9` "scale") match the dark header — no variant needed.
- **Accept:** upper-left shows the ggscale wordmark image linking to `/v1/control-panel`, legible on the dark header.

---

# Repo B — billing (`/Users/mydev/code/ggscale-billing`)

## B1 (UI 9) — Emphasize both plans
- **File:** `internal/web/web.go` (`tiers`, `:69–74`)
- Add `Featured: true,` to the **Indie** card (`:70`) so both Indie and Studio carry the `featured` treatment (blue border + blue button). The template (`checkout_start.html:37`) and CSS (`:14,:22`) are already flag-driven — no template/CSS edits.
- Existing test (`web_test.go:61–74`) doesn't assert on `featured`; no test change needed.
- Per **C3**, this is the "both emphasized" reading. To instead highlight neither, remove `Featured: true` from Studio (`:72`).
- **Accept:** `/start` shows Indie and Studio with identical emphasis; neither singled out.

---

# Repo C — landing (`/Users/mydev/code/ggscale-landing`)

Hugo + Hextra theme (module). The theme's `static/` is overridden by files at the same path in the repo's (currently empty) `static/`. Preview: `hugo server` → http://localhost:1313 (extended Hugo 0.154.5). The site has a **light/dark toggle** — logos must work in both.

## C1 (ggscale.com) — Favicon
- Copy the app favicon to **`static/favicon.svg`** (overrides the theme's). Source: `/Users/mydev/code/ggscale/internal/webassets/static/favicon.svg` — self-contained `viewBox 0 0 64 64` tile (`#111827` bg, indigo `#818cf8` "gg"); safe on light/dark browser chrome.
- The `<head>` (theme `favicons.html`) also references `favicon.ico`, `favicon-16x16.png`, `favicon-32x32.png`, `apple-touch-icon.png`. Modern browsers prefer the SVG. Per **C4**, generating raster/`.ico` variants (needs a rasterizer, e.g. `rsvg-convert`/ImageMagick) into `static/` is optional polish so non-SVG clients also get the ggscale mark.
- **Accept:** built site serves our `favicon.svg` at `/favicon.svg`; tab shows the gg mark.

## C2 (ggscale.com) — Header logo
- The navbar is currently text-only (`hugo.yaml`: `title: ggscale`, `navbar.displayLogo: false`, `displayTitle: true`). Hextra renders `logo.svg` in **light** mode and `logo-dark.svg` in **dark** mode.
- **Assets** (the app logo's "scale" text is near-white `#f1f5f9` — invisible on a light navbar, so we need two variants):
  - `static/images/logo-dark.svg` = the app logo as-is (`/Users/mydev/code/ggscale/internal/webassets/static/logo.svg`) — for dark mode.
  - `static/images/logo.svg` = a light-mode variant: same SVG with the "scale" fill changed from `#f1f5f9` to a dark slate (e.g. `#0f172a`); keep the gg `#818cf8` (legible on white) — for light mode.
- **`hugo.yaml`:** set `navbar.displayLogo: true`, `navbar.displayTitle: false` (the wordmark already includes the name — avoid duplicate "ggscale"), and add under `navbar.logo`: `path: images/logo.svg`, `dark: images/logo-dark.svg`, plus `width`/`height` matching the 88:79 ratio (e.g. height ~24 → width ~27). No template edits — the theme partial already reads these params.
- **Accept:** navbar shows the ggscale logo, legible in both light and dark modes; no duplicate text title. Verify by toggling the theme in `hugo server`.

---

## Suggested order (app repo)

1. **Quick wins:** A1, A2, A3, A4, A7 (small templ/CSS edits).
2. **Structural:** A5 (nav), A6 (API card move), A8 (billing UX), A9 (storage units) — each followed by templ regen + `make lint` + `make test`.
3. **A10** (logo) last (touches shared header CSS).
4. **B1** (billing, one line).
5. **C1/C2** (landing, assets + `hugo.yaml`), preview with `hugo server`.

Update this doc's checkboxes as tasks land.
