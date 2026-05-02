package httpapi

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
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
)

const (
	accessTokenTTL       = 15 * time.Minute
	refreshTokenTTL      = 30 * 24 * time.Hour
	verifyTokenTTL       = 24 * time.Hour
	bcryptCost           = 12
	maxJSONBodyBytes     = 1 << 20
	mailerVerifySubject  = "Verify your ggscale email"
	mailerVerifyBodyTmpl = "Your verification token (valid 24h):\n\n%s\n\nSubmit it to POST /v1/auth/verify."
)

type anonymousResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	EndUserID    int64  `json:"end_user_id"`
	ExternalID   string `json:"external_id"`
	ExpiresAt    string `json:"expires_at"`
}

type sessionResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	EndUserID    int64  `json:"end_user_id"`
	ExpiresAt    string `json:"expires_at"`
}

type signupRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type verifyRequest struct {
	Token string `json:"token"`
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

// anonymousHandler — m1.md 4.1.6
func anonymousHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		projectID, ok := db.ProjectFromContext(ctx)
		if !ok {
			http.Error(w, "api key has no project pin", http.StatusBadRequest)
			return
		}
		tenantID, _ := db.TenantFromContext(ctx)

		externalID, err := randomHex("anon_", 16)
		if err != nil {
			internalError(w, "anonymous: external_id rand", err)
			return
		}
		refreshToken, err := randomHex("", 32)
		if err != nil {
			internalError(w, "anonymous: refresh rand", err)
			return
		}

		accessExpiresAt := time.Now().Add(accessTokenTTL)
		var endUserID int64
		err = d.Pool.Q(ctx, func(tx pgx.Tx) error {
			q := sqlcgen.New(tx)
			user, err := q.CreateAnonymousEndUser(ctx, sqlcgen.CreateAnonymousEndUserParams{
				ProjectID:  projectID,
				ExternalID: externalID,
			})
			if err != nil {
				return fmt.Errorf("insert end_user: %w", err)
			}
			endUserID = user.ID
			if err := insertSession(ctx, tx, user.ID, refreshToken); err != nil {
				return err
			}
			return auditlog.Write(ctx, tx, user.ID, "auth.anonymous", "", map[string]any{"external_id": externalID})
		})
		if err != nil {
			internalError(w, "anonymous: tx", err)
			return
		}

		accessToken, err := d.Signer.Sign(auth.Claims{
			EndUserID: endUserID, TenantID: tenantID, ProjectID: projectID,
			ExpiresAt: accessExpiresAt,
		})
		if err != nil {
			internalError(w, "anonymous: sign", err)
			return
		}

		writeJSON(w, anonymousResponse{
			AccessToken: accessToken, RefreshToken: refreshToken,
			EndUserID: endUserID, ExternalID: externalID,
			ExpiresAt: accessExpiresAt.UTC().Format(time.RFC3339),
		})
	}
}

// signupHandler — m1.md 4.1.1
func signupHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req signupRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		if !validateEmail(req.Email) || len(req.Password) < 8 {
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
			internalError(w, "signup: bcrypt", err)
			return
		}
		verifyToken, err := randomHex("", 32)
		if err != nil {
			internalError(w, "signup: verify rand", err)
			return
		}
		verifyHash := sha256.Sum256([]byte(verifyToken))
		externalID, err := randomHex("user_", 16)
		if err != nil {
			internalError(w, "signup: ext_id rand", err)
			return
		}

		err = d.Pool.Q(ctx, func(tx pgx.Tx) error {
			q := sqlcgen.New(tx)
			email := req.Email
			expires := pgtype.Timestamptz{Time: time.Now().Add(verifyTokenTTL), Valid: true}
			id, err := q.CreateEmailEndUser(ctx, sqlcgen.CreateEmailEndUserParams{
				ProjectID:                  projectID,
				ExternalID:                 externalID,
				Email:                      &email,
				PasswordHash:               hash,
				EmailVerificationHash:      verifyHash[:],
				EmailVerificationExpiresAt: expires,
			})
			if err != nil {
				return fmt.Errorf("insert end_user: %w", err)
			}
			return auditlog.Write(ctx, tx, id, "auth.signup", email, nil)
		})
		if err != nil {
			if isUniqueViolation(err) {
				http.Error(w, "email already in use", http.StatusConflict)
				return
			}
			internalError(w, "signup: tx", err)
			return
		}

		if d.Mailer != nil {
			msg := mailer.Message{
				From: d.MailFrom, To: []string{req.Email},
				Subject: mailerVerifySubject,
				Body:    fmt.Sprintf(mailerVerifyBodyTmpl, verifyToken),
			}
			if err := d.Mailer.Send(ctx, msg); err != nil {
				slog.Error("signup: mailer", "error", err)
			}
		}

		w.WriteHeader(http.StatusAccepted)
	}
}

