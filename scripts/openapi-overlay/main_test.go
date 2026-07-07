package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func merged(t *testing.T, base, overlay string) string {
	t.Helper()
	var baseDoc, overlayDoc yaml.Node
	require.NoError(t, yaml.Unmarshal([]byte(base), &baseDoc))
	require.NoError(t, yaml.Unmarshal([]byte(overlay), &overlayDoc))
	mergeNodes(baseDoc.Content[0], overlayDoc.Content[0])
	out, err := yaml.Marshal(baseDoc.Content[0])
	require.NoError(t, err)
	return string(out)
}

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "doc.yaml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	return path
}

func TestMergeNodes(t *testing.T) {
	tests := []struct {
		name    string
		base    string
		overlay string
		want    string
	}{
		{
			name:    "should_add_new_key_when_missing_from_base",
			base:    "a: 1\n",
			overlay: "b: 2\n",
			want:    "a: 1\nb: 2\n",
		},
		{
			name:    "should_replace_scalar_when_key_exists",
			base:    "a: 1\n",
			overlay: "a: 2\n",
			want:    "a: 2\n",
		},
		{
			name:    "should_deep_merge_nested_mappings",
			base:    "paths:\n  /a:\n    get: {}\n",
			overlay: "paths:\n  /b:\n    post: {}\n",
			want:    "paths:\n  /a:\n    get: {}\n  /b:\n    post: {}\n",
		},
		{
			name:    "should_merge_mappings_under_shared_key",
			base:    "paths:\n  /a:\n    get:\n      responses:\n        default: {}\n",
			overlay: "paths:\n  /a:\n    get:\n      responses:\n        \"200\": {}\n",
			want:    "paths:\n  /a:\n    get:\n      responses:\n        default: {}\n        \"200\": {}\n",
		},
		{
			name:    "should_replace_sequence_wholesale",
			base:    "tags:\n  - a\n  - b\n",
			overlay: "tags:\n  - c\n",
			want:    "tags:\n  - c\n",
		},
		{
			name:    "should_preserve_base_key_order",
			base:    "z: 1\na: 2\n",
			overlay: "a: 3\n",
			want:    "z: 1\na: 3\n",
		},
		{
			name:    "should_delete_key_when_overlay_value_is_null",
			base:    "responses:\n  default: {}\n  \"401\": {}\n",
			overlay: "responses:\n  default: null\n",
			want:    "responses:\n  \"401\": {}\n",
		},
		{
			name:    "should_ignore_null_delete_for_missing_key",
			base:    "a: 1\n",
			overlay: "b: null\n",
			want:    "a: 1\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.YAMLEq(t, tt.want, merged(t, tt.base, tt.overlay))
		})
	}
}

func TestParseFile(t *testing.T) {
	t.Run("should_error_on_multi_document_input", func(t *testing.T) {
		_, err := parseFile(writeTemp(t, "a: 1\n---\nb: 2\n"))
		assert.ErrorContains(t, err, "multiple YAML documents")
	})
	t.Run("should_error_on_anchor_or_alias", func(t *testing.T) {
		_, err := parseFile(writeTemp(t, "a: &x 1\nb: *x\n"))
		assert.ErrorContains(t, err, "anchors/aliases")
	})
	t.Run("should_parse_single_document", func(t *testing.T) {
		node, err := parseFile(writeTemp(t, "a: 1\n"))
		assert.NoError(t, err)
		assert.NotNil(t, node)
	})
}

func TestRunLeavesBaseIntactOnBadOverlay(t *testing.T) {
	base := writeTemp(t, "a: 1\n")
	overlay := writeTemp(t, "b: [unclosed\n")
	assert.Error(t, run(base, overlay))
	raw, err := os.ReadFile(base)
	require.NoError(t, err)
	assert.Equal(t, "a: 1\n", string(raw))
}

func TestMergeNodesKeyOrderStable(t *testing.T) {
	// YAMLEq ignores order, so pin the exact rendering separately: base keys
	// keep their position, overlay-only keys append at the end.
	got := merged(t, "z: 1\na: 2\n", "a: 3\nnew: 4\n")
	assert.Equal(t, "z: 1\na: 3\nnew: 4\n", got)
}
