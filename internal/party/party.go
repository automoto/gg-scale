// Package party manages project-scoped groups that queue as one unit.
package party

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/json"
	"errors"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/automoto/gg-scale/internal/matchmaker/query"
)

// Stable errors are also the player API error codes.
var (
	ErrNotFound     = errors.New("party_not_found")
	ErrNotLeader    = errors.New("not_leader")
	ErrNotReady     = errors.New("not_ready")
	ErrFull         = errors.New("party_full")
	ErrBusy         = errors.New("party_busy")
	ErrStale        = errors.New("stale_version")
	ErrModeCapacity = errors.New("party_exceeds_mode_capacity")
	ErrActive       = errors.New("ticket_already_active")
	ErrMembership   = errors.New("party_already_active")
	ErrInvite       = errors.New("invalid_invite")
	ErrCooldown     = errors.New("code_redemption_cooldown")
)

// Settings are shared queue criteria. Properties remain per member.
type Settings struct {
	Mode             string `json:"mode"`
	FleetID          int64  `json:"fleet_id,omitempty"`
	Region           string `json:"region,omitempty"`
	GameMode         string `json:"game_mode,omitempty"`
	MinCount         int    `json:"min_count"`
	MaxCount         int    `json:"max_count"`
	CountMultiple    int    `json:"count_multiple"`
	AllowCrossRegion bool   `json:"allow_cross_region"`
	Query            string `json:"query,omitempty"`
}

// Member is an active party member; readiness is bound to the current roster.
type Member struct {
	TicketID           int64              `json:"ticket_id,omitempty"`
	PlayerID           int64              `json:"player_id"`
	ReadyVersion       int64              `json:"ready_version"`
	StringProperties   map[string]string  `json:"string_properties,omitempty"`
	NumericProperties  map[string]float64 `json:"numeric_properties,omitempty"`
	Attributes         json.RawMessage    `json:"attributes,omitempty"`
	JoinedAt           time.Time          `json:"joined_at"`
	LastSeenAt         time.Time          `json:"last_seen_at"`
	DisconnectDeadline time.Time          `json:"disconnect_deadline"`
}

// Party is the recoverable current party state.
type Party struct {
	ID                  int64    `json:"id"`
	ProjectID           int64    `json:"project_id"`
	LeaderID            int64    `json:"leader_id"`
	State               string   `json:"state"`
	Version             int64    `json:"version"`
	RosterVersion       int64    `json:"roster_version"`
	Settings            Settings `json:"settings"`
	MaxMembers          int      `json:"max_members"`
	CurrentQueueEntryID *int64   `json:"current_queue_entry_id,omitempty"`
	LastMatchID         string   `json:"last_match_id,omitempty"`
	Members             []Member `json:"members"`
}

func (p *Party) rosterChanged() {
	p.Version++
	p.RosterVersion++
	for i := range p.Members {
		p.Members[i].ReadyVersion = 0
	}
}

func (p *Party) canQueue(caller int64, now time.Time) error {
	if p.LeaderID != caller {
		return ErrNotLeader
	}
	if p.State != "idle" && p.State != "matched" {
		return ErrBusy
	}
	if len(p.Members) > p.Settings.MaxCount {
		return ErrModeCapacity
	}
	for _, m := range p.Members {
		if m.ReadyVersion != p.RosterVersion || !m.DisconnectDeadline.After(now) {
			return ErrNotReady
		}
	}
	return nil
}

func (p *Party) promote(now time.Time) {
	members := slices.Clone(p.Members)
	slices.SortFunc(members, func(a, b Member) int {
		if n := a.JoinedAt.Compare(b.JoinedAt); n != 0 {
			return n
		}
		if a.PlayerID < b.PlayerID {
			return -1
		}
		if a.PlayerID > b.PlayerID {
			return 1
		}
		return 0
	})
	for _, m := range members {
		if m.DisconnectDeadline.After(now) {
			p.LeaderID = m.PlayerID
			return
		}
	}
	p.State = "closed"
}

func newCode() (string, error) {
	var raw [10]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return base32.NewEncoding("0123456789ABCDEFGHJKMNPQRSTVWXYZ").WithPadding(base32.NoPadding).EncodeToString(raw[:]), nil
}

func codeHash(code string) []byte {
	sum := sha256.Sum256([]byte(strings.ToUpper(strings.ReplaceAll(code, "-", ""))))
	return sum[:]
}

// Validate checks settings before they reach the queue.
func (s Settings) Validate() error {
	if s.Mode != "match_only" && s.Mode != "game_session" && s.Mode != "fleet_allocation" {
		return errors.New("invalid_mode")
	}
	if s.MinCount < 1 || s.MaxCount < s.MinCount || s.MaxCount > math.MaxInt32 || s.CountMultiple < 1 || s.CountMultiple > s.MaxCount {
		return errors.New("invalid_counts")
	}
	if (s.Mode == "fleet_allocation") != (s.FleetID > 0) {
		return errors.New("invalid_fleet")
	}
	if len(s.Region) > 64 || len(s.GameMode) > 64 || len(s.Query) > 4096 {
		return errors.New("invalid_settings")
	}
	if s.Query != "" {
		_, err := query.Parse(s.Query)
		return err
	}
	return nil
}
