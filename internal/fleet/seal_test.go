package fleet_test

import (
	"context"
	"crypto/rand"
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

func TestFleetRowToModel_reads_config(t *testing.T) {
	c := testCipher(t)
	sealed, err := fleet.EncodeFleetConfigForTest(c, "plugin:x", map[string]string{"api_token": sealMarker})
	require.NoError(t, err)
	other, err := fleet.EncodeFleetConfigForTest(testCipher(t), "plugin:x", map[string]string{"api_token": sealMarker})
	require.NoError(t, err)
	otherValue := decodeRaw(t, other)["api_token"]

	tests := []struct {
		name   string
		cipher *secretseal.Cipher
		config string
		want   string
	}{
		{"should_open_a_sealed_value", c, string(sealed), sealMarker},
		{"should_read_an_old_plain_value", c, `{"api_token":"` + sealMarker + `"}`, sealMarker},
		{"should_read_a_plain_value_with_no_cipher", nil, `{"api_token":"` + sealMarker + `"}`, sealMarker},
		{"should_return_a_value_that_does_not_open_unchanged", c, `{"api_token":"` + otherValue + `"}`, otherValue},
		{"should_return_a_bad_sealed_frame_unchanged", c, `{"api_token":"ggseal1:%%%"}`, "ggseal1:%%%"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, err := fleet.FleetRowToModelForTest(tt.cipher, sqlcgen.Fleet{Backend: "plugin:x", Config: []byte(tt.config)})

			require.NoError(t, err)
			assert.Equal(t, tt.want, f.Config["api_token"])
		})
	}
}

func decodeRaw(t *testing.T, raw []byte) map[string]string {
	t.Helper()
	f, err := fleet.FleetRowToModelForTest(nil, sqlcgen.Fleet{Backend: "plugin:x", Config: raw})
	require.NoError(t, err)
	return f.Config
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
