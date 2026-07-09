package httpapi

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"golang.org/x/crypto/bcrypt"

	"github.com/ggscale/ggscale/internal/auditlog"
	"github.com/ggscale/ggscale/internal/auth"
	"github.com/ggscale/ggscale/internal/db"
	sqlcgen "github.com/ggscale/ggscale/internal/db/sqlc"
	"github.com/ggscale/ggscale/internal/mailer"
	"github.com/ggscale/ggscale/internal/observability"
	"github.com/ggscale/ggscale/internal/verifycode"
	"github.com/ggscale/ggscale/internal/webutil"
)

const (
	accessTokenTTL       = 15 * time.Minute
	refreshTokenTTL      = 30 * 24 * time.Hour
	signupNoticeCooldown = 15 * time.Minute
	maxPasswordBytes     = 72
	mailerVerifySubject  = "Verify your ggscale email"
	mailerVerifyBodyTmpl = "Your ggscale verification code is %s (valid 15 minutes)."
)

func apiNow(d Deps) time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

// bcryptCost is webutil.BcryptCost re-bound locally so existing call
// sites stay untouched after the helper extraction.
const bcryptCost = webutil.BcryptCost

type anonymousResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	PlayerID     int64  `json:"player_id"`
	ExternalID   string `json:"external_id"`
	ExpiresAt    string `json:"expires_at"`
}

type sessionResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	PlayerID     int64  `json:"player_id"`
	ExpiresAt    string `json:"expires_at"`
}

// signup/login fields stay schema-optional: signup enforces password byte
// length (bcrypt's 72-byte limit — a rune-counting schema would be wrong) and a
// deliberately-vague combined 400; login funnels every malformed/mismatched
// input to a uniform 401 so the request shape reveals nothing. Both own their
// validation in the handler.
type signupRequest struct {
	Email    string `json:"email,omitempty"`
	Password string `json:"password,omitempty"`
}

type verifyRequest struct {
	Email string `json:"email" minLength:"1"`
	Code  string `json:"code" minLength:"1"`
}

type loginRequest struct {
	Email    string `json:"email,omitempty"`
	Password string `json:"password,omitempty"`
}

type refreshRequest struct {
	RefreshToken string `json:"refresh_token" minLength:"1"`
}

type logoutRequest struct {
	RefreshToken string `json:"refresh_token" minLength:"1"`
}

type customTokenRequest struct {
	Token string `json:"token" minLength:"1"`
}

type anonymousOutput struct {
	Body anonymousResponse
}

type signupInput struct {
	Body signupRequest
}

type verifyInput struct {
	Body verifyRequest
}

type verifyResult struct {
	PlayerID int64 `json:"player_id"`
	Verified bool  `json:"verified"`
}

type verifyOutput struct {
	Body verifyResult
}

type loginInput struct {
	Body loginRequest
}

type sessionOutput struct {
	Body sessionResponse
}

type refreshInput struct {
	Body refreshRequest
}

type logoutInput struct {
	Body logoutRequest
}

type customTokenInput struct {
	Body customTokenRequest
}

