package httpapi

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

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
	maxJSONBodyBytes     = 1 << 20
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

type signupRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type verifyRequest struct {
	Email string `json:"email"`
	Code  string `json:"code"`
}

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type refreshRequest struct {
	RefreshToken string `json:"refresh_token"`
}

type logoutRequest struct {
	RefreshToken string `json:"refresh_token"`
}

type customTokenRequest struct {
	Token string `json:"token"`
}

// anonymousHandler
func anonymousHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		projectID, ok := db.ProjectFromContext(ctx)
		if !ok {
			http.Error(w, "api key has no project pin", http.StatusBadRequest)
			return
		}
		tenantID, _ := db.TenantFromContext(ctx)

		externalID, err := webutil.RandomHex("anon_", 16)
		if err != nil {
			webutil.InternalError(w, "anonymous: external_id rand", err)
			return
		}
		refreshToken, err := webutil.RandomHex("", 32)
		if err != nil {
			webutil.InternalError(w, "anonymous: refresh rand", err)
			return
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
			webutil.InternalError(w, "anonymous: tx", err)
			return
		}

		accessToken, err := d.Signer.Sign(auth.Claims{
			PlayerID: playerID, TenantID: tenantID, ProjectID: projectID,
			ExpiresAt: accessExpiresAt,
		})
		if err != nil {
			webutil.InternalError(w, "anonymous: sign", err)
			return
		}

		writeJSON(w, anonymousResponse{
			AccessToken: accessToken, RefreshToken: refreshToken,
			PlayerID: playerID, ExternalID: externalID,
			ExpiresAt: accessExpiresAt.UTC().Format(time.RFC3339),
		})
	}
}

// signupHandler
func signupHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req signupRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		if !validateEmail(req.Email) || !validPassword(req.Password) {
			http.Error(w, "email or password invalid", http.StatusBadRequest)
			return
		}

		ctx := r.Context()
		projectID, ok := db.ProjectFromContext(ctx)
		if !ok {
			http.Error(w, "api key has no project pin", http.StatusBadRequest)
			return
		}

		hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcryptCost)
		if err != nil {
			webutil.InternalError(w, "signup: bcrypt", err)
			return
		}
		code, err := verifycode.GenerateCode()
		if err != nil {
			webutil.InternalError(w, "signup: code", err)
			return
		}
		salt, err := verifycode.NewSalt()
		if err != nil {
			webutil.InternalError(w, "signup: salt", err)
			return
		}
		codeHash := verifycode.Hash(salt, code)
		externalID, err := webutil.RandomHex("user_", 16)
		if err != nil {
			webutil.InternalError(w, "signup: ext_id rand", err)
			return
		}
		now := apiNow(d)

		err = d.Pool.Q(ctx, func(tx pgx.Tx) error {
			q := sqlcgen.New(tx)
			email := req.Email
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
				// only the legitimate owner can read.
				if d.Mailer != nil {
					existing := mailer.Message{
						From: d.MailFrom, To: []string{req.Email},
						Subject: "Your ggscale account",
						Body:    "Someone tried to sign up using this email. If that was you, sign in directly — your account already exists.",
					}
					if err := d.Mailer.Send(ctx, existing); err != nil {
						slog.Error("signup: existing-account mailer", "error", err)
					}
				}
				w.WriteHeader(http.StatusAccepted)
				return
			}
			webutil.InternalError(w, "signup: tx", err)
			return
		}
		d.Metrics.Signup(observability.SignupPlayer)

		if d.Mailer != nil {
			msg := mailer.Message{
				From: d.MailFrom, To: []string{req.Email},
				Subject: mailerVerifySubject,
				Body:    fmt.Sprintf(mailerVerifyBodyTmpl, code),
			}
			if err := d.Mailer.Send(ctx, msg); err != nil {
				slog.Error("signup: mailer", "error", err)
			}
		}

		w.WriteHeader(http.StatusAccepted)
	}
}

