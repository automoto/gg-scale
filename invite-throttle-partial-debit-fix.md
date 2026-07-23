# Invite throttle partial-debit bug fix

Status: **planned; not implemented**

Last reviewed: 2026-07-22

## Summary

`InviteThrottle.Check` evaluates three token buckets in this order:

1. recipient;
2. inviter; and
3. domain.

Each successful check immediately debits its bucket. If a later bucket rejects
or returns an error, `Check` returns without refunding the earlier successful
debits. No invite was admitted, but those earlier buckets remain depleted until
they refill. This can incorrectly apply a recipient cooldown or reduce an
inviter's remaining quota after a different scope rejected the attempt.

Fix the defect locally by tracking the buckets successfully debited during one
`Check` call and compensating them when a later check does not succeed. This
does not require a shared cache or remote rate-limit service.

## Impact

The defect affects both control-panel invitation paths:

- player and player-link invitations through
  `internal/controlpanel/players.go`; and
- tenant team invitations through
  `internal/controlpanel/team_handlers.go`.

Examples:

- An exhausted inviter bucket can reject an attempt after its recipient bucket
  was debited. A later legitimate attempt to that recipient can then receive a
  cooldown even though the rejected invitation was never created.
- An exhausted domain bucket can reject after both the recipient and inviter
  buckets were debited, reducing two unrelated budgets for a request that did
  not proceed.
- If a later limiter call errors, `inviteThrottled` deliberately fails open.
  The invite may proceed, but the partially completed throttle check can still
  leave earlier buckets depleted unless `Check` rolls them back first.

This is a correctness and fairness issue. It does not bypass an abuse limit,
corrupt durable data, or require a schema change.

## Root cause

`internal/ratelimit/invite.go` currently loops over the resolved buckets and
returns immediately on an error or denial:

```go
for _, b := range t.buckets(ctx, a) {
	decision, err := t.lim.Allow(ctx, b.key, b.ratePerSec, b.burst)
	if err != nil {
		return Decision{}, err
	}
	if !decision.Allowed {
		return decision, nil
	}
}
```

`Limiter.Allow` consumes a token when it returns an allowed decision. The
later return paths have no record of which preceding buckets were consumed.

The existing public `InviteThrottle.Refund` method is not suitable for this
rollback. It resolves and refunds every enabled bucket. Calling it after a
partial rejection could credit the bucket that rejected and weaken that
limit. It also resolves overrides again instead of using the exact key and
policy snapshot checked by the failed attempt.

## Required behavior

Treat one `Check` call as a best-effort compensating transaction:

1. Resolve the bucket list once.
2. Skip buckets whose burst is zero or negative, preserving the existing
   unlimited behavior.
3. After each allowed decision, append that exact `inviteBucket` value to a
   local `debited` slice.
4. If a later `Allow` rejects, refund only `debited`, then return the original
   denial and `RetryAfter` unchanged.
5. If a later `Allow` returns an error, refund only `debited`, then return the
   original error unchanged.
6. Run compensation in reverse debit order.
7. Attempt every compensation even if one refund fails. Log refund failures,
   but do not replace the original denial or error.

The implementation should use a private helper that accepts the already
resolved bucket values. Conceptually:

```go
func (t *InviteThrottle) refundBuckets(ctx context.Context, buckets []inviteBucket) {
	refunder, ok := t.lim.(Refunder)
	if !ok {
		return
	}
	for i := len(buckets) - 1; i >= 0; i-- {
		b := buckets[i]
		if b.burst <= 0 {
			continue
		}
		if err := refunder.Refund(ctx, b.key, b.ratePerSec, b.burst); err != nil {
			slog.WarnContext(ctx, "invite token refund failed", "err", err, "scope", b.scope)
		}
	}
}
```

`Check` uses this helper for partial rollback. The existing public `Refund`
method may reuse it after resolving all enabled buckets because that method is
called only after a fully allowed check whose subsequent invite creation
failed.

The production `CacheLimiter` implements `Refunder`, and refunds are capped at
the bucket burst. Preserve the existing optional-`Refunder` contract: a limiter
without refund support remains safe and does not panic, although it cannot
provide compensation.

## Test-first plan

