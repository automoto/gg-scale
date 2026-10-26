package relaynode_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/automoto/gg-scale/internal/relaynode"
)

func node(region string, urls ...string) relaynode.Node {
	return relaynode.Node{Region: region, TURNURLs: urls}
}

func fixedSource(nodes ...relaynode.Node) relaynode.EligibleFunc {
	return func(context.Context) ([]relaynode.Node, error) { return nodes, nil }
}

func TestSelect_should_serve_only_the_requested_region_when_it_has_a_node(t *testing.T) {
	sel := relaynode.NewSelector(fixedSource(
		node("us-east", "turn:a:3478"),
		node("eu-west", "turn:b:3478"),
	), nil)

	got, err := sel.Select(t.Context(), "eu-west")

	require.NoError(t, err)
	assert.Equal(t, relaynode.Selection{URLs: []string{"turn:b:3478"}, Region: "eu-west"}, got)
}

func TestSelect_should_fall_back_to_every_node_for_an_unknown_region(t *testing.T) {
	sel := relaynode.NewSelector(fixedSource(
		node("us-east", "turn:a:3478"),
		node("eu-west", "turn:b:3478"),
	), nil)

	got, err := sel.Select(t.Context(), "ap-south")

	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"turn:a:3478", "turn:b:3478"}, got.URLs)
}

func TestSelect_should_report_no_region_when_the_fallback_spans_regions(t *testing.T) {
	sel := relaynode.NewSelector(fixedSource(
		node("us-east", "turn:a:3478"),
		node("eu-west", "turn:b:3478"),
	), nil)

	got, err := sel.Select(t.Context(), "")

	require.NoError(t, err)
	assert.Empty(t, got.Region)
}

func TestSelect_should_report_the_region_when_the_fallback_has_one_region(t *testing.T) {
	sel := relaynode.NewSelector(fixedSource(
		node("us-east", "turn:a:3478"),
		node("us-east", "turn:b:3478"),
	), nil)

	got, err := sel.Select(t.Context(), "eu-west")

	require.NoError(t, err)
	assert.Equal(t, "us-east", got.Region)
}

func TestSelect_should_return_no_eligible_nodes_when_there_are_none(t *testing.T) {
	sel := relaynode.NewSelector(fixedSource(), nil)

	_, err := sel.Select(t.Context(), "us-east")

	assert.ErrorIs(t, err, relaynode.ErrNoEligibleNodes)
}

func TestSelect_should_pass_through_source_errors(t *testing.T) {
	boom := errors.New("db down")
	sel := relaynode.NewSelector(func(context.Context) ([]relaynode.Node, error) { return nil, boom }, nil)

	_, err := sel.Select(t.Context(), "us-east")

	assert.ErrorIs(t, err, boom)
}

func TestSelect_should_keep_each_node_urls_together_in_order(t *testing.T) {
	sel := relaynode.NewSelector(fixedSource(
		node("us-east", "turn:a:3478?transport=udp", "turns:a:5349?transport=tcp"),
		node("us-east", "turn:b:3478?transport=udp", "turns:b:5349?transport=tcp"),
	), nil)

	got, err := sel.Select(t.Context(), "us-east")

	require.NoError(t, err)
	assert.Contains(t, [][]string{
		{"turn:a:3478?transport=udp", "turns:a:5349?transport=tcp", "turn:b:3478?transport=udp", "turns:b:5349?transport=tcp"},
		{"turn:b:3478?transport=udp", "turns:b:5349?transport=tcp", "turn:a:3478?transport=udp", "turns:a:5349?transport=tcp"},
	}, got.URLs)
}

func TestSelect_should_shuffle_node_order_between_calls(t *testing.T) {
	sel := relaynode.NewSelector(fixedSource(
		node("us-east", "turn:a:3478"),
		node("us-east", "turn:b:3478"),
		node("us-east", "turn:c:3478"),
		node("us-east", "turn:d:3478"),
	), nil)
	seen := map[string]bool{}

	for range 50 {
		got, err := sel.Select(t.Context(), "us-east")
		require.NoError(t, err)
		seen[got.URLs[0]] = true
	}

	assert.Greater(t, len(seen), 1)
}

func TestSelect_should_not_mutate_the_source_nodes(t *testing.T) {
	nodes := []relaynode.Node{node("us-east", "turn:a:3478"), node("us-east", "turn:b:3478")}
	sel := relaynode.NewSelector(func(context.Context) ([]relaynode.Node, error) { return nodes, nil }, nil)

	for range 20 {
		_, err := sel.Select(t.Context(), "us-east")
		require.NoError(t, err)
	}

	assert.Equal(t, "turn:a:3478", nodes[0].TURNURLs[0])
}