// verifyHandler accepts {email, code}; matches by salt+hash
// after looking up the row; enforces a 5-attempt cap before clearing.
func verifyHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req verifyRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		if req.Email == "" || req.Code == "" {
			http.Error(w, "email and code required", http.StatusBadRequest)
			return
		}

		ctx := r.Context()
		projectID, ok := db.ProjectFromContext(ctx)
		if !ok {
			http.Error(w, "api key has no project pin", http.StatusBadRequest)
			return
		}
		now := apiNow(d)

		var (
			playerID           int64
			lockedAfterAttempt bool
		)
		err := d.Pool.Q(ctx, func(tx pgx.Tx) error {
			q := sqlcgen.New(tx)
			email := req.Email
			row, err := q.GetPlayerVerificationState(ctx, sqlcgen.GetPlayerVerificationStateParams{
				ProjectID: projectID,
				Email:     &email,
			})
			if err != nil {
				return err
			}
			if row.EmailVerifiedAt.Valid {
				playerID = row.ID
				return nil
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
			expected := verifycode.Hash(row.EmailVerificationSalt, req.Code)
			if subtle.ConstantTimeCompare(expected, row.EmailVerificationCodeHash) == 1 {
				if err := q.MarkPlayerVerified(ctx, row.ID); err != nil {
					return err
				}
				playerID = row.ID
				return auditlog.Write(ctx, tx, row.ID, "auth.verify", "", nil)
			}
			return errVerifyBadCode
		})
		switch {
		case errors.Is(err, pgx.ErrNoRows), errors.Is(err, errVerifyBadCode), errors.Is(err, errVerifyExpired):
			d.Metrics.Verification(observability.VerifyInvalid)
			http.Error(w, "invalid email or code", http.StatusBadRequest)
			return
		case errors.Is(err, errVerifyExhausted):
			d.Metrics.Verification(observability.VerifyThrottled)
			http.Error(w, "too many attempts", http.StatusTooManyRequests)
			return
		case errors.Is(err, errVerifyAccountLocked):
			d.Metrics.Verification(observability.VerifyThrottled)
			http.Error(w, "account locked, contact support", http.StatusTooManyRequests)
			return
		case err != nil:
			webutil.InternalError(w, "verify: tx", err)
			return
		}
		if lockedAfterAttempt {
			d.Metrics.Verification(observability.VerifyThrottled)
			http.Error(w, "account locked, contact support", http.StatusTooManyRequests)
			return
		}

		d.Metrics.Verification(observability.VerifyOK)
		writeJSON(w, map[string]any{"player_id": playerID, "verified": true})
	}
}

// loginHandler
func loginHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req loginRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		if !validPassword(req.Password) {
			http.Error(w, "invalid credentials", http.StatusUnauthorized)
			return
		}

		ctx := r.Context()
		projectID, ok := db.ProjectFromContext(ctx)
		if !ok {
			http.Error(w, "api key has no project pin", http.StatusBadRequest)
			return
		}
		tenantID, _ := db.TenantFromContext(ctx)

		refreshToken, err := webutil.RandomHex("", 32)
		if err != nil {
			webutil.InternalError(w, "login: refresh rand", err)
			return
		}
		now := apiNow(d)
		var playerID, sessionEpoch int64
		err = d.Pool.Q(ctx, func(tx pgx.Tx) error {
			q := sqlcgen.New(tx)
			email := req.Email
			row, err := q.GetPlayerByEmail(ctx, sqlcgen.GetPlayerByEmailParams{
				ProjectID: projectID,
				Email:     &email,
			})
			if err != nil {
				return err
			}
			if bcrypt.CompareHashAndPassword(row.PasswordHash, []byte(req.Password)) != nil {
				return errBadCredentials
			}
			// Tenant-ban enforcement: a banned account cannot log in.
			if _, berr := q.IsPlayerBannedByTenant(ctx, row.ID); berr == nil {
				return errPlayerBanned
			} else if !errors.Is(berr, pgx.ErrNoRows) {
				return berr
			}
			playerID = row.ID
			ep, eerr := q.GetPlayerSessionEpoch(ctx, row.ID)
			if eerr != nil {
				return eerr
			}
			sessionEpoch = int64(ep)
			if err := insertSession(ctx, tx, projectID, row.ID, refreshToken, now); err != nil {
				return err
			}
			return auditlog.Write(ctx, tx, row.ID, "auth.login", req.Email, nil)
		})
		if errors.Is(err, pgx.ErrNoRows) {
			_ = bcrypt.CompareHashAndPassword(dummyBcryptHash, []byte(req.Password))
			d.Metrics.Login(observability.SurfaceAPI, observability.LoginInvalid)
			http.Error(w, "invalid credentials", http.StatusUnauthorized)
			return
		}
		if errors.Is(err, errBadCredentials) {
			d.Metrics.Login(observability.SurfaceAPI, observability.LoginInvalid)
			http.Error(w, "invalid credentials", http.StatusUnauthorized)
			return
		}
		if errors.Is(err, errPlayerBanned) {
			d.Metrics.Login(observability.SurfaceAPI, observability.LoginLocked)
			http.Error(w, "account banned", http.StatusForbidden)
			return
		}
		if err != nil {
			webutil.InternalError(w, "login: tx", err)
			return
		}
		d.Metrics.Login(observability.SurfaceAPI, observability.LoginOK)

		expiresAt := now.Add(accessTokenTTL)
		accessToken, err := d.Signer.Sign(auth.Claims{
			PlayerID: playerID, TenantID: tenantID, ProjectID: projectID,
			SessionEpoch: sessionEpoch,
			ExpiresAt:    expiresAt,
		})
		if err != nil {
			webutil.InternalError(w, "login: sign", err)
			return
		}
		writeJSON(w, sessionResponse{
			AccessToken: accessToken, RefreshToken: refreshToken,
			PlayerID: playerID, ExpiresAt: expiresAt.UTC().Format(time.RFC3339),
		})
	}
}