// registerAuthRoutes registers the tenant-scoped, player-anonymous
// /v1/auth/* operations. They share the per-IP rate-limiter group the adapter
// binds to.
func registerAuthRoutes(api huma.API, d Deps) {
	huma.Register(api, huma.Operation{
		OperationID: "authAnonymous",
		Method:      http.MethodPost,
		Path:        "/v1/auth/anonymous",
		Summary:     "Create an anonymous player session",
		Tags:        []string{"/v1"},
		Security:    apiKeySecurity,
	}, authAnonymous(d))

	huma.Register(api, huma.Operation{
		OperationID:   "authSignup",
		Method:        http.MethodPost,
		Path:          "/v1/auth/signup",
		Summary:       "Sign up with email and password",
		Tags:          []string{"/v1"},
		Security:      apiKeySecurity,
		DefaultStatus: http.StatusAccepted,
	}, authSignup(d))

	huma.Register(api, huma.Operation{
		OperationID: "authVerify",
		Method:      http.MethodPost,
		Path:        "/v1/auth/verify",
		Summary:     "Verify an email address with a code",
		Tags:        []string{"/v1"},
		Security:    apiKeySecurity,
	}, authVerify(d))

	huma.Register(api, huma.Operation{
		OperationID: "authLogin",
		Method:      http.MethodPost,
		Path:        "/v1/auth/login",
		Summary:     "Log in with email and password",
		Tags:        []string{"/v1"},
		Security:    apiKeySecurity,
	}, authLogin(d))

	huma.Register(api, huma.Operation{
		OperationID: "authRefresh",
		Method:      http.MethodPost,
		Path:        "/v1/auth/refresh",
		Summary:     "Rotate a refresh token",
		Tags:        []string{"/v1"},
		Security:    apiKeySecurity,
	}, authRefresh(d))

	huma.Register(api, huma.Operation{
		OperationID:   "authLogout",
		Method:        http.MethodPost,
		Path:          "/v1/auth/logout",
		Summary:       "Revoke a refresh token",
		Tags:          []string{"/v1"},
		Security:      apiKeySecurity,
		DefaultStatus: http.StatusNoContent,
	}, authLogout(d))

	huma.Register(api, huma.Operation{
		OperationID: "authCustomToken",
		Method:      http.MethodPost,
		Path:        "/v1/auth/custom-token",
		Summary:     "Exchange a tenant-signed token for a session",
		Tags:        []string{"/v1"},
		Security:    apiKeySecurity,
	}, authCustomToken(d))
}

func authAnonymous(d Deps) func(context.Context, *struct{}) (*anonymousOutput, error) {
	return func(ctx context.Context, _ *struct{}) (*anonymousOutput, error) {
		projectID, tenantID, err := pinnedProject(ctx)
		if err != nil {
			return nil, err
		}

		externalID, err := webutil.RandomHex("anon_", 16)
		if err != nil {
			return nil, serverError(ctx, "anonymous: external_id rand", err)
		}
		refreshToken, err := webutil.RandomHex("", 32)
		if err != nil {
			return nil, serverError(ctx, "anonymous: refresh rand", err)
		}

		now := apiNow(d)
		accessExpiresAt := now.Add(accessTokenTTL)
		var playerID int64
		err = d.Pool.Q(ctx, func(tx pgx.Tx) error {
			q := sqlcgen.New(tx)
			user, err := q.CreateAnonymousPlayer(ctx, sqlcgen.CreateAnonymousPlayerParams{
				ProjectID:  projectID,
				ExternalID: externalID,
			})
			if err != nil {
				return fmt.Errorf("insert player: %w", err)
			}
			playerID = user.ID
			if err := insertSession(ctx, tx, projectID, user.ID, refreshToken, now); err != nil {
				return err
			}
			return auditlog.Write(ctx, tx, user.ID, "auth.anonymous", "", map[string]any{"external_id": externalID})
		})
		if err != nil {
			return nil, serverError(ctx, "anonymous: tx", err)
		}

		accessToken, err := d.Signer.Sign(auth.Claims{
			PlayerID: playerID, TenantID: tenantID, ProjectID: projectID,
			ExpiresAt: accessExpiresAt,
		})
		if err != nil {
			return nil, serverError(ctx, "anonymous: sign", err)
		}

		return &anonymousOutput{Body: anonymousResponse{
			AccessToken: accessToken, RefreshToken: refreshToken,
			PlayerID: playerID, ExternalID: externalID,
			ExpiresAt: accessExpiresAt.UTC().Format(time.RFC3339),
		}}, nil
	}
}