// verifyHandler — m1.md 4.1.2
func verifyHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req verifyRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		if req.Token == "" {
			http.Error(w, "token required", http.StatusBadRequest)
			return
		}
		hash := sha256.Sum256([]byte(req.Token))

		ctx := r.Context()
		var endUserID int64
		err := d.Pool.Q(ctx, func(tx pgx.Tx) error {
			q := sqlcgen.New(tx)
			id, err := q.VerifyEmailByTokenHash(ctx, hash[:])
			if err != nil {
				return err
			}
			endUserID = id
			return auditlog.Write(ctx, tx, id, "auth.verify", "", nil)
		})
		if errors.Is(err, pgx.ErrNoRows) {
			http.Error(w, "invalid or expired token", http.StatusBadRequest)
			return
		}
		if err != nil {
			internalError(w, "verify: tx", err)
			return
		}

		writeJSON(w, map[string]any{"end_user_id": endUserID, "verified": true})
	}
}

// loginHandler — m1.md 4.1.3
func loginHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req loginRequest
		if !decodeJSON(w, r, &req) {
			return
		}

		ctx := r.Context()
		projectID, ok := db.ProjectFromContext(ctx)
		if !ok {
			http.Error(w, "api key has no project pin", http.StatusBadRequest)
			return
		}
		tenantID, _ := db.TenantFromContext(ctx)

		refreshToken, err := randomHex("", 32)
		if err != nil {
			internalError(w, "login: refresh rand", err)
			return
		}
		var endUserID int64
		err = d.Pool.Q(ctx, func(tx pgx.Tx) error {
			q := sqlcgen.New(tx)
			email := req.Email
			row, err := q.GetEndUserByEmail(ctx, sqlcgen.GetEndUserByEmailParams{
				ProjectID: projectID,
				Email:     &email,
			})
			if err != nil {
				return err
			}
			if bcrypt.CompareHashAndPassword(row.PasswordHash, []byte(req.Password)) != nil {
				return errBadCredentials
			}
			endUserID = row.ID
			if err := insertSession(ctx, tx, row.ID, refreshToken); err != nil {
				return err
			}
			return auditlog.Write(ctx, tx, row.ID, "auth.login", req.Email, nil)
		})
		if errors.Is(err, pgx.ErrNoRows) {
			_ = bcrypt.CompareHashAndPassword(dummyBcryptHash, []byte(req.Password))
			http.Error(w, "invalid credentials", http.StatusUnauthorized)
			return
		}
		if errors.Is(err, errBadCredentials) {
			http.Error(w, "invalid credentials", http.StatusUnauthorized)
			return
		}
		if err != nil {
			internalError(w, "login: tx", err)
			return
		}

		expiresAt := time.Now().Add(accessTokenTTL)
		accessToken, err := d.Signer.Sign(auth.Claims{
			EndUserID: endUserID, TenantID: tenantID, ProjectID: projectID,
			ExpiresAt: expiresAt,
		})
		if err != nil {
			internalError(w, "login: sign", err)
			return
		}
		writeJSON(w, sessionResponse{
			AccessToken: accessToken, RefreshToken: refreshToken,
			EndUserID: endUserID, ExpiresAt: expiresAt.UTC().Format(time.RFC3339),
		})
	}
}

// refreshHandler — m1.md 4.1.4: rotate refresh, issue new access token.
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
		newRefresh, err := randomHex("", 32)
		if err != nil {
			internalError(w, "refresh: rand", err)
			return
		}

		var endUserID int64
		err = d.Pool.Q(ctx, func(tx pgx.Tx) error {
			q := sqlcgen.New(tx)
			row, err := q.GetSessionByRefreshHash(ctx, oldHash[:])
			if err != nil {
				return err
			}
			if row.RevokedAt.Valid || row.ExpiresAt.Time.Before(time.Now()) {
				return errSessionRevoked
			}
			endUserID = row.EndUserID
			if err := q.RevokeSession(ctx, row.ID); err != nil {
				return err
			}
			if err := insertSession(ctx, tx, row.EndUserID, newRefresh); err != nil {
				return err
			}
			return auditlog.Write(ctx, tx, row.EndUserID, "auth.refresh", "", nil)
		})
		if errors.Is(err, pgx.ErrNoRows) || errors.Is(err, errSessionRevoked) {
			http.Error(w, "invalid refresh", http.StatusUnauthorized)
			return
		}
		if err != nil {
			internalError(w, "refresh: tx", err)
			return
		}

		expiresAt := time.Now().Add(accessTokenTTL)
		accessToken, err := d.Signer.Sign(auth.Claims{
			EndUserID: endUserID, TenantID: tenantID, ProjectID: projectID,
			ExpiresAt: expiresAt,
		})
		if err != nil {
			internalError(w, "refresh: sign", err)
			return
		}
		writeJSON(w, sessionResponse{
			AccessToken: accessToken, RefreshToken: newRefresh,
			EndUserID: endUserID, ExpiresAt: expiresAt.UTC().Format(time.RFC3339),
		})
	}
}

