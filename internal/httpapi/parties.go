package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/automoto/gg-scale/internal/matchmaker"
	"github.com/automoto/gg-scale/internal/party"
	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
)

type partyVersion struct {
	ExpectedVersion int64 `json:"expected_version" minimum:"1"`
}
type partyIDInput struct {
	ID int64 `path:"id" minimum:"1"`
}
type partyInput[T any] struct {
	ID   int64 `path:"id" minimum:"1"`
	Body T
}
type partyOutput[T any] struct{ Body T }
type partySettingsBody struct {
	ExpectedVersion int64                   `json:"expected_version" minimum:"1"`
	Settings        matchmakerTicketRequest `json:"settings"`
}
type partyCreateInput struct {
	Body struct {
		Settings matchmakerTicketRequest `json:"settings"`
	}
}
type partyReadyInput struct {
	ID   int64 `path:"id" minimum:"1"`
	Body struct {
		ExpectedVersion int64           `json:"expected_version" minimum:"1"`
		Ready           bool            `json:"ready"`
		Properties      partyProperties `json:"properties"`
	}
}
type partyKickInput struct {
	ID       int64 `path:"id" minimum:"1"`
	PlayerID int64 `path:"player_id" minimum:"1"`
	Body     partyVersion
}
type partyCodeRevokeInput struct {
	ID     int64 `path:"id" minimum:"1"`
	CodeID int64 `path:"code_id" minimum:"1"`
	Body   partyVersion
}
type partyInviteBody struct {
	ExpectedVersion int64 `json:"expected_version" minimum:"1"`
	PlayerID        int64 `json:"player_id" minimum:"1"`
}
type partyCodeBody struct {
	ExpectedVersion int64 `json:"expected_version" minimum:"1"`
	MaxUses         int   `json:"max_uses" minimum:"1" maximum:"7" default:"7"`
}
type partyJoinInput struct {
	Body struct {
		Code string `json:"code" minLength:"16" maxLength:"64"`
	}
}
type partyQueueInput struct {
	ID   int64  `path:"id" minimum:"1"`
	Key  string `header:"Idempotency-Key" required:"true" minLength:"1" maxLength:"128"`
	Body partyVersion
}
type partyRematchInput struct {
	ID   int64  `path:"id" minimum:"1"`
	Key  string `header:"Idempotency-Key" required:"true" minLength:"1" maxLength:"128"`
	Body struct {
		ExpectedVersion int64  `json:"expected_version" minimum:"1"`
		LastMatchID     string `json:"last_match_id" minLength:"1"`
	}
}
type partyIPKey struct{}

func partyError(err error) error {
	if err == nil {
		return nil
	}
	var status huma.StatusError
	if errors.As(err, &status) {
		return err
	}
	switch {
	case errors.Is(err, party.ErrNotFound), errors.Is(err, party.ErrInvite):
		return huma.Error404NotFound(err.Error())
	case errors.Is(err, party.ErrNotLeader):
		return huma.Error403Forbidden(err.Error())
	case errors.Is(err, party.ErrCooldown):
		return huma.ErrorWithHeaders(huma.Error429TooManyRequests(err.Error()), http.Header{"Retry-After": []string{"900"}})
	case errors.Is(err, party.ErrFull), errors.Is(err, party.ErrBusy), errors.Is(err, party.ErrStale), errors.Is(err, party.ErrNotReady), errors.Is(err, party.ErrModeCapacity), errors.Is(err, party.ErrActive), errors.Is(err, party.ErrMembership):
		return huma.Error409Conflict(err.Error())
	default:
		return huma.Error500InternalServerError("internal error")
	}
}