// refreshHandler rotates the refresh token and issues a new access token.
func refreshHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req refreshRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		if req.RefreshToken == "" {
			http.Error(w, "refresh_token required", http.StatusBadRequest)
			return
		}

		ctx := r.Context()
		projectID, ok := db.ProjectFromContext(ctx)
		if !ok {
			http.Error(w, "api key has no project pin", http.StatusBadRequest)
			return
		}
		tenantID, _ := db.TenantFromContext(ctx)
		oldHash := sha256.Sum256([]byte(req.RefreshToken))
		newRefresh, err := webutil.RandomHex("", 32)
		if err != nil {
			webutil.InternalError(w, "refresh: rand", err)
			return
		}
		now := apiNow(d)

		var playerID, sessionEpoch int64
		err = d.Pool.Q(ctx, func(tx pgx.Tx) error {
			q := sqlcgen.New(tx)
			row, err := q.GetSessionByRefreshHash(ctx, sqlcgen.GetSessionByRefreshHashParams{
				ProjectID:   projectID,
				RefreshHash: oldHash[:],
			})
			if err != nil {
				return err
			}
			if row.RevokedAt.Valid || row.ExpiresAt.Time.Before(now) {
				return errSessionRevoked
			}
			// A tenant-banned account cannot refresh into a new access token.
			if _, berr := q.IsPlayerBannedByTenant(ctx, row.PlayerID); berr == nil {
				return errPlayerBanned
			} else if !errors.Is(berr, pgx.ErrNoRows) {
				return berr
			}
			playerID = row.PlayerID
			ep, eerr := q.GetPlayerSessionEpoch(ctx, row.PlayerID)
			if eerr != nil {
				return eerr
			}
			sessionEpoch = int64(ep)
			if err := q.RevokeSession(ctx, row.ID); err != nil {
				return err
			}
			if err := insertSession(ctx, tx, row.ProjectID, row.PlayerID, newRefresh, now); err != nil {
				return err
			}
			return auditlog.Write(ctx, tx, row.PlayerID, "auth.refresh", "", nil)
		})
		if errors.Is(err, pgx.ErrNoRows) || errors.Is(err, errSessionRevoked) {
			http.Error(w, "invalid refresh", http.StatusUnauthorized)
			return
		}
		if errors.Is(err, errPlayerBanned) {
			http.Error(w, "account banned", http.StatusForbidden)
			return
		}
		if err != nil {
			webutil.InternalError(w, "refresh: tx", err)
			return
		}

		expiresAt := now.Add(accessTokenTTL)
		accessToken, err := d.Signer.Sign(auth.Claims{
			PlayerID: playerID, TenantID: tenantID, ProjectID: projectID,
			SessionEpoch: sessionEpoch,
			ExpiresAt:    expiresAt,
		})
		if err != nil {
			webutil.InternalError(w, "refresh: sign", err)
			return
		}
		writeJSON(w, sessionResponse{
			AccessToken: accessToken, RefreshToken: newRefresh,
			PlayerID: playerID, ExpiresAt: expiresAt.UTC().Format(time.RFC3339),
		})
	}
}