// logoutHandler — m1.md 4.1.5
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
			if err := q.RevokeSessionByRefreshHash(ctx, hash[:]); err != nil {
				return err
			}
			return auditlog.Write(ctx, tx, 0, "auth.logout", "", nil)
		})
		if err != nil {
			internalError(w, "logout: tx", err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// customTokenHandler — m1.md 4.1.7: tenant-signed JWT carrying an
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
			secret     []byte
			endUserID  int64
			externalID string
			refreshTok string
		)
		var err error
		refreshTok, err = randomHex("", 32)
		if err != nil {
			internalError(w, "custom-token: rand", err)
			return
		}

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
			if _, err := jwt.ParseWithClaims(req.Token, parsed, func(t *jwt.Token) (any, error) {
				if t.Method.Alg() != jwt.SigningMethodHS256.Alg() {
					return nil, fmt.Errorf("unsupported alg %v", t.Method.Alg())
				}
				return secret, nil
			}); err != nil {
				return errCustomTokenInvalid
			}
			if parsed.External == "" {
				return errCustomTokenInvalid
			}
			externalID = parsed.External

			id, err := q.UpsertEndUserByExternalID(ctx, sqlcgen.UpsertEndUserByExternalIDParams{
				ProjectID:  projectID,
				ExternalID: externalID,
			})
			if err != nil {
				return fmt.Errorf("upsert end_user: %w", err)
			}
			endUserID = id
			if err := insertSession(ctx, tx, id, refreshTok); err != nil {
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
		case err != nil:
			internalError(w, "custom-token: tx", err)
			return
		}

		expiresAt := time.Now().Add(accessTokenTTL)
		accessToken, err := d.Signer.Sign(auth.Claims{
			EndUserID: endUserID, TenantID: tenantID, ProjectID: projectID,
			ExpiresAt: expiresAt,
		})
		if err != nil {
			internalError(w, "custom-token: sign", err)
			return
		}
		writeJSON(w, sessionResponse{
			AccessToken: accessToken, RefreshToken: refreshTok,
			EndUserID: endUserID, ExpiresAt: expiresAt.UTC().Format(time.RFC3339),
		})
	}
}

type customTokenClaims struct {
	jwt.RegisteredClaims
	External string `json:"external_id"`
}

var (
	errBadCredentials           = errors.New("auth: bad credentials")
	errSessionRevoked           = errors.New("auth: session revoked or expired")
	errCustomTokenNotConfigured = errors.New("auth: custom token secret not set")
	errCustomTokenInvalid       = errors.New("auth: custom token invalid")
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

func insertSession(ctx context.Context, tx pgx.Tx, endUserID int64, refreshToken string) error {
	q := sqlcgen.New(tx)
	sum := sha256.Sum256([]byte(refreshToken))
	_, err := q.CreateSession(ctx, sqlcgen.CreateSessionParams{
		EndUserID:   endUserID,
		RefreshHash: sum[:],
		ExpiresAt:   pgtype.Timestamptz{Time: time.Now().Add(refreshTokenTTL), Valid: true},
	})
	if err != nil {
		return fmt.Errorf("insert session: %w", err)
	}
	return nil
}

func randomHex(prefix string, nbytes int) (string, error) {
	buf := make([]byte, nbytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(buf), nil
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
	return true
}

func writeJSON(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	//nolint:gosec // gosec G117 false-positive: handler contract is to return tokens.
	_ = json.NewEncoder(w).Encode(body)
}

func internalError(w http.ResponseWriter, msg string, err error) {
	slog.Error(msg, "error", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}

func validateEmail(s string) bool {
	at := strings.IndexByte(s, '@')
	dot := strings.LastIndexByte(s, '.')
	return at > 0 && dot > at && len(s) >= 5
}

func isUniqueViolation(err error) bool {
	var pgErr *pgxPgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "23505"
	}
	// Fallback for the wrapped path — pgconn.PgError wrapped via %w from sqlc.
	return strings.Contains(err.Error(), "23505")
}

// pgxPgError is a thin shim so we don't pull pgconn into this file; the
// substring check above covers the practical case for now.
type pgxPgError struct {
	Code string
}

func (e *pgxPgError) Error() string { return "pg error " + e.Code }
