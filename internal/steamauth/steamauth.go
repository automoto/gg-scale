// Package steamauth verifies Steam session tickets against Valve's
// ISteamUserAuth Web API. It is the sign-in backend for POST /v1/auth/steam:
// the game client obtains a ticket from the Steamworks SDK and the server
// exchanges it here for the player's SteamID64.
package steamauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// DefaultBaseURL is Valve's Web API host; tests point BaseURL at a fake.
const DefaultBaseURL = "https://api.steampowered.com"

// WebAPIIdentity is the identity string game clients must pass to
// ISteamUser::GetAuthTicketForWebApi. Sending it with the verify call binds
// the ticket to this service, so a ticket minted for another consumer is
// rejected by Valve.
const WebAPIIdentity = "ggscale"

const (
	verifyPath     = "/ISteamUserAuth/AuthenticateUserTicket/v1/"
	defaultTimeout = 5 * time.Second
	// maxResponseBytes caps the Valve response read; the real payload is a
	// few hundred bytes.
	maxResponseBytes = 1 << 20
)

var (
	// ErrInvalidTicket means Valve examined and rejected the ticket.
	ErrInvalidTicket = errors.New("steamauth: ticket rejected by steam")
	// ErrUnavailable means the ticket could not be examined: Valve was
	// unreachable, errored, or answered with an unusable body.
	ErrUnavailable = errors.New("steamauth: steam web api unavailable")
)

// Result is Valve's verdict on a valid ticket. SteamID identifies the playing
// account; OwnerSteamID differs from it under Family Sharing (the license
// owner). Callers key identity on SteamID.
type Result struct {
	SteamID         string
	OwnerSteamID    string
	VACBanned       bool
	PublisherBanned bool
}

// Verifier is the seam handlers depend on; tests substitute fakes.
type Verifier interface {
	Verify(ctx context.Context, appID, webAPIKey, ticketHex string) (Result, error)
}

// Client verifies tickets against the real (or a fake) Steam Web API.
type Client struct {
	// BaseURL overrides DefaultBaseURL (tests).
	BaseURL string
	// HTTPClient overrides the default 5s-timeout client.
	HTTPClient *http.Client
}

// New returns a production Client.
func New() *Client {
	return &Client{}
}

// verifyResponse covers both Valve body shapes: params on success, error when
// the ticket is rejected.
type verifyResponse struct {
	Response struct {
		Params *struct {
			Result          string `json:"result"`
			SteamID         string `json:"steamid"`
			OwnerSteamID    string `json:"ownersteamid"`
			VACBanned       bool   `json:"vacbanned"`
			PublisherBanned bool   `json:"publisherbanned"`
		} `json:"params"`
		Error *struct {
			ErrorCode int    `json:"errorcode"`
			ErrorDesc string `json:"errordesc"`
		} `json:"error"`
	} `json:"response"`
}

// Verify exchanges a session ticket for the caller's SteamID64. Transport
// errors are never wrapped verbatim: the request URL carries the Web API key,
// and Go's *url.Error stringifies the full URL.
func (c *Client) Verify(ctx context.Context, appID, webAPIKey, ticketHex string) (Result, error) {
	base := c.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	client := c.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: defaultTimeout}
	}

	q := url.Values{}
	q.Set("key", webAPIKey)
	q.Set("appid", appID)
	q.Set("ticket", ticketHex)
	q.Set("identity", WebAPIIdentity)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+verifyPath+"?"+q.Encode(), nil)
	if err != nil {
		return Result{}, fmt.Errorf("%w: build request", ErrUnavailable)
	}
	resp, err := client.Do(req)
	if err != nil {
		return Result{}, fmt.Errorf("%w: request failed", ErrUnavailable)
	}
	defer resp.Body.Close() //nolint:errcheck // read-only body

	if resp.StatusCode != http.StatusOK {
		return Result{}, fmt.Errorf("%w: status %d", ErrUnavailable, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return Result{}, fmt.Errorf("%w: read body", ErrUnavailable)
	}
	var parsed verifyResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return Result{}, fmt.Errorf("%w: unexpected body", ErrUnavailable)
	}
	if parsed.Response.Error != nil {
		return Result{}, fmt.Errorf("%w: code %d", ErrInvalidTicket, parsed.Response.Error.ErrorCode)
	}
	params := parsed.Response.Params
	if params == nil || params.SteamID == "" {
		return Result{}, fmt.Errorf("%w: unexpected body", ErrUnavailable)
	}
	return Result{
		SteamID:         params.SteamID,
		OwnerSteamID:    params.OwnerSteamID,
		VACBanned:       params.VACBanned,
		PublisherBanned: params.PublisherBanned,
	}, nil
}