func authSignup(d Deps) func(context.Context, *signupInput) (*struct{}, error) {
	return func(ctx context.Context, in *signupInput) (*struct{}, error) {
		if !validateEmail(in.Body.Email) || !validPassword(in.Body.Password) {
			return nil, huma.Error400BadRequest("email or password invalid")
		}

		projectID, _, err := pinnedProject(ctx)
		if err != nil {
			return nil, err
		}

		hash, err := bcrypt.GenerateFromPassword([]byte(in.Body.Password), bcryptCost)
		if err != nil {
			return nil, serverError(ctx, "signup: bcrypt", err)
		}
		code, err := verifycode.GenerateCode()
		if err != nil {
			return nil, serverError(ctx, "signup: code", err)
		}
		salt, err := verifycode.NewSalt()
		if err != nil {
			return nil, serverError(ctx, "signup: salt", err)
		}
		codeHash := verifycode.Hash(salt, code)
		externalID, err := webutil.RandomHex("user_", 16)
		if err != nil {
			return nil, serverError(ctx, "signup: ext_id rand", err)
		}
		now := apiNow(d)

		err = d.Pool.Q(ctx, func(tx pgx.Tx) error {
			q := sqlcgen.New(tx)
			email := in.Body.Email
			expires := pgtype.Timestamptz{Time: now.Add(verifycode.CodeTTL), Valid: true}
			id, err := q.CreateEmailPlayer(ctx, sqlcgen.CreateEmailPlayerParams{
				ProjectID:                  projectID,
				ExternalID:                 externalID,
				Email:                      &email,
				PasswordHash:               hash,
				EmailVerificationCodeHash:  codeHash,
				EmailVerificationSalt:      salt,
				EmailVerificationExpiresAt: expires,
			})
			if err != nil {
				return fmt.Errorf("insert player: %w", err)
			}
			return auditlog.Write(ctx, tx, id, "auth.signup", email, nil)
		})
		if err != nil {
			if webutil.IsUniqueViolation(err) {
				// Uniform 202 on both insert and conflict so the response
				// status doesn't disclose whether the email already has an
				// account (account-enumeration oracle). The "you already
				// have an account" hint goes to the address itself, which
				// only the legitimate owner can read. The per-recipient
				// cooldown stops a hostile caller from turning repeated
				// duplicate signups into an email flood against that address.
				if d.Mailer != nil && noticeThrottleAllows(ctx, d, projectID, in.Body.Email) {
					existing := mailer.Message{
						From: d.MailFrom, To: []string{in.Body.Email},
						Subject: "Your ggscale account",
						Body:    "Someone tried to sign up using this email. If that was you, sign in directly — your account already exists.",
					}
					if err := d.Mailer.Send(ctx, existing); err != nil {
						slog.Error("signup: existing-account mailer", "error", err)
					}
				}
				return nil, nil
			}
			return nil, serverError(ctx, "signup: tx", err)
		}
		d.Metrics.Signup(observability.SignupPlayer)

		if d.Mailer != nil {
			msg := mailer.Message{
				From: d.MailFrom, To: []string{in.Body.Email},
				Subject: mailerVerifySubject,
				Body:    fmt.Sprintf(mailerVerifyBodyTmpl, code),
			}
			if err := d.Mailer.Send(ctx, msg); err != nil {
				slog.Error("signup: mailer", "error", err)
			}
		}

		return nil, nil
	}
}

