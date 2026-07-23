package matchmaker

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/ggscale/ggscale/internal/matchmaker/query"
)

func gt(id int64, age time.Duration, region string, minC, maxC, mult int, cross bool) *Ticket {
	return &Ticket{
		ID:               id,
		PlayerID:         id,
		Region:           region,
		MinCount:         minC,
		MaxCount:         maxC,
		CountMultiple:    mult,
		AllowCrossRegion: cross,
		CreatedAt:        testNow.Add(-age),
	}
}

var testNow = time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC)

func groupIDs(groups [][]*Ticket) [][]int64 {
	if len(groups) == 0 {
		return nil
	}
	out := make([][]int64, 0, len(groups))
	for _, g := range groups {
		ids := make([]int64, 0, len(g))
		for _, t := range g {
			ids = append(ids, t.ID)
		}
		out = append(out, ids)
	}
	return out
}

func TestFormGroups_count_semantics(t *testing.T) {
	cfg := groupConfig{relaxAfter: 30 * time.Second, regionRelaxAfter: 60 * time.Second}
	cases := []struct {
		name    string
		tickets []*Ticket
		want    [][]int64
	}{
		{
			"solo defaults commit immediately",
			[]*Ticket{gt(1, time.Second, "", 1, 1, 1, true)},
			[][]int64{{1}},
		},
		{
			"full max_count group forms immediately",
			[]*Ticket{
				gt(1, 3*time.Second, "eu", 2, 2, 1, true),
				gt(2, 2*time.Second, "eu", 2, 2, 1, true),
			},
			[][]int64{{1, 2}},
		},
		{
			"below min_count waits",
			[]*Ticket{gt(1, time.Second, "eu", 2, 4, 1, true)},
			nil,
		},
		{
			"below max but above min waits inside relax window",
			[]*Ticket{
				gt(1, time.Second, "eu", 2, 4, 1, true),
				gt(2, time.Second, "eu", 2, 4, 1, true),
				gt(3, time.Second, "eu", 2, 4, 1, true),
			},
			nil,
		},
		{
			"relaxation commits at min after the window",
			[]*Ticket{
				gt(1, 31*time.Second, "eu", 2, 4, 1, true),
				gt(2, time.Second, "eu", 2, 4, 1, true),
			},
			[][]int64{{1, 2}},
		},
		{
			"count_multiple blocks invalid sizes even when relaxed",
			[]*Ticket{
				gt(1, 31*time.Second, "eu", 2, 4, 2, true),
				gt(2, 31*time.Second, "eu", 2, 4, 2, true),
				gt(3, 31*time.Second, "eu", 2, 4, 2, true),
			},
			[][]int64{{1, 2}}, // 3 is not a multiple of 2 → trims to 2
		},
		{
			"mixed min and max in one bucket",
			[]*Ticket{
				gt(1, 40*time.Second, "eu", 1, 2, 1, true),
				gt(2, 39*time.Second, "eu", 2, 4, 1, true),
				gt(3, time.Second, "eu", 1, 1, 1, true),
			},
			// Seed 1 caps the group at 2 → {1,2}; ticket 3 solos (max 1).
			[][]int64{{1, 2}, {3}},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := formGroups(c.tickets, testNow, cfg)
			assert.Equal(t, c.want, groupIDs(got))
		})
	}
}

func TestFormGroups_should_not_group_duplicate_player(t *testing.T) {
	cfg := groupConfig{relaxAfter: 30 * time.Second, regionRelaxAfter: 60 * time.Second}
	// Two queued tickets share a player_id (defense in depth: even if the
	// one-active unique index is ever relaxed, a player must never appear
	// twice in one roster — that would let a lone player self-match).
	dup := func(id, playerID int64) *Ticket {
		return &Ticket{
			ID:               id,
			PlayerID:         playerID,
			Region:           "eu",
			MinCount:         2,
			MaxCount:         2,
			CountMultiple:    1,
			AllowCrossRegion: true,
			CreatedAt:        testNow.Add(-2 * time.Second),
		}
	}
	cases := []struct {
		name    string
		tickets []*Ticket
		want    [][]int64
	}{
		{
			"same player cannot fill its own group",
			[]*Ticket{dup(1, 99), dup(2, 99)},
			nil, // only player 99 present → no valid 2-player group
		},
		{
			"duplicate player is skipped, distinct player fills the group",
			[]*Ticket{dup(1, 99), dup(2, 99), dup(3, 7)},
			[][]int64{{1, 3}}, // ticket 2 (player 99, already in group) is skipped
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := formGroups(c.tickets, testNow, cfg)
			assert.Equal(t, c.want, groupIDs(got))
		})
	}
}

