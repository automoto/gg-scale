-- api_keys.key_type has defaulted to 'secret' since the baseline. The
-- token-route per-IP limiter now exempts secret keys, which makes that
-- omission-default fail-open: an INSERT that forgets key_type would mint a
-- limiter-exempt key. Flip the default so an accidental omission yields the
-- least-privileged type; every live write path sets key_type explicitly.
ALTER TABLE api_keys ALTER COLUMN key_type SET DEFAULT 'publishable';