// authVerify accepts {email, code} and flips the player to verified when the
// code matches. Wrong guesses burn the per-code attempt budget (the bump is
// committed even though the request fails); the lifetime budget survives
// resends and trips a 24h lock. Already-verified accounts get the same uniform
// 400 as unknown emails: once verified the code hash is cleared, so no code can
// be checked, and a distinguishable 200 would confirm "this email is a verified
// account here" (and leak its player_id) to any publishable-key holder —
// undoing the anti-enumeration cost signup and login already pay.
func authVerify(d Deps) func(context.Context, *verifyInput) (*verifyOutput, error) {
	return func(ctx context.Context, in *verifyInput) (*verifyOutput, error) {
		projectID, _, err := pinnedProject(ctx)
		if err != nil {
			return nil, err
		}
		now := apiNow(d)

		var (
			playerID           int64
			badCode            bool
			lockedAfterAttempt bool
		)
		err = d.Pool.Q(ctx, func(tx pgx.Tx) error {
			q := sqlcgen.New(tx)
			email := in.Body.Email
			row, err := q.GetPlayerVerificationState(ctx, sqlcgen.GetPlayerVerificationStateParams{
				ProjectID: projectID,
				Email:     &email,
			})
			if err != nil {
				return err
			}
			if row.EmailVerifiedAt.Valid {
				return errVerifyBadCode
			}
			if row.EmailVerificationLockedUntil.Valid && verifycode.AccountLocked(row.EmailVerificationLockedUntil.Time, now) {
				return errVerifyAccountLocked
			}
			if verifycode.Expired(row.EmailVerificationExpiresAt.Time, now) {
				return errVerifyExpired
			}
			if len(row.EmailVerificationSalt) == 0 || len(row.EmailVerificationCodeHash) == 0 {
				return errVerifyExpired
			}
			// Atomic check-and-bump replaces the prior fetch-then-increment
			// pattern that could undercount concurrent wrong codes.
			reserved, err := q.ReservePlayerVerifyAttempt(ctx, sqlcgen.ReservePlayerVerifyAttemptParams{
				ID:          row.ID,
				MaxAttempts: int32(verifycode.MaxAttempts),
			})
			if err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return errVerifyExhausted
				}
				return err
			}
			// Lifetime cap survives /resend; trip the long lockout. The
			// Lock write happens in the same tx as the Reserve increment
			// so the bump that crossed the cap and the lock both commit
			// together; we return nil here and surface the lock via the
			// dedicated guard at the head of the closure on the next call.
			if verifycode.LifetimeExhausted(int(reserved.EmailVerificationLifetimeAttempts)) {
				lockedUntil := pgtype.Timestamptz{Time: now.Add(verifycode.LockoutDuration), Valid: true}
				if lerr := q.LockPlayerVerification(ctx, sqlcgen.LockPlayerVerificationParams{
					ID: row.ID, LockedUntil: lockedUntil,
				}); lerr != nil {
					return lerr
				}
				lockedAfterAttempt = true
				return nil
			}
			expected := verifycode.Hash(row.EmailVerificationSalt, in.Body.Code)
			if subtle.ConstantTimeCompare(expected, row.EmailVerificationCodeHash) != 1 {
				// A wrong code must COMMIT, not error out: an error would
				// roll the ReservePlayerVerifyAttempt bump back with the
				// rest of the tx, resetting both counters and handing an
				// attacker unlimited guesses at the 10^6 code space. The
				// 400 is raised from the flag once the tx has committed.
				badCode = true
				return nil
			}
			if err := q.MarkPlayerVerified(ctx, row.ID); err != nil {
				return err
			}
			playerID = row.ID
			return auditlog.Write(ctx, tx, row.ID, "auth.verify", "", nil)
		})
		switch {
		case errors.Is(err, pgx.ErrNoRows), errors.Is(err, errVerifyBadCode), errors.Is(err, errVerifyExpired):
			d.Metrics.Verification(observability.VerifyInvalid)
			return nil, huma.Error400BadRequest("invalid email or code")
		case errors.Is(err, errVerifyExhausted):
			d.Metrics.Verification(observability.VerifyThrottled)
			return nil, huma.Error429TooManyRequests("too many attempts")
		case errors.Is(err, errVerifyAccountLocked):
			d.Metrics.Verification(observability.VerifyThrottled)
			return nil, huma.Error429TooManyRequests("account locked, contact support")
		case err != nil:
			return nil, serverError(ctx, "verify: tx", err)
		}
		if lockedAfterAttempt {
			d.Metrics.Verification(observability.VerifyThrottled)
			return nil, huma.Error429TooManyRequests("account locked, contact support")
		}
		if badCode {
			d.Metrics.Verification(observability.VerifyInvalid)
			return nil, huma.Error400BadRequest("invalid email or code")
		}

		d.Metrics.Verification(observability.VerifyOK)
		return &verifyOutput{Body: verifyResult{PlayerID: playerID, Verified: true}}, nil
	}
}