func TestFormGroups_region_semantics(t *testing.T) {
	cfg := groupConfig{relaxAfter: 30 * time.Second, regionRelaxAfter: 60 * time.Second}
	cases := []struct {
		name    string
		cfg     groupConfig
		tickets []*Ticket
		want    [][]int64
	}{
		{
			"same region groups before the window",
			cfg,
			[]*Ticket{
				gt(1, 2*time.Second, "eu", 2, 2, 1, true),
				gt(2, time.Second, "eu", 2, 2, 1, true),
				gt(3, time.Second, "us", 2, 2, 1, true),
			},
			[][]int64{{1, 2}},
		},
		{
			"different regions do not group before the window",
			cfg,
			[]*Ticket{
				gt(1, 2*time.Second, "eu", 2, 2, 1, true),
				gt(2, time.Second, "us", 2, 2, 1, true),
			},
			nil,
		},
		{
			"cross-region groups after the widen window",
			cfg,
			[]*Ticket{
				gt(1, 61*time.Second, "eu", 2, 2, 1, true),
				gt(2, time.Second, "us", 2, 2, 1, true),
			},
			[][]int64{{1, 2}},
		},
		{
			"allow_cross_region false never widens",
			cfg,
			[]*Ticket{
				gt(1, 61*time.Second, "eu", 2, 2, 1, false),
				gt(2, 61*time.Second, "us", 2, 2, 1, false),
			},
			nil,
		},
		{
			"empty region groups with anyone immediately",
			cfg,
			[]*Ticket{
				gt(1, 2*time.Second, "eu", 2, 2, 1, true),
				gt(2, time.Second, "", 2, 2, 1, true),
			},
			[][]int64{{1, 2}},
		},
		{
			"same region preferred even after widening",
			cfg,
			[]*Ticket{
				gt(1, 61*time.Second, "eu", 2, 2, 1, true),
				gt(2, 50*time.Second, "us", 2, 2, 1, true),
				gt(3, time.Second, "eu", 2, 2, 1, true),
			},
			// Widened bucket, but seed 1 still picks the same-region 3
			// first; 2 stays queued.
			[][]int64{{1, 3}},
		},
		{
			"widening disabled with zero window",
			groupConfig{relaxAfter: 30 * time.Second},
			[]*Ticket{
				gt(1, time.Hour, "eu", 2, 2, 1, true),
				gt(2, time.Hour, "us", 2, 2, 1, true),
			},
			nil,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := formGroups(c.tickets, testNow, c.cfg)
			assert.Equal(t, c.want, groupIDs(got))
		})
	}
}

func TestFormGroups_mutual_query_acceptance(t *testing.T) {
	mustParse := func(q string) query.Expr {
		e, err := query.Parse(q)
		if err != nil {
			t.Fatalf("parse %q: %v", q, err)
		}
		return e
	}
	base := groupConfig{relaxAfter: 30 * time.Second, regionRelaxAfter: 60 * time.Second}
	withRank := func(t_ *Ticket, rank float64) *Ticket {
		t_.NumericProperties = map[string]float64{"rank": rank}
		return t_
	}

	t.Run("one-way-compatible tickets never group", func(t *testing.T) {
		// 1 accepts 2 (rank 8 >= 5), but 2 requires rank >= 20 and 1 has 8.
		t1 := withRank(gt(1, 40*time.Second, "eu", 2, 2, 1, true), 8)
		t2 := withRank(gt(2, 40*time.Second, "eu", 2, 2, 1, true), 25)
		rejections := 0
		cfg := base
		cfg.queries = map[int64]query.Expr{
			1: mustParse("rank>=5"),
			2: mustParse("rank>=20"),
		}
		cfg.onQueryReject = func() { rejections++ }

		got := formGroups([]*Ticket{t1, t2}, testNow, cfg)

		assert.Nil(t, groupIDs(got))
		assert.Positive(t, rejections, "mutual rejection must be metered")
	})

	t.Run("mutually-compatible tickets group", func(t *testing.T) {
		t1 := withRank(gt(1, 40*time.Second, "eu", 2, 2, 1, true), 8)
		t2 := withRank(gt(2, 40*time.Second, "eu", 2, 2, 1, true), 9)
		cfg := base
		cfg.queries = map[int64]query.Expr{
			1: mustParse("rank>=5 AND rank<=10"),
			2: mustParse("rank>=5 AND rank<=10"),
		}

		got := formGroups([]*Ticket{t1, t2}, testNow, cfg)

		assert.Equal(t, [][]int64{{1, 2}}, groupIDs(got))
	})

	t.Run("query region is a hard constraint that never widens", func(t *testing.T) {
		// Both widen-eligible and past the window, but 1's query pins eu.
		t1 := gt(1, 2*time.Hour, "eu", 2, 2, 1, true)
		t2 := gt(2, 2*time.Hour, "us", 2, 2, 1, true)
		cfg := base
		cfg.queries = map[int64]query.Expr{1: mustParse("region:eu")}

		got := formGroups([]*Ticket{t1, t2}, testNow, cfg)

		assert.Nil(t, groupIDs(got))
	})

	t.Run("implicit game_mode property is queryable", func(t *testing.T) {
		t1 := gt(1, 40*time.Second, "eu", 2, 2, 1, true)
		t1.GameMode = "ranked"
		t2 := gt(2, 40*time.Second, "eu", 2, 2, 1, true)
		t2.GameMode = "ranked"
		cfg := base
		cfg.queries = map[int64]query.Expr{1: mustParse("game_mode:ranked")}

		got := formGroups([]*Ticket{t1, t2}, testNow, cfg)

		assert.Equal(t, [][]int64{{1, 2}}, groupIDs(got))
	})
}