func registerPartyOperation[I, O any](api huma.API, d Deps, id, method, path, summary string, fn func(context.Context, *party.Store, matchmakerContext, *I) (O, error)) {
	huma.Register(api, huma.Operation{OperationID: id, Method: method, Path: path, Summary: summary, Tags: []string{"Parties"}, Security: playerSecurity, MaxBodyBytes: 64 << 10,
		Middlewares: huma.Middlewares{func(ctx huma.Context, next func(huma.Context)) {
			req, _ := humachi.Unwrap(ctx)
			next(huma.WithValue(ctx, partyIPKey{}, d.ProxyTrust.ClientIP(req)))
		}},
	}, func(ctx context.Context, in *I) (*partyOutput[O], error) {
		mc, err := resolveMatchmakerContext(ctx)
		if err != nil {
			return nil, err
		}
		if err = authorizeMatchmaker(ctx, d, mc); err != nil {
			return nil, err
		}
		if d.Pool == nil {
			return nil, huma.Error503ServiceUnavailable("parties unavailable")
		}
		result, err := fn(ctx, party.NewStore(d.Pool), mc, in)
		if err != nil {
			return nil, partyError(err)
		}
		return &partyOutput[O]{Body: result}, nil
	})
}

func partySettings(ctx context.Context, d Deps, mc matchmakerContext, req matchmakerTicketRequest) (party.Settings, error) {
	mode, problem := resolveTicketMode(&req)
	if problem != "" {
		return party.Settings{}, huma.Error400BadRequest(problem)
	}
	var fleetID int64
	if mode == matchmaker.ModeFleetAllocation {
		var err error
		fleetID, err = resolveFleetForTicket(ctx, d, mc, req.Fleet)
		if err != nil {
			return party.Settings{}, err
		}
	}
	settings := party.Settings{Mode: string(mode), FleetID: fleetID, Region: req.Region, GameMode: req.GameMode, MinCount: req.MinCount, MaxCount: req.MaxCount, CountMultiple: req.CountMultiple, AllowCrossRegion: req.AllowCrossRegion == nil || *req.AllowCrossRegion, Query: req.Query}
	if err := settings.Validate(); err != nil {
		return settings, huma.Error400BadRequest(err.Error())
	}
	return settings, nil
}

