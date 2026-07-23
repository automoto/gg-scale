### UI Bugs

1. The tenant sign page repeats this phrase too much:
"Tenants / Tenant sign-ups

Tenant sign-ups
Tenant sign-ups"

It doesnt need to be repeated that much

2. The approve and email invite button on this page https://app.ggscale.com/v1/control-panel/admin/tenant-signups is highlighted it should be neutral, we dont want deny or approve to be given higher visbility in this situation.\

3. The "create your first api key" button looks disabled on https://app.ggscale.com/v1/control-panel/tenants/1/api-keys with no keys
   1. the "create your firsty project" page has the same issue"
4. Get rid of the "Help" link
5. Top menu links dissappear sometimes e.g. a tenant admin on this page https://app.ggscale.com/v1/control-panel does not see the top level "Tenant" menu on the nav bar but on this page https://app.ggscale.com/v1/control-panel/tenants/1/projects its visible. Also the top level menbu should just say "Menu" to avoid confusion
6. This message is confusing: "HTTP API limit
Per-second rate and burst applied to this tenant's API keys. Blank restores the tier default (250/s, burst 500).

Using the tier default.

Only platform admins can change the API ceiling" on the tenant settings page. It should just be documented somewhere and we can show it read only on a lower part of the page(on the invite quotas and rate limits page).

7. The "note" inputs on the "Plan & feature requests" section on this page https://app.ggscale.com/v1/control-panel/tenants/1/settings is crooked and sitting too low now. The upgrade link feels out of place.
8. The "upgrade" link is now confusing, it includes "Upgrade" link but it also includes the "Upgrade to" fields. I dont think we need both, it should only display the paid upgrade link when the billing url is setup and it should show those fields if not.
9. On our billing page https://billing.ggscale.com/start?t=MS4xNzg0Njc2Mzg0.46EkX56xSiR87BEexncDqWm_uwNz8QfVJkdcX9Rs44I we highlight "Studio" instead of Indie but I dont think we should highlight either one, they should both be emphaized.

10. Max value bytes is kind of confusing on this page https://app.ggscale.com/v1/control-panel/tenants/1/rate-limits can we switch the input to be in megabytes instead of bytes and can you show the total tenant limit in GB
11. The link in the upper left of our site should use our logo in: internal/webassets/static


### Billing Bugs

2. We should have a "Manage My Subscription" Link on the tenant settings page once a tenant has been upgraded


### ggscale.com

We need to use the same favicon and logo that the main site uses in `internal/webassets/static`