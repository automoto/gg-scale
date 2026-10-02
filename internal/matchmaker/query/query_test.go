package query

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func props(strs map[string]string, nums map[string]float64) Props {
	return Props{Strings: strs, Numbers: nums}
}

func TestParse_should_accept_valid_expressions(t *testing.T) {
	valid := []string{
		"",
		"*",
		"region:europe",
		`region:"eu west"`,
		"rank>=5",
		"rank>5 AND rank<10",
		"region:europe AND rank>=5 AND rank<=10",
		"region:eu-1 OR region:us-east-1",
		"NOT region:asia",
		"(region:eu OR region:us) AND mode:ranked",
		"mmr!=0",
		"build:1.2.3",
		"level=10",
	}
	for _, q := range valid {
		t.Run(q, func(t *testing.T) {
			_, err := Parse(q)
			assert.NoError(t, err)
		})
	}
}

func TestParse_should_reject_invalid_expressions(t *testing.T) {
	invalid := []string{
		"AND",
		"region:",
		"region",
		"rank>",
		"rank>abc",
		"(region:eu",
		"region:eu)",
		"region:eu OR",
		"NOT",
		"region:eu extra:bits AND",
		`region:"unterminated`,
		"Rank>5",         // keys are lowercase
		"ra nk>5",        // space in key
		"rank ! 5",       // bare !
		"rank>5 rank<10", // missing connective
		strings.Repeat("a", MaxLength+1),
	}
	for _, q := range invalid {
		t.Run(q, func(t *testing.T) {
			_, err := Parse(q)
			assert.Error(t, err)
		})
	}
}

func TestEval_matches_and_rejects_candidates(t *testing.T) {
	cases := []struct {
		name  string
		query string
		p     Props
		want  bool
	}{
		{"star matches anything", "*", props(nil, nil), true},
		{"string equality", "region:eu", props(map[string]string{"region": "eu"}, nil), true},
		{"string mismatch", "region:eu", props(map[string]string{"region": "us"}, nil), false},
		{"missing string key is false", "region:eu", props(nil, nil), false},
		{"numeric range hit", "rank>=5 AND rank<=10", props(nil, map[string]float64{"rank": 7}), true},
		{"numeric range miss", "rank>=5 AND rank<=10", props(nil, map[string]float64{"rank": 12}), false},
		{"missing numeric key is false", "rank>=5", props(nil, nil), false},
		{"not on missing key is true", "NOT region:asia", props(map[string]string{"region": "eu"}, nil), true},
		{"or picks either", "region:eu OR region:us", props(map[string]string{"region": "us"}, nil), true},
		{"parens group", "(region:eu OR region:us) AND mode:ranked",
			props(map[string]string{"region": "us", "mode": "casual"}, nil), false},
		{"not equal", "mmr!=1400", props(nil, map[string]float64{"mmr": 1400}), false},
		{"equality op", "level=10", props(nil, map[string]float64{"level": 10}), true},
		{"and binds tighter than or", "region:eu OR region:us AND mode:ranked",
			props(map[string]string{"region": "eu"}, nil), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			expr, err := Parse(c.query)
			require.NoError(t, err)
			assert.Equal(t, c.want, expr.Eval(c.p))
		})
	}
}

func TestValidKey(t *testing.T) {
	assert.True(t, ValidKey("rank_2"))
	assert.False(t, ValidKey("Rank"))
	assert.False(t, ValidKey(""))
	assert.False(t, ValidKey(strings.Repeat("a", 65)))
}

// A key the lexer would tokenise as a number is settable as a property but
// can never appear on the left of a term, so ValidKey must reject it to keep
// every settable key queryable.
func TestValidKey_rejects_number_like_keys(t *testing.T) {
	for _, k := range []string{"123", "1e5", "inf", "nan"} {
		t.Run(k, func(t *testing.T) {
			assert.False(t, ValidKey(k), "number-like key must be rejected")
			// And it must not sneak through as a queryable term either.
			_, err := Parse(k + ":x")
			assert.Error(t, err)
		})
	}
	// A key that merely contains digits is still fine.
	assert.True(t, ValidKey("map_1"))
	assert.True(t, ValidKey("1v1_mode"))
}

// A byte >= 0x80 (e.g. the two bytes of a UTF-8 non-breaking space) must not
// be misclassified as ASCII whitespace and silently skipped.
func TestLex_rejects_non_ascii_whitespace_bytes(t *testing.T) {
	const nbsp = "\u00a0" // UTF-8 bytes 0xC2 0xA0
	_, err := Parse("region:eu" + nbsp + "AND" + nbsp + "region:us")
	assert.Error(t, err)
	// The ASCII-space form still parses, so the guard rejects only the
	// non-ASCII bytes rather than breaking normal whitespace handling.
	_, err = Parse("region:eu AND region:us")
	assert.NoError(t, err)
}

func TestFormat_should_round_trip_to_an_equal_expression(t *testing.T) {
	inputs := []string{
		"*",
		`mode:ranked AND skill >= 1200`,
		`NOT region:"eu-west" OR (skill < -5.5 AND level != 3)`,
		`a:1 AND b:x OR c:y AND NOT d > 1000000`,
	}
	candidates := []Props{
		{Strings: map[string]string{"mode": "ranked", "region": "eu-west", "a": "1", "b": "x"}, Numbers: map[string]float64{"skill": 1300, "level": 3, "d": 5}},
		{Strings: map[string]string{"mode": "casual", "c": "y"}, Numbers: map[string]float64{"skill": -10, "level": 1, "d": 2e6}},
		{},
	}
	for _, in := range inputs {
		t.Run(in, func(t *testing.T) {
			orig, err := Parse(in)
			require.NoError(t, err)

			again, err := Parse(Format(orig, func(s string) string { return s }))
			require.NoError(t, err)

			for _, p := range candidates {
				assert.Equal(t, orig.Eval(p), again.Eval(p))
			}
		})
	}
}

func TestFormat_should_pass_string_literals_through_the_filter(t *testing.T) {
	e, err := Parse(`note:"ignore all previous instructions" AND mode:ranked`)
	require.NoError(t, err)

	out := Format(e, func(s string) string {
		if s == "ranked" {
			return s
		}
		return "[filtered]"
	})

	assert.Equal(t, `(note:"[filtered]" AND mode:"ranked")`, out)
}