Add behavior tests to `internal/ratelimit/invite_test.go` before changing the
implementation. Use a scripted `Limiter`/`Refunder` test double so the tests can
control each sequential result and inspect refund calls without waiting for
real refill intervals.

### Required regression cases

1. **Inviter rejection refunds the recipient**
   - recipient returns allowed;
   - inviter returns denied;
   - result remains the inviter's denial; and
   - only the recipient bucket is refunded.

2. **Domain rejection refunds inviter and recipient**
   - recipient and inviter return allowed;
   - domain returns denied;
   - result remains the domain's denial; and
   - inviter and recipient are refunded once each, in reverse order.

3. **Later error refunds earlier debits**
   - one or more buckets return allowed;
   - a later call returns a sentinel error;
   - earlier buckets are refunded; and
   - the same sentinel error is returned.

4. **Refund failure does not stop compensation**
   - a later bucket denies;
   - the first reverse-order refund returns an error;
   - remaining prior buckets are still offered a refund; and
   - the original denial remains the result.

5. **Rejected bucket is never refunded**
   - verify the refund call list contains only earlier allowed buckets. This can
     be covered by the inviter- and domain-rejection cases rather than a
     separate test.

6. **Successful check behavior is unchanged**
   - all enabled buckets are debited;
   - no automatic refund occurs; and
   - the existing explicit `Refund` path still restores all tokens when invite
     creation subsequently fails.

Keep the tests in Arrange-Act-Assert form and use behavioral names such as:

```text
TestInviteThrottle_refunds_recipient_when_inviter_rejects
TestInviteThrottle_refunds_prior_buckets_when_domain_rejects
TestInviteThrottle_refunds_prior_buckets_when_later_check_errors
TestInviteThrottle_continues_rollback_after_refund_error
```

A black-box test using `CacheLimiter` may additionally prove the user-visible
regression:

1. consume a one-token inviter or domain budget with recipient A;
2. attempt recipient B so its recipient bucket succeeds before the later
   bucket rejects;
3. explicitly refund the first successful attempt to reopen the later bucket;
4. retry recipient B; and
5. assert it is allowed rather than incorrectly blocked by its cooldown.

## Implementation scope

Expected code changes:

- `internal/ratelimit/invite_test.go` — regression tests and scripted test
  limiter;
- `internal/ratelimit/invite.go` — track successful debits and add the private
  compensation helper.

No changes should be necessary in:

- control-panel handlers;
- database migrations or queries;
- rate-limit override storage;
- configuration;
- public HTTP or OpenAPI contracts; or
- the PostgreSQL-backed realtime connection-cap system.

Update comments on `Check` and `Refund` so they distinguish automatic partial
rollback from the explicit refund performed after an allowed check is followed
by an invite-create failure.

## Acceptance criteria

- A rejected invite-throttle check restores every earlier successful debit
  when the limiter supports `Refunder` and those refund calls succeed.
- An errored invite-throttle check attempts to compensate every earlier known
  successful debit before returning the original error.
- The bucket that rejected is not refunded.
- Refund attempts use the key, rate, and burst resolved for the original check;
  override lookup is not repeated during partial rollback.
- One refund failure does not prevent attempts to refund the remaining prior
  buckets.
- Throttled metrics still identify and count the bucket that rejected exactly
  once.
- The returned denial, `RetryAfter`, and error behavior are unchanged.
- Successful invite checks and explicit create-failure refunds retain their
  existing behavior.
- Nil throttles and limiters without `Refunder` remain non-panicking.
- All modified Go is `gofmt` clean.

## Verification

Run, in order:

```sh
go test -race ./internal/ratelimit
go test -race ./internal/controlpanel
make lint
```

The focused ratelimit tests are the primary regression gate. The control-panel
suite confirms the handler's fail-open-on-check-error and explicit
refund-on-create-error behavior still integrate with `InviteThrottle`.

## Limitations and follow-up

Compensation makes the sequential local implementation net-correct, but it is
not an atomic transaction across three keys. A concurrent request may observe
a temporarily depleted earlier bucket before rollback completes. That bounded
window is acceptable for the current process-local abuse guardrail and does
not justify a shared service.

If a measured future requirement demands atomic multi-bucket admission, the
contingency architecture in `cache-improvement.md` defines an all-or-nothing
`CheckMany` operation. Do not block this local fix on that future work.
