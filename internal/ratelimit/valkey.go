package ratelimit

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// luaTokenBucket is a classic continuous-rate token bucket. State per key:
//
//	HSET <key> tokens <float> last <unix_ms>
//
// Each call refills proportional to elapsed time, capped at burst, then
// either consumes one token (allow) or computes how many ms until the
// next token (retry_ms).
//
// ARGV: rate-per-ms, burst, now-unix-ms
// Returns: { allowed (0|1), retry_ms }
//
//nolint:gosec // G101 false positive: "tokens" is a Redis hash field, not a credential.
const luaTokenBucket = `
local key   = KEYS[1]
local rate  = tonumber(ARGV[1])
local burst = tonumber(ARGV[2])
local now   = tonumber(ARGV[3])

local data = redis.call('HMGET', key, 'tokens', 'last')
local tokens = tonumber(data[1])
local last   = tonumber(data[2])
if tokens == nil then tokens = burst end
if last   == nil then last   = now   end

local delta = math.max(0, now - last)
tokens = math.min(burst, tokens + delta * rate)

local allowed = 0
local retry_ms = 0
if tokens >= 1 then
    tokens = tokens - 1
    allowed = 1
else
    retry_ms = math.ceil((1 - tokens) / rate)
end

redis.call('HMSET', key, 'tokens', tokens, 'last', now)
-- Expire well after the bucket would naturally refill. The 60s floor
-- avoids thrashing on very-low-rate buckets.
local ttl_ms = math.max(60000, math.ceil(burst / rate * 2))
redis.call('PEXPIRE', key, ttl_ms)

return {allowed, retry_ms}
`

// ValkeyLimiter is a Limiter backed by a Valkey/Redis server.
type ValkeyLimiter struct {
	client *redis.Client
	script *redis.Script
}

// NewValkeyLimiter constructs a ValkeyLimiter. The Lua script is uploaded
// lazily on the first Allow call via redis.Script.Run (EVALSHA fallback to
// EVAL).
func NewValkeyLimiter(client *redis.Client) *ValkeyLimiter {
	return &ValkeyLimiter{
		client: client,
		script: redis.NewScript(luaTokenBucket),
	}
}

// Allow consults the Valkey-resident bucket for key. ratePerSecond and
// burst define the bucket; the same key must always be passed the same
// parameters or behaviour is undefined.
func (v *ValkeyLimiter) Allow(ctx context.Context, key string, ratePerSecond, burst float64) (Decision, error) {
	now := time.Now().UnixMilli()
	ratePerMs := ratePerSecond / 1000.0

	res, err := v.script.Run(ctx, v.client, []string{key}, ratePerMs, burst, now).Result()
	if err != nil {
		return Decision{}, fmt.Errorf("ratelimit script: %w", err)
	}

	arr, ok := res.([]any)
	if !ok || len(arr) != 2 {
		return Decision{}, fmt.Errorf("ratelimit script: unexpected reply %#v", res)
	}
	allowed, _ := arr[0].(int64)
	retryMs, _ := arr[1].(int64)

	return Decision{
		Allowed:    allowed == 1,
		RetryAfter: time.Duration(retryMs) * time.Millisecond,
	}, nil
}