func TestSelect_should_serve_the_embedded_relay_when_no_node_is_eligible(t *testing.T) {
	embedded := node("", "turn:203.0.113.10:3478?transport=udp")
	sel := relaynode.NewSelector(fixedSource(), &embedded)

	got, err := sel.Select(t.Context(), "us-east")

	require.NoError(t, err)
	assert.Equal(t, relaynode.Selection{URLs: []string{"turn:203.0.113.10:3478?transport=udp"}, Region: ""}, got)
}

func TestSelect_should_leave_the_embedded_relay_out_of_a_region_match(t *testing.T) {
	embedded := node("", "turn:203.0.113.10:3478?transport=udp")
	sel := relaynode.NewSelector(fixedSource(node("us-east", "turn:a:3478")), &embedded)

	got, err := sel.Select(t.Context(), "us-east")

	require.NoError(t, err)
	assert.Equal(t, []string{"turn:a:3478"}, got.URLs)
}

func TestSelect_should_include_the_embedded_relay_in_the_fallback(t *testing.T) {
	embedded := node("", "turn:203.0.113.10:3478?transport=udp")
	sel := relaynode.NewSelector(fixedSource(node("us-east", "turn:a:3478")), &embedded)

	got, err := sel.Select(t.Context(), "eu-west")

	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"turn:a:3478", "turn:203.0.113.10:3478?transport=udp"}, got.URLs)
}

func TestSelect_should_match_the_region_case_insensitively(t *testing.T) {
	sel := relaynode.NewSelector(fixedSource(
		node("us-east", "turn:a:3478"),
		node("eu-west", "turn:b:3478"),
	), nil)

	got, err := sel.Select(t.Context(), " US-East ")

	require.NoError(t, err)
	assert.Equal(t, relaynode.Selection{URLs: []string{"turn:a:3478"}, Region: "us-east"}, got)
}

func TestSelect_should_serve_the_embedded_relay_when_the_source_fails(t *testing.T) {
	embedded := node("", "turn:203.0.113.10:3478?transport=udp")
	sel := relaynode.NewSelector(func(context.Context) ([]relaynode.Node, error) { return nil, errors.New("db down") }, &embedded)

	got, err := sel.Select(t.Context(), "us-east")

	require.NoError(t, err)
	assert.Equal(t, []string{"turn:203.0.113.10:3478?transport=udp"}, got.URLs)
}

// countingSource counts its reads.
type countingSource struct {
	calls int
	nodes []relaynode.Node
}

func (c *countingSource) eligible(context.Context) ([]relaynode.Node, error) {
	c.calls++
	return c.nodes, nil
}

func TestSelect_should_reuse_a_recent_read_of_the_eligible_nodes(t *testing.T) {
	src := &countingSource{nodes: []relaynode.Node{node("us-east", "turn:a:3478")}}
	sel := relaynode.NewSelector(src.eligible, nil)
	now := time.Unix(1000, 0)
	sel.SetClock(func() time.Time { return now })

	for range 3 {
		_, err := sel.Select(t.Context(), "us-east")
		require.NoError(t, err)
	}

	assert.Equal(t, 1, src.calls)
}

func TestSelect_should_read_the_eligible_nodes_again_after_the_cache_expires(t *testing.T) {
	src := &countingSource{nodes: []relaynode.Node{node("us-east", "turn:a:3478")}}
	sel := relaynode.NewSelector(src.eligible, nil)
	now := time.Unix(1000, 0)
	sel.SetClock(func() time.Time { return now })
	_, err := sel.Select(t.Context(), "us-east")
	require.NoError(t, err)
	now = now.Add(3 * time.Second)

	_, err = sel.Select(t.Context(), "us-east")

	require.NoError(t, err)
	assert.Equal(t, 2, src.calls)
}

func TestSelect_should_not_cache_a_failed_read(t *testing.T) {
	calls := 0
	sel := relaynode.NewSelector(func(context.Context) ([]relaynode.Node, error) {
		calls++
		if calls == 1 {
			return nil, errors.New("db down")
		}
		return []relaynode.Node{node("us-east", "turn:a:3478")}, nil
	}, nil)
	_, _ = sel.Select(t.Context(), "us-east")

	got, err := sel.Select(t.Context(), "us-east")

	require.NoError(t, err)
	assert.Equal(t, []string{"turn:a:3478"}, got.URLs)
}