// logoutHandler
func logoutHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req logoutRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		if req.RefreshToken == "" {
			http.Error(w, "refresh_token required", http.StatusBadRequest)
			return
		}

		ctx := r.Context()
		hash := sha256.Sum256([]byte(req.RefreshToken))
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
			webutil.InternalError(w, "logout: tx", err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// customTokenHandler accepts a tenant-signed JWT carrying an
// external_id; ggscale verifies and mints a session for that user.
func customTokenHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req customTokenRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		if req.Token == "" {
			http.Error(w, "token required", http.StatusBadRequest)
			return
		}

		ctx := r.Context()
		projectID, ok := db.ProjectFromContext(ctx)
		if !ok {
			http.Error(w, "api key has no project pin", http.StatusBadRequest)
			return
		}
		tenantID, _ := db.TenantFromContext(ctx)

		var (
			secret       []byte
			playerID     int64
			sessionEpoch int64
			externalID   string
			refreshTok   string
		)
		var err error
		refreshTok, err = webutil.RandomHex("", 32)
		if err != nil {
			webutil.InternalError(w, "custom-token: rand", err)
			return
		}
		now := apiNow(d)

		err = d.Pool.Q(ctx, func(tx pgx.Tx) error {
			q := sqlcgen.New(tx)
			s, err := q.GetTenantCustomTokenSecret(ctx)
			if err != nil {
				return err
			}
			if len(s) == 0 {
				return errCustomTokenNotConfigured
			}
			secret = s

			parsed := &customTokenClaims{}
			parser := jwt.NewParser(
				jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
				jwt.WithExpirationRequired(),
				jwt.WithAudience(customTokenAudience),
				jwt.WithLeeway(30*time.Second),
			)
			if _, err := parser.ParseWithClaims(req.Token, parsed, func(_ *jwt.Token) (any, error) {
				return secret, nil
			}); err != nil {
				return errCustomTokenInvalid
			}
			if parsed.External == "" {
				return errCustomTokenInvalid
			}
			// Reject tokens minted with a future iat — a small skew is
			// permitted by jwt.WithLeeway above; anything beyond that is
			// either a clock fault or a forged token.
			if parsed.IssuedAt != nil && parsed.IssuedAt.After(now.Add(5*time.Minute)) {
				return errCustomTokenInvalid
			}
			externalID = parsed.External

			id, err := q.UpsertPlayerByExternalID(ctx, sqlcgen.UpsertPlayerByExternalIDParams{
				ProjectID:  projectID,
				ExternalID: externalID,
			})
			if err != nil {
				return fmt.Errorf("upsert player: %w", err)
			}
			playerID = id
			if _, berr := q.IsPlayerBannedByTenant(ctx, id); berr == nil {
				return errPlayerBanned
			} else if !errors.Is(berr, pgx.ErrNoRows) {
				return berr
			}
			ep, eerr := q.GetPlayerSessionEpoch(ctx, id)
			if eerr != nil {
				return eerr
			}
			sessionEpoch = int64(ep)
			if err := insertSession(ctx, tx, projectID, id, refreshTok, now); err != nil {
				return err
			}
			return auditlog.Write(ctx, tx, id, "auth.custom_token", externalID, nil)
		})
		switch {
		case errors.Is(err, errCustomTokenNotConfigured):
			http.Error(w, "custom-token not configured for this tenant", http.StatusBadRequest)
			return
		case errors.Is(err, errCustomTokenInvalid):
			http.Error(w, "invalid custom token", http.StatusUnauthorized)
			return
		case errors.Is(err, errPlayerBanned):
			http.Error(w, "account banned", http.StatusForbidden)
			return
		case err != nil:
			webutil.InternalError(w, "custom-token: tx", err)
			return
		}

		expiresAt := now.Add(accessTokenTTL)
		accessToken, err := d.Signer.Sign(auth.Claims{
			PlayerID: playerID, TenantID: tenantID, ProjectID: projectID,
			SessionEpoch: sessionEpoch,
			ExpiresAt:    expiresAt,
		})
		if err != nil {
			webutil.InternalError(w, "custom-token: sign", err)
			return
		}
		writeJSON(w, sessionResponse{
			AccessToken: accessToken, RefreshToken: refreshTok,
			PlayerID: playerID, ExpiresAt: expiresAt.UTC().Format(time.RFC3339),
		})
	}
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
	errCustomTokenNotConfigured = errors.New("auth: custom token secret not set")
	errCustomTokenInvalid       = errors.New("auth: custom token invalid")
	errVerifyBadCode            = errors.New("auth: bad verification code")
	errVerifyExpired            = errors.New("auth: verification code expired")
	errVerifyExhausted          = errors.New("auth: verification attempts exhausted")
	errVerifyAccountLocked      = errors.New("auth: account locked after too many verification attempts")
)

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

func decodeJSON(w http.ResponseWriter, r *http.Request, into any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return false
		}
		http.Error(w, "bad request body", http.StatusBadRequest)
		return false
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		http.Error(w, "bad request body", http.StatusBadRequest)
		return false
	}
	return true
}

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
