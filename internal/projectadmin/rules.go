package projectadmin

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/automoto/gg-scale/internal/period"
)

// Size limits for the JSON objects that the dashboard and the MCP tools
// write. The database allows more; these are the editor limits.
const (
	RemoteConfigMaxBytes = 64 << 10
	// LeaderboardMetadataMaxBytes is smaller than remote config: every
	// /v1/leaderboards reply carries the metadata of every board.
	LeaderboardMetadataMaxBytes = 16 << 10
	LeaderboardNameMax          = 120
)

// ErrInvalidLeaderboard wraps every leaderboard settings failure. The
// wrapped message is safe to show the person or the agent.
var ErrInvalidLeaderboard = errors.New("invalid leaderboard settings")

// NormalizeJSONObject checks a single top-level JSON object (no trailing
// data), encodes it again in canonical form, and applies maxBytes to that
// form. Numbers keep their exact text.
func NormalizeJSONObject(raw string, maxBytes int) ([]byte, error) {
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	var blob map[string]any
	if err := dec.Decode(&blob); err != nil || blob == nil {
		return nil, errors.New("not a JSON object")
	}
	if err := dec.Decode(new(any)); !errors.Is(err, io.EOF) {
		return nil, errors.New("trailing data after the JSON object")
	}
	encoded, err := json.Marshal(blob)
	if err != nil {
		return nil, err
	}
	if len(encoded) > maxBytes {
		return nil, fmt.Errorf("JSON object larger than %d KiB", maxBytes>>10)
	}
	return encoded, nil
}

// ValidLeaderboardName rejects names PostgreSQL cannot store or that are
// too long. A NUL byte raises SQLSTATE 22021, which would show as a 500
// instead of a field error. Invalid UTF-8 is checked first because the rune
// loop below would see it as U+FFFD.
func ValidLeaderboardName(name string) bool {
	if name == "" || !utf8.ValidString(name) || utf8.RuneCountInString(name) > LeaderboardNameMax {
		return false
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// ValidateLeaderboard checks settings that did not come from the dashboard
// form. The form parser gives field-level messages for the same rules.
// ScoreOperator is checked only on create; it is fixed after that.
func ValidateLeaderboard(s LeaderboardSettings, create bool) error {
	invalid := func(msg string) error { return fmt.Errorf("%w: %s", ErrInvalidLeaderboard, msg) }
	switch {
	case !ValidLeaderboardName(s.Name):
		return invalid(fmt.Sprintf("name must be 1 to %d characters with no control characters", LeaderboardNameMax))
	case s.SortOrder != "asc" && s.SortOrder != "desc":
		return invalid("sort_order must be asc or desc")
	case create && s.ScoreOperator != "best" && s.ScoreOperator != "set" && s.ScoreOperator != "incr":
		return invalid("score_operator must be best, set, or incr")
	case !period.ValidSchedule(s.ResetSchedule):
		return invalid("reset_schedule must be none, daily, weekly, or monthly")
	case s.ScoreMin != nil && s.ScoreMax != nil && *s.ScoreMin > *s.ScoreMax:
		return invalid("score_min must not be more than score_max")
	case s.AttemptCap != nil && *s.AttemptCap <= 0:
		return invalid("attempt_cap must be a positive whole number")
	}
	return nil
}