func authLogin(d Deps) func(context.Context, *loginInput) (*sessionOutput, error) {
	return func(ctx context.Context, in *loginInput) (*sessionOutput, error) {
		if !validPassword(in.Body.Password) {
			return nil, huma.Error401Unauthorized("invalid credentials")
		}

		projectID, tenantID, err := pinnedProject(ctx)
		if err != nil {
			return nil, err
		}

		refreshToken, err := webutil.RandomHex("", 32)
		if err != nil {
			return nil, serverError(ctx, "login: refresh rand", err)
		}
		now := apiNow(d)
		var playerID, sessionEpoch int64
		err = d.Pool.Q(ctx, func(tx pgx.Tx) error {
			q := sqlcgen.New(tx)
			email := in.Body.Email
			row, err := q.GetPlayerByEmail(ctx, sqlcgen.GetPlayerByEmailParams{
				ProjectID: projectID,
				Email:     &email,
			})
			if err != nil {
				return err
			}
			if bcrypt.CompareHashAndPassword(row.PasswordHash, []byte(in.Body.Password)) != nil {
				return errBadCredentials
			}
			// Email verification gate: checked only after the password matches so
			// an attacker can't probe verification state without valid credentials.
			if !row.EmailVerifiedAt.Valid {
				return errEmailUnverified
			}
			if err := playerBanCheck(ctx, q, row.ID); err != nil {
				return err
			}
			playerID = row.ID
			if sessionEpoch, err = playerEpoch(ctx, q, row.ID); err != nil {
				return err
			}
			if err := insertSession(ctx, tx, projectID, row.ID, refreshToken, now); err != nil {
				return err
			}
			return auditlog.Write(ctx, tx, row.ID, "auth.login", in.Body.Email, nil)
		})
		if errors.Is(err, pgx.ErrNoRows) {
			_ = bcrypt.CompareHashAndPassword(dummyBcryptHash, []byte(in.Body.Password))
			d.Metrics.Login(observability.SurfaceAPI, observability.LoginInvalid)
			return nil, huma.Error401Unauthorized("invalid credentials")
		}
		if errors.Is(err, errBadCredentials) {
			d.Metrics.Login(observability.SurfaceAPI, observability.LoginInvalid)
			return nil, huma.Error401Unauthorized("invalid credentials")
		}
		if errors.Is(err, errEmailUnverified) {
			d.Metrics.Login(observability.SurfaceAPI, observability.LoginUnverified)
			return nil, huma.Error403Forbidden("email not verified")
		}
		if errors.Is(err, errPlayerBanned) {
			d.Metrics.Login(observability.SurfaceAPI, observability.LoginLocked)
			return nil, huma.Error403Forbidden("account banned")
		}
		if err != nil {
			return nil, serverError(ctx, "login: tx", err)
		}
		d.Metrics.Login(observability.SurfaceAPI, observability.LoginOK)

		return mintSession(ctx, d, "login", auth.Claims{
			PlayerID: playerID, TenantID: tenantID, ProjectID: projectID,
			SessionEpoch: sessionEpoch,
			ExpiresAt:    now.Add(accessTokenTTL),
		}, refreshToken)
	}
}

// authRefresh rotates the refresh token and issues a new access token.
func authRefresh(d Deps) func(context.Context, *refreshInput) (*sessionOutput, error) {
	return func(ctx context.Context, in *refreshInput) (*sessionOutput, error) {
		projectID, tenantID, err := pinnedProject(ctx)
		if err != nil {
			return nil, err
		}
		oldHash := sha256.Sum256([]byte(in.Body.RefreshToken))
		newRefresh, err := webutil.RandomHex("", 32)
		if err != nil {
			return nil, serverError(ctx, "refresh: rand", err)
		}
		now := apiNow(d)

		var (
			playerID, sessionEpoch int64
			reuseDetected          bool
		)
		err = d.Pool.Q(ctx, func(tx pgx.Tx) error {
			q := sqlcgen.New(tx)
			row, err := q.GetSessionByRefreshHash(ctx, sqlcgen.GetSessionByRefreshHashParams{
				ProjectID:   projectID,
				RefreshHash: oldHash[:],
			})
			if err != nil {
				return err
			}
			if row.RevokedAt.Valid {
				// Replaying a token revoked by *rotation* means it was
				// captured — the legitimate client already rotated past it.
				// Treat it as theft: revoke every live session for the player
				// and bump the epoch so outstanding access tokens die at the
				// gate immediately. (A client that lost the rotation response
				// and retries the old token trips this too and must re-auth —
				// the accepted cost of reuse detection.) A token revoked by
				// logout is a benign stale retry and falls through to the plain
				// 401 below. The mitigation must COMMIT, so we set a flag and
				// return nil (an error would roll it back) and surface the
				// opaque 401 after.
				if row.RevokedReason != nil && *row.RevokedReason == sessionRevokedRotated {
					if _, err := q.RevokeActivePlayerSessions(ctx, sqlcgen.RevokeActivePlayerSessionsParams{
						ProjectID: row.ProjectID,
						PlayerID:  row.PlayerID,
					}); err != nil {
						return err
					}
					if err := q.BumpPlayerSessionEpoch(ctx, row.PlayerID); err != nil {
						return err
					}
					reuseDetected = true
					return auditlog.Write(ctx, tx, row.PlayerID, "auth.refresh_reuse", "", nil)
				}
				return errSessionRevoked
			}
			if row.ExpiresAt.Time.Before(now) {
				return errSessionRevoked
			}
			if err := playerBanCheck(ctx, q, row.PlayerID); err != nil {
				return err
			}
			playerID = row.PlayerID
			if sessionEpoch, err = playerEpoch(ctx, q, row.PlayerID); err != nil {
				return err
			}
			if err := q.RevokeSession(ctx, row.ID); err != nil {
				return err
			}
			if err := insertSession(ctx, tx, row.ProjectID, row.PlayerID, newRefresh, now); err != nil {
				return err
			}
			return auditlog.Write(ctx, tx, row.PlayerID, "auth.refresh", "", nil)
		})
		if errors.Is(err, pgx.ErrNoRows) || errors.Is(err, errSessionRevoked) {
			return nil, huma.Error401Unauthorized("invalid refresh")
		}
		if errors.Is(err, errPlayerBanned) {
			return nil, huma.Error403Forbidden("account banned")
		}
		if err != nil {
			return nil, serverError(ctx, "refresh: tx", err)
		}
		if reuseDetected {
			return nil, huma.Error401Unauthorized("invalid refresh")
		}

		return mintSession(ctx, d, "refresh", auth.Claims{
			PlayerID: playerID, TenantID: tenantID, ProjectID: projectID,
			SessionEpoch: sessionEpoch,
			ExpiresAt:    now.Add(accessTokenTTL),
		}, newRefresh)
	}
}

