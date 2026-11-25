package rbac

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func cachedGrantAuthorizer(keys ...featureCacheKey) *Authorizer {
	a := &Authorizer{featureCache: make(map[featureCacheKey]featureCacheEntry), featureTTL: time.Minute}
	for _, k := range keys {
		a.storeFeatureCache(k, true, time.Unix(1000, 0), a.featureDrops)
	}
	return a
}

func TestInvalidateFeatureGrants_should_drop_the_grants_of_the_tenant(t *testing.T) {
	a := cachedGrantAuthorizer(
		featureCacheKey{tenantID: 3, feature: FeatureP2PRelay},
		featureCacheKey{tenantID: 3, projectID: 9, feature: FeatureDedicatedServers},
	)

	a.InvalidateFeatureGrants(3)

	assert.Empty(t, a.featureCache)
}

func TestInvalidateFeatureGrants_should_keep_the_grants_of_other_tenants(t *testing.T) {
	other := featureCacheKey{tenantID: 4, feature: FeatureP2PRelay}
	a := cachedGrantAuthorizer(other)

	a.InvalidateFeatureGrants(3)

	assert.Contains(t, a.featureCache, other)
}

func TestInvalidateFeatureGrants_should_do_nothing_on_a_nil_authorizer(t *testing.T) {
	var a *Authorizer

	assert.NotPanics(t, func() { a.InvalidateFeatureGrants(3) })
}

func TestStoreFeatureCache_should_keep_an_entry_for_the_configured_ttl(t *testing.T) {
	key := featureCacheKey{tenantID: 3, feature: FeatureP2PRelay}
	now := time.Unix(1000, 0)
	a := cachedGrantAuthorizer()

	a.storeFeatureCache(key, true, now, a.featureDrops)

	assert.Equal(t, now.Add(time.Minute), a.featureCache[key].expiresAt)
}

func TestStoreFeatureCache_should_not_keep_a_read_that_ran_during_a_drop(t *testing.T) {
	a := cachedGrantAuthorizer()
	dropsBeforeRead := a.featureDrops
	a.InvalidateFeatureGrants(3)

	a.storeFeatureCache(featureCacheKey{tenantID: 3, feature: FeatureP2PRelay}, true, time.Unix(1000, 0), dropsBeforeRead)

	assert.Empty(t, a.featureCache)
}

func TestOptions_should_use_defaults_for_zero_values(t *testing.T) {
	got := Options{}.withDefaults()

	assert.Equal(t, Options{
		PolicyReloadInterval: DefaultPolicyReloadInterval,
		FeatureGrantCacheTTL: DefaultFeatureGrantCacheTTL,
	}, got)
}

func TestOptions_should_keep_set_values(t *testing.T) {
	set := Options{PolicyReloadInterval: time.Second, FeatureGrantCacheTTL: 2 * time.Second}

	assert.Equal(t, set, set.withDefaults())
}
