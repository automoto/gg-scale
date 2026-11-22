package fleet_test

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	sqlcgen "github.com/automoto/gg-scale/internal/db/sqlc"
	"github.com/automoto/gg-scale/internal/fleet"
	"github.com/automoto/gg-scale/internal/secretseal"
)

const sealMarker = "tok-SEALMARKER-7f3a"

func testCipher(t *testing.T) *secretseal.Cipher {
	t.Helper()
	key := make([]byte, 32)
	_, err := rand.Read(key)
	require.NoError(t, err)
	c, err := secretseal.NewCipherKeyring(key)
	require.NoError(t, err)
	return c
}

func TestEncodeFleetConfig_should_not_store_a_plain_plugin_value(t *testing.T) {
	raw, err := fleet.EncodeFleetConfigForTest(testCipher(t), "plugin:x", map[string]string{"api_token": sealMarker})

	require.NoError(t, err)
	assert.NotContains(t, string(raw), sealMarker)
}

func TestEncodeFleetConfig_should_keep_plugin_keys_plain(t *testing.T) {
	raw, err := fleet.EncodeFleetConfigForTest(testCipher(t), "plugin", map[string]string{"api_token": sealMarker})

	require.NoError(t, err)
	assert.Contains(t, string(raw), `"api_token"`)
}

func TestEncodeFleetConfig_should_keep_agones_values_plain(t *testing.T) {
	raw, err := fleet.EncodeFleetConfigForTest(testCipher(t), "agones", map[string]string{"fleet_name": sealMarker})

	require.NoError(t, err)
	assert.Contains(t, string(raw), sealMarker)
}

func TestEncodeFleetConfig_should_store_plain_when_no_cipher(t *testing.T) {
	raw, err := fleet.EncodeFleetConfigForTest(nil, "plugin:x", map[string]string{"api_token": sealMarker})

	require.NoError(t, err)
	assert.JSONEq(t, `{"api_token":"`+sealMarker+`"}`, string(raw))
}

// sealedValue returns a value as the store writes it for a plugin config.
func sealedValue(t *testing.T, c *secretseal.Cipher, plain string) string {
	t.Helper()
	raw, err := fleet.EncodeFleetConfigForTest(c, "plugin:x", map[string]string{"v": plain})
	require.NoError(t, err)
	var m map[string]string
	require.NoError(t, json.Unmarshal(raw, &m))
	return m["v"]
}

func configJSON(t *testing.T, key, value string) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]string{key: value})
	require.NoError(t, err)
	return b
}

func TestFleetRowToModel_reads_a_plugin_config(t *testing.T) {
	c := testCipher(t)
	tests := []struct {
		name   string
		cipher *secretseal.Cipher
		value  string
	}{
		{"should_open_a_sealed_value", c, sealedValue(t, c, sealMarker)},
		{"should_read_an_old_plain_value", c, sealMarker},
		{"should_read_a_plain_value_with_no_cipher", nil, sealMarker},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, err := fleet.FleetRowToModelForTest(tt.cipher, sqlcgen.Fleet{Backend: "plugin:x", Config: configJSON(t, "api_token", tt.value)})

			require.NoError(t, err)
			assert.Equal(t, sealMarker, f.Config["api_token"])
		})
	}
}

func TestFleetRowToModel_should_not_open_an_agones_value(t *testing.T) {
	c := testCipher(t)
	frame := sealedValue(t, c, sealMarker)

	f, err := fleet.FleetRowToModelForTest(c, sqlcgen.Fleet{Backend: "agones", Config: configJSON(t, "fleet_name", frame)})

	require.NoError(t, err)
	assert.Equal(t, frame, f.Config["fleet_name"])
}

func TestFleetRowToModel_should_not_return_a_plugin_value_that_does_not_open(t *testing.T) {
	c := testCipher(t)
	tests := []struct {
		name   string
		cipher *secretseal.Cipher
		value  string
	}{
		{"sealed_with_another_key", c, sealedValue(t, testCipher(t), sealMarker)},
		{"bad_frame", c, "ggseal1:%%%"},
		{"no_cipher", nil, sealedValue(t, c, sealMarker)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, err := fleet.FleetRowToModelForTest(tt.cipher, sqlcgen.Fleet{Backend: "plugin:x", Config: configJSON(t, "api_token", tt.value)})

			require.NoError(t, err)
			assert.Empty(t, f.Config["api_token"])
		})
	}
}

// rowFleetStore serves one stored row through the real row decode, so the
// manager sees what the Postgres store gives it.
type rowFleetStore struct {
	cipher *secretseal.Cipher
	row    sqlcgen.Fleet
}