func authLogout(d Deps) func(context.Context, *logoutInput) (*struct{}, error) {
	return func(ctx context.Context, in *logoutInput) (*struct{}, error) {
		hash := sha256.Sum256([]byte(in.Body.RefreshToken))
		err := d.Pool.Q(ctx, func(tx pgx.Tx) error {
			q := sqlcgen.New(tx)
			playerID, err := q.RevokeSessionByRefreshHash(ctx, hash[:])
			if err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					// Unknown or already-revoked refresh token. Treat the
					// logout as a no-op rather than 500 — the client's
					// goal (no live session for this token) is satisfied.
					return nil
				}
				return err
			}
			return auditlog.Write(ctx, tx, playerID, "auth.logout", "", nil)
		})
		if err != nil {
			return nil, serverError(ctx, "logout: tx", err)
		}
		return nil, nil
	}
}

// authCustomToken accepts a tenant-signed JWT carrying an external_id; ggscale
// verifies and mints a session for that user.
func authCustomToken(d Deps) func(context.Context, *customTokenInput) (*sessionOutput, error) {
	return func(ctx context.Context, in *customTokenInput) (*sessionOutput, error) {
		projectID, tenantID, err := pinnedProject(ctx)
		if err != nil {
			return nil, err
		}
		refreshTok, err := webutil.RandomHex("", 32)
		if err != nil {
			return nil, serverError(ctx, "custom-token: rand", err)
		}
		now := apiNow(d)

		var playerID, sessionEpoch int64
		err = d.Pool.Q(ctx, func(tx pgx.Tx) error {
			q := sqlcgen.New(tx)
			secret, err := q.GetTenantCustomTokenSecret(ctx)
			if err != nil {
				return err
			}
			if len(secret) == 0 {
				return errCustomTokenNotConfigured
			}
			parsed, err := parseCustomToken(in.Body.Token, secret, now)
			if err != nil {
				return err
			}
			id, err := q.UpsertPlayerByExternalID(ctx, sqlcgen.UpsertPlayerByExternalIDParams{
				ProjectID:  projectID,
				ExternalID: parsed.External,
			})
			if err != nil {
				return fmt.Errorf("upsert player: %w", err)
			}
			playerID = id
			if err := playerBanCheck(ctx, q, id); err != nil {
				return err
			}
			if sessionEpoch, err = playerEpoch(ctx, q, id); err != nil {
				return err
			}
			if err := insertSession(ctx, tx, projectID, id, refreshTok, now); err != nil {
				return err
			}
			return auditlog.Write(ctx, tx, id, "auth.custom_token", parsed.External, nil)
		})
		switch {
		case errors.Is(err, errCustomTokenNotConfigured):
			return nil, huma.Error400BadRequest("custom-token not configured for this tenant")
		case errors.Is(err, errCustomTokenInvalid):
			return nil, huma.Error401Unauthorized("invalid custom token")
		case errors.Is(err, errPlayerBanned):
			return nil, huma.Error403Forbidden("account banned")
		case err != nil:
			return nil, serverError(ctx, "custom-token: tx", err)
		}

		return mintSession(ctx, d, "custom-token", auth.Claims{
			PlayerID: playerID, TenantID: tenantID, ProjectID: projectID,
			SessionEpoch: sessionEpoch,
			ExpiresAt:    now.Add(accessTokenTTL),
		}, refreshTok)
	}
}