func registerPartyRoutes(api huma.API, d Deps) {
	registerPartyOperation(api, d, "createParty", http.MethodPost, "/v1/parties", "Create a party", func(ctx context.Context, s *party.Store, mc matchmakerContext, in *partyCreateInput) (*party.Party, error) {
		settings, err := partySettings(ctx, d, mc, in.Body.Settings)
		if err != nil {
			return nil, err
		}
		return s.Create(ctx, mc.projectID, mc.playerID, settings)
	})
	registerPartyOperation(api, d, "getCurrentParty", http.MethodGet, "/v1/parties/current", "Recover the current party", func(ctx context.Context, s *party.Store, mc matchmakerContext, _ *struct{}) (*party.Party, error) {
		return s.Get(ctx, mc.projectID, 0, mc.playerID)
	})
	registerPartyOperation(api, d, "getParty", http.MethodGet, "/v1/parties/{id}", "Get a party", func(ctx context.Context, s *party.Store, mc matchmakerContext, in *partyIDInput) (*party.Party, error) {
		return s.Get(ctx, mc.projectID, in.ID, mc.playerID)
	})
	registerPartyOperation(api, d, "updateParty", http.MethodPatch, "/v1/parties/{id}", "Set party queue settings", func(ctx context.Context, s *party.Store, mc matchmakerContext, in *partyInput[partySettingsBody]) (*party.Party, error) {
		settings, err := partySettings(ctx, d, mc, in.Body.Settings)
		if err != nil {
			return nil, err
		}
		return s.Update(ctx, mc.projectID, in.ID, mc.playerID, in.Body.ExpectedVersion, settings)
	})
	registerPartyOperation(api, d, "disbandParty", http.MethodDelete, "/v1/parties/{id}", "Disband a party", func(ctx context.Context, s *party.Store, mc matchmakerContext, in *partyInput[partyVersion]) (*party.Party, error) {
		return s.Remove(ctx, mc.projectID, in.ID, mc.playerID, in.Body.ExpectedVersion, mc.playerID, true)
	})
	registerPartyOperation(api, d, "leaveParty", http.MethodDelete, "/v1/parties/{id}/members/me", "Leave a party", func(ctx context.Context, s *party.Store, mc matchmakerContext, in *partyInput[partyVersion]) (*party.Party, error) {
		return s.Remove(ctx, mc.projectID, in.ID, mc.playerID, in.Body.ExpectedVersion, mc.playerID, false)
	})
	registerPartyOperation(api, d, "kickPartyMember", http.MethodDelete, "/v1/parties/{id}/members/{player_id}", "Remove a party member", func(ctx context.Context, s *party.Store, mc matchmakerContext, in *partyKickInput) (*party.Party, error) {
		return s.Remove(ctx, mc.projectID, in.ID, mc.playerID, in.Body.ExpectedVersion, in.PlayerID, false)
	})
	registerPartyOperation(api, d, "readyPartyMember", http.MethodPut, "/v1/parties/{id}/members/me/ready", "Set your readiness and properties", func(ctx context.Context, s *party.Store, mc matchmakerContext, in *partyReadyInput) (*party.Party, error) {
		req := matchmakerTicketRequest{StringProperties: in.Body.Properties.StringProperties, NumericProperties: in.Body.Properties.NumericProperties, Attributes: in.Body.Properties.Attributes}
		if problem := validateTicketCriteria(&req); problem != "" {
			return nil, huma.Error400BadRequest(problem)
		}
		if len(req.Attributes) > maxAttributesBytes {
			return nil, huma.Error400BadRequest("attributes too large")
		}
		return s.Ready(ctx, mc.projectID, in.ID, mc.playerID, in.Body.ExpectedVersion, in.Body.Ready, party.Member{StringProperties: req.StringProperties, NumericProperties: req.NumericProperties, Attributes: req.Attributes})
	})
	registerPartyOperation(api, d, "heartbeatParty", http.MethodPost, "/v1/parties/{id}/heartbeat", "Extend party presence", func(ctx context.Context, s *party.Store, mc matchmakerContext, in *partyInput[partyVersion]) (*party.Party, error) {
		return s.Heartbeat(ctx, mc.projectID, in.ID, mc.playerID, in.Body.ExpectedVersion)
	})
	registerPartyOperation(api, d, "invitePartyFriend", http.MethodPost, "/v1/parties/{id}/invites", "Invite a friend to the party", func(ctx context.Context, s *party.Store, mc matchmakerContext, in *partyInput[partyInviteBody]) (*party.Invite, error) {
		return s.InviteFriend(ctx, mc.projectID, in.ID, mc.playerID, in.Body.ExpectedVersion, in.Body.PlayerID)
	})
	registerPartyOperation(api, d, "listPartyInvites", http.MethodGet, "/v1/party-invites", "List your pending party invites", func(ctx context.Context, s *party.Store, mc matchmakerContext, _ *struct{}) ([]party.Invite, error) {
		return s.Invites(ctx, mc.projectID, mc.playerID)
	})
	registerPartyOperation(api, d, "acceptPartyInvite", http.MethodPost, "/v1/party-invites/{id}/accept", "Accept a party invite", func(ctx context.Context, s *party.Store, mc matchmakerContext, in *partyInput[partyVersion]) (*party.Party, error) {
		return s.ResolveInvite(ctx, mc.projectID, in.ID, mc.playerID, in.Body.ExpectedVersion, true)
	})
	registerPartyOperation(api, d, "declinePartyInvite", http.MethodDelete, "/v1/party-invites/{id}", "Decline or revoke a party invite", func(ctx context.Context, s *party.Store, mc matchmakerContext, in *partyInput[partyVersion]) (*party.Party, error) {
		_, err := s.ResolveInvite(ctx, mc.projectID, in.ID, mc.playerID, in.Body.ExpectedVersion, false)
		// A declining invitee must not receive the private roster.
		return nil, err
	})
	registerPartyOperation(api, d, "createPartyCode", http.MethodPost, "/v1/parties/{id}/invite-codes", "Create or rotate a party invite code", func(ctx context.Context, s *party.Store, mc matchmakerContext, in *partyInput[partyCodeBody]) (*party.Code, error) {
		return s.CreateCode(ctx, mc.projectID, in.ID, mc.playerID, in.Body.ExpectedVersion, in.Body.MaxUses)
	})
	registerPartyOperation(api, d, "revokePartyCode", http.MethodDelete, "/v1/parties/{id}/invite-codes/{code_id}", "Revoke a party invite code", func(ctx context.Context, s *party.Store, mc matchmakerContext, in *partyCodeRevokeInput) (*party.Party, error) {
		return s.RevokeCode(ctx, mc.projectID, in.ID, mc.playerID, in.Body.ExpectedVersion, in.CodeID)
	})
	registerPartyOperation(api, d, "joinPartyCode", http.MethodPost, "/v1/parties/join", "Join a party by invite code", func(ctx context.Context, s *party.Store, mc matchmakerContext, in *partyJoinInput) (*party.Party, error) {
		ip, _ := ctx.Value(partyIPKey{}).(string)
		return s.JoinCode(ctx, mc.projectID, mc.playerID, in.Body.Code, ip)
	})
	registerPartyOperation(api, d, "queueParty", http.MethodPost, "/v1/parties/{id}/queue", "Queue the ready party", func(ctx context.Context, s *party.Store, mc matchmakerContext, in *partyQueueInput) (*party.Party, error) {
		if !d.PartyEnqueueEnabled {
			return nil, huma.Error503ServiceUnavailable("party_enqueue_disabled")
		}
		if err := authorizePartyQueue(ctx, s, d, mc, in.ID); err != nil {
			return nil, err
		}
		return s.Queue(ctx, mc.projectID, in.ID, mc.playerID, in.Body.ExpectedVersion, in.Key, "", d.MatchmakerTicketTTL)
	})
	registerPartyOperation(api, d, "cancelPartyQueue", http.MethodDelete, "/v1/parties/{id}/queue", "Cancel the whole party queue entry", func(ctx context.Context, s *party.Store, mc matchmakerContext, in *partyInput[partyVersion]) (*party.Party, error) {
		return s.Cancel(ctx, mc.projectID, in.ID, mc.playerID, in.Body.ExpectedVersion)
	})
	registerPartyOperation(api, d, "rematchParty", http.MethodPost, "/v1/parties/{id}/rematch", "Queue the current party for another match", func(ctx context.Context, s *party.Store, mc matchmakerContext, in *partyRematchInput) (*party.Party, error) {
		if !d.PartyEnqueueEnabled {
			return nil, huma.Error503ServiceUnavailable("party_enqueue_disabled")
		}
		if err := authorizePartyQueue(ctx, s, d, mc, in.ID); err != nil {
			return nil, err
		}
		return s.Queue(ctx, mc.projectID, in.ID, mc.playerID, in.Body.ExpectedVersion, in.Key, in.Body.LastMatchID, d.MatchmakerTicketTTL)
	})
}

type partyProperties struct {
	StringProperties  map[string]string  `json:"string_properties,omitempty"`
	NumericProperties map[string]float64 `json:"numeric_properties,omitempty"`
	Attributes        json.RawMessage    `json:"attributes,omitempty"`
}

func authorizePartyQueue(ctx context.Context, s *party.Store, d Deps, mc matchmakerContext, id int64) error {
	p, err := s.Get(ctx, mc.projectID, id, mc.playerID)
	if err != nil {
		return err
	}
	if p.Settings.Mode != "fleet_allocation" {
		return nil
	}
	if d.Fleet == nil {
		return huma.Error503ServiceUnavailable("fleet backend not configured")
	}
	f, err := d.Fleet.Fleets().GetByID(ctx, p.Settings.FleetID)
	if err != nil || f.ProjectID != mc.projectID {
		return huma.Error400BadRequest("unknown fleet")
	}
	_, err = resolveFleetForTicket(ctx, d, mc, f.Name)
	return err
}