func (s rowFleetStore) GetByID(context.Context, int64) (*fleet.Fleet, error) {
	return fleet.FleetRowToModelForTest(s.cipher, s.row)
}

func (s rowFleetStore) GetByName(context.Context, int64, string) (*fleet.Fleet, error) {
	return fleet.FleetRowToModelForTest(s.cipher, s.row)
}

func (s rowFleetStore) ListForProject(context.Context, int64) ([]*fleet.Fleet, error) {
	return nil, nil
}

func (s rowFleetStore) ListDeletedForProject(context.Context, int64) ([]*fleet.Fleet, error) {
	return nil, nil
}

type recordingBackend struct {
	*fakeBackend
	got map[string]string
}

func (b *recordingBackend) Allocate(_ context.Context, req fleet.AllocationRequest) (*fleet.Allocation, error) {
	b.got = req.Config
	return &fleet.Allocation{BackendRef: "ref-1", Address: "10.0.0.1:7777"}, nil
}

func TestManagerAllocate_should_give_the_plugin_the_plain_value_of_a_sealed_row(t *testing.T) {
	c := testCipher(t)
	sealed, err := fleet.EncodeFleetConfigForTest(c, "plugin:x", map[string]string{"api_token": sealMarker})
	require.NoError(t, err)
	backend := &recordingBackend{fakeBackend: &fakeBackend{name: "plugin:x"}}
	store := rowFleetStore{cipher: c, row: sqlcgen.Fleet{ID: 1, ProjectID: 2, Backend: "plugin:x", Config: sealed}}
	mgr := fleet.NewManager(newFakeStore(), store, backend, fleet.ManagerOptions{Clock: zeroClock})

	_, err = mgr.Allocate(context.Background(), sampleReq())

	require.NoError(t, err)
	assert.Equal(t, sealMarker, backend.got["api_token"])
}

func TestManagerAllocate_should_refuse_a_plugin_value_that_does_not_open(t *testing.T) {
	c := testCipher(t)
	backend := &recordingBackend{fakeBackend: &fakeBackend{name: "plugin:x"}}
	row := sqlcgen.Fleet{ID: 1, ProjectID: 2, Backend: "plugin:x", Config: configJSON(t, "api_token", sealedValue(t, testCipher(t), sealMarker))}
	mgr := fleet.NewManager(newFakeStore(), rowFleetStore{cipher: c, row: row}, backend, fleet.ManagerOptions{Clock: zeroClock})

	_, err := mgr.Allocate(context.Background(), sampleReq())

	assert.ErrorIs(t, err, fleet.ErrConfigNotOpened)
	assert.Nil(t, backend.got)
}

func TestManagerAllocate_should_give_the_plugin_a_frame_typed_as_a_value_unchanged(t *testing.T) {
	c := testCipher(t)
	frame := sealedValue(t, c, sealMarker)
	stored, err := fleet.EncodeFleetConfigForTest(c, "plugin:x", map[string]string{"api_token": frame})
	require.NoError(t, err)
	backend := &recordingBackend{fakeBackend: &fakeBackend{name: "plugin:x"}}
	row := sqlcgen.Fleet{ID: 1, ProjectID: 2, Backend: "plugin:x", Config: stored}
	mgr := fleet.NewManager(newFakeStore(), rowFleetStore{cipher: c, row: row}, backend, fleet.ManagerOptions{Clock: zeroClock})

	_, err = mgr.Allocate(context.Background(), sampleReq())

	require.NoError(t, err)
	assert.Equal(t, frame, backend.got["api_token"])
}

type recordingCleaner struct {
	*fakeBackend
	called bool
}

func (b *recordingCleaner) CleanupResolution(context.Context, string, map[string]string) error {
	b.called = true
	return nil
}

func TestManagerDeallocate_should_not_clean_up_with_a_plugin_value_that_does_not_open(t *testing.T) {
	c := testCipher(t)
	store := newFakeStore()
	backend := &recordingCleaner{fakeBackend: &fakeBackend{name: "plugin:x"}}
	row := sqlcgen.Fleet{ID: 1, ProjectID: 2, Backend: "plugin:x", Config: configJSON(t, "api_token", sealedValue(t, testCipher(t), sealMarker))}
	mgr := fleet.NewManager(store, rowFleetStore{cipher: c, row: row}, backend, fleet.ManagerOptions{Clock: zeroClock})
	req := sampleReq()
	req.Labels = map[string]string{"ggscale.dev/resolution-id": "mm_sealed"}
	id, err := store.InsertPending(t.Context(), req, backend.name)
	require.NoError(t, err)

	err = mgr.Deallocate(t.Context(), id)

	assert.Equal(t, []any{true, false}, []any{errors.Is(err, fleet.ErrConfigNotOpened), backend.called})
}