// parseCustomToken verifies a tenant-signed custom token against secret and
// returns its claims. Every validation failure collapses to
// errCustomTokenInvalid so the wire can't distinguish a bad signature from an
// expired or wrong-audience token. The parser pins HS256 (blocking alg=none
// and RS→HS confusion), requires exp, checks aud, and allows a small clock
// skew; the explicit future-iat guard rejects tokens minted further ahead
// than that leeway (a clock fault or a forged token).
func parseCustomToken(tokenStr string, secret []byte, now time.Time) (*customTokenClaims, error) {
	parsed := &customTokenClaims{}
	parser := jwt.NewParser(
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithExpirationRequired(),
		jwt.WithAudience(customTokenAudience),
		jwt.WithLeeway(30*time.Second),
	)
	if _, err := parser.ParseWithClaims(tokenStr, parsed, func(*jwt.Token) (any, error) {
		return secret, nil
	}); err != nil {
		return nil, errCustomTokenInvalid
	}
	if parsed.External == "" {
		return nil, errCustomTokenInvalid
	}
	if parsed.IssuedAt != nil && parsed.IssuedAt.After(now.Add(5*time.Minute)) {
		return nil, errCustomTokenInvalid
	}
	return parsed, nil
}

type customTokenClaims struct {
	jwt.RegisteredClaims
	External string `json:"external_id"`
}

// customTokenAudience is the required aud claim on tenant-signed custom
// tokens. Pinning aud prevents a token issued for another system that
// happens to share the secret from being replayed against ggscale.
const customTokenAudience = "ggscale-custom-token" //nolint:gosec // aud claim value, not a credential

var (
	errBadCredentials           = errors.New("auth: bad credentials")
	errSessionRevoked           = errors.New("auth: session revoked or expired")
	errPlayerBanned             = errors.New("auth: player banned in tenant")
	errEmailUnverified          = errors.New("auth: email not verified")
	errCustomTokenNotConfigured = errors.New("auth: custom token secret not set")
	errCustomTokenInvalid       = errors.New("auth: custom token invalid")
	errVerifyBadCode            = errors.New("auth: bad verification code")
	errVerifyExpired            = errors.New("auth: verification code expired")
	errVerifyExhausted          = errors.New("auth: verification attempts exhausted")
	errVerifyAccountLocked      = errors.New("auth: account locked after too many verification attempts")
)

// sessionRevokedRotated is the sessions.revoked_reason value auth.sql writes
// when a refresh token is superseded by rotation (RevokeSession). authRefresh
// reads it to tell a replayed rotated token (reuse/theft) from a logged-out
// one; the 'logout' and 'reuse_detected' values are only ever written in SQL,
// never read here.
const sessionRevokedRotated = "rotated"

// dummyBcryptHash is a valid bcryptCost=12 hash used to flatten login timing
// when the email lookup misses. Without this, /v1/auth/login returns ~5ms on
// unknown emails versus ~250ms on known emails, leaking enumeration.
var dummyBcryptHash = mustGenerateDummyHash()

func mustGenerateDummyHash() []byte {
	h, err := bcrypt.GenerateFromPassword([]byte("dummy-password-for-timing-equalisation"), bcryptCost)
	if err != nil {
		panic(err)
	}
	return h
}

// noticeThrottleAllows reports whether a duplicate-signup notice may be sent to
// email now, recording the send in the shared cache when it returns true. The
// window is per (project, email); the email is hashed so no address sits in a
// cache key. With no cache wired (d.Cache == nil) it always allows, so
// zero-config self-host still notifies — it just forgoes the throttle. Any
// cache error fails open (send) rather than dropping a legitimate notice; a
// race between two concurrent duplicates sends at most one extra, which is
// harmless.
func noticeThrottleAllows(ctx context.Context, d Deps, projectID int64, email string) bool {
	if d.Cache == nil {
		return true
	}
	sum := sha256.Sum256([]byte(email))
	key := fmt.Sprintf("signup-notice:%d:%x", projectID, sum[:])
	if v, err := d.Cache.Get(ctx, key); err == nil && v != nil {
		return false
	}
	_ = d.Cache.Set(ctx, key, []byte{1}, signupNoticeCooldown)
	return true
}

// pinnedProject extracts the project and tenant IDs the API-key middleware
// pinned onto ctx. Every /v1/auth handler needs the project pin; returning
// the shared 400 keeps the "api key has no project pin" contract identical
// across sites. The tenant is guaranteed present by the tenant middleware
// that fronts these routes, so its lookup error is intentionally discarded.
func pinnedProject(ctx context.Context) (projectID, tenantID int64, err error) {
	projectID, ok := db.ProjectFromContext(ctx)
	if !ok {
		return 0, 0, huma.Error400BadRequest("api key has no project pin")
	}
	tenantID, _ = db.TenantFromContext(ctx)
	return projectID, tenantID, nil
}

// playerBanCheck returns errPlayerBanned when the player is banned in the
// current tenant, nil when not, or the underlying error. IsPlayerBannedByTenant
// returns a row (nil error) IFF a ban exists, so the nil-error branch is the
// "banned" case — inverted from the usual Go idiom, which is exactly why it
// lives behind one audited helper rather than being retyped at each call site.
func playerBanCheck(ctx context.Context, q *sqlcgen.Queries, playerID int64) error {
	if _, err := q.IsPlayerBannedByTenant(ctx, playerID); err == nil {
		return errPlayerBanned
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	return nil
}

// playerEpoch reads the player's session epoch, widening the generated int32
// column to the int64 the JWT claim carries.
func playerEpoch(ctx context.Context, q *sqlcgen.Queries, playerID int64) (int64, error) {
	ep, err := q.GetPlayerSessionEpoch(ctx, playerID)
	return int64(ep), err
}

// mintSession signs an access token for claims and packages it with
// refreshToken into the shared session response. op labels the 500 log line
// on a signing failure. Reading PlayerID and ExpiresAt back off claims keeps
// the response body and the signed token from ever disagreeing.
func mintSession(ctx context.Context, d Deps, op string, claims auth.Claims, refreshToken string) (*sessionOutput, error) {
	accessToken, err := d.Signer.Sign(claims)
	if err != nil {
		return nil, serverError(ctx, op+": sign", err)
	}
	return &sessionOutput{Body: sessionResponse{
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		PlayerID:     claims.PlayerID,
		ExpiresAt:    claims.ExpiresAt.UTC().Format(time.RFC3339),
	}}, nil
}

func insertSession(ctx context.Context, tx pgx.Tx, projectID, playerID int64, refreshToken string, now time.Time) error {
	q := sqlcgen.New(tx)
	sum := sha256.Sum256([]byte(refreshToken))
	_, err := q.CreateSession(ctx, sqlcgen.CreateSessionParams{
		ProjectID:   projectID,
		PlayerID:    playerID,
		RefreshHash: sum[:],
		ExpiresAt:   pgtype.Timestamptz{Time: now.Add(refreshTokenTTL), Valid: true},
	})
	if err != nil {
		return fmt.Errorf("insert session: %w", err)
	}
	return nil
}

// writeJSON is the last raw JSON writer, used only by the verify body-callback
// (which owns its response entirely to keep the opaque-401 shape).
func writeJSON(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	//nolint:gosec // gosec G117 false-positive: handler contract is to return tokens.
	_ = json.NewEncoder(w).Encode(body)
}

func validateEmail(s string) bool {
	_, err := webutil.ValidateEmail(s)
	return err == nil
}

func validPassword(s string) bool {
	return len(s) >= 8 && len(s) <= maxPasswordBytes
}
