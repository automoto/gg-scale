package controlpanel

// Optional public tenant sign-up with manual platform-admin approval.
// Deny-by-default: a developer submits a request, a platform admin approves or
// denies it, and the tenant is created only when an approved requester accepts
// the emailed invite. Players never self-join; this is a control-panel-only
// developer onboarding path.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"golang.org/x/crypto/bcrypt"

	"github.com/ggscale/ggscale/internal/auditlog"
	sqlcgen "github.com/ggscale/ggscale/internal/db/sqlc"
	"github.com/ggscale/ggscale/internal/mailer"
	"github.com/ggscale/ggscale/internal/verifycode"
	"github.com/ggscale/ggscale/internal/webutil"
)

const (
	pathAdminTenantSignups = "/v1/control-panel/admin/tenant-signups"
	tenantNameMin          = 2
	tenantNameMax          = 60
	projectDescriptionMax  = 2000
	studioNameMax          = 120
)

var errSignupNameTaken = errors.New("control panel: tenant name is taken")

// --- view models -----------------------------------------------------------

// TenantSignupFormView backs the public request form and its field errors.
type TenantSignupFormView struct {
	CSRFToken          string
	Email              string
	TenantName         string
	ProjectDescription string
	StudioName         string
	FieldErrors        map[string]string
}

// TenantSignupRequestView is one pending request in the admin review table.
type TenantSignupRequestView struct {
	ID                 int64
	Email              string
	TenantName         string
	ProjectDescription string
	StudioName         string
	CreatedAt          time.Time
}

// TenantSignupRequestsView backs the platform-admin review page (toggle + list).
type TenantSignupRequestsView struct {
	UserEmail     string
	CSRFToken     string
	SignupEnabled bool
	Requests      []TenantSignupRequestView
	Message       string
}

// TenantSignupAcceptView backs the invite-acceptance (set-password) page.
type TenantSignupAcceptView struct {
	Code        string
	Email       string
	TenantName  string
	NewUser     bool
	ExpiresAt   time.Time
	CSRFToken   string
	Error       string
	FieldErrors map[string]string
}

// --- validation (pure, unit-testable) --------------------------------------

type tenantSignupInput struct {
	Email               string
	RequestedTenantName string
	ProjectDescription  string
	StudioName          string
}

// validTenantName enforces a length window and rejects control characters.
func validTenantName(name string) bool {
	name = strings.TrimSpace(name)
	if len(name) < tenantNameMin || len(name) > tenantNameMax {
		return false
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// validateTenantSignupInput returns field-keyed errors; empty means valid.
func validateTenantSignupInput(in tenantSignupInput) map[string]string {
	errs := map[string]string{}
	if !validControlPanelEmail(normalizeEmail(in.Email)) {
		errs["email"] = "Enter a valid email address."
	}
	if !validTenantName(in.RequestedTenantName) {
		errs["tenant_name"] = fmt.Sprintf("Tenant name must be %d–%d characters.", tenantNameMin, tenantNameMax)
	}
	desc := strings.TrimSpace(in.ProjectDescription)
	if desc == "" || len(desc) > projectDescriptionMax {
		errs["project_description"] = fmt.Sprintf("Describe your game/project (up to %d characters).", projectDescriptionMax)
	}
	if len(in.StudioName) > studioNameMax {
		errs["studio_name"] = fmt.Sprintf("Studio name is too long (max %d characters).", studioNameMax)
	}
	return errs
}

// --- shared DB helpers -----------------------------------------------------

// publicSignupEnabled reads the platform toggle, failing closed on error so a
// transient DB issue can never expose the form.
func (h *Handler) publicSignupEnabled(ctx context.Context) bool {
	var enabled bool
	if err := h.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		var e error
		enabled, e = sqlcgen.New(tx).GetPublicSignupEnabled(ctx)
		return e
	}); err != nil {
		return false
	}
	return enabled
}

func (h *Handler) tenantNameTaken(ctx context.Context, name string, excludeRequestID int64) (bool, error) {
	var taken bool
	err := h.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		var e error
		taken, e = sqlcgen.New(tx).TenantNameTaken(ctx, sqlcgen.TenantNameTakenParams{
			Name:             name,
			ExcludeRequestID: excludeRequestID,
		})
		return e
	})
	return taken, err
}

// --- public request form ---------------------------------------------------

func (h *Handler) tenantSignupPage(w http.ResponseWriter, r *http.Request) {
	if !h.publicSignupEnabled(r.Context()) {
		h.renderSignupClosed(w, r)
		return
	}
	webutil.Render(r, w, TenantSignupPage(TenantSignupFormView{CSRFToken: webutil.CSRFTokenFromContext(r.Context())}))
}

func (h *Handler) tenantSignupHandler(w http.ResponseWriter, r *http.Request) {
	if !h.publicSignupEnabled(r.Context()) {
		h.renderSignupClosed(w, r)
		return
	}
	if !webutil.ParseForm(w, r) {
		return
	}
	in := tenantSignupInput{
		Email:               r.Form.Get("email"),
		RequestedTenantName: strings.TrimSpace(r.Form.Get("tenant_name")),
		ProjectDescription:  strings.TrimSpace(r.Form.Get("project_description")),
		StudioName:          strings.TrimSpace(r.Form.Get("studio_name")),
	}
	fieldErrors := validateTenantSignupInput(in)
	if len(fieldErrors) == 0 {
		// Tenant names aren't secret, so a name collision is surfaced as a normal
		// "pick another" validation (unlike a duplicate email — see below).
		taken, err := h.tenantNameTaken(r.Context(), in.RequestedTenantName, 0)
		if err != nil {
			webutil.InternalError(w, "tenant signup: name check", err)
			return
		}
		if taken {
			fieldErrors["tenant_name"] = "That tenant name is taken. Choose another."
		}
	}
	if len(fieldErrors) > 0 {
		w.WriteHeader(http.StatusUnprocessableEntity)
		webutil.Render(r, w, TenantSignupPage(TenantSignupFormView{
			CSRFToken:          webutil.CSRFTokenFromContext(r.Context()),
			Email:              strings.TrimSpace(in.Email),
			TenantName:         in.RequestedTenantName,
			ProjectDescription: in.ProjectDescription,
			StudioName:         in.StudioName,
			FieldErrors:        fieldErrors,
		}))
		return
	}
	if err := h.createTenantSignupRequest(r.Context(), normalizeEmail(in.Email), in); err != nil {
		if errors.Is(err, errSignupNameTaken) {
			// A concurrent submit won the name between the pre-check and the
			// insert: tell the requester to pick another rather than silently
			// dropping their request (matches the pre-check's behaviour).
			w.WriteHeader(http.StatusUnprocessableEntity)
			webutil.Render(r, w, TenantSignupPage(TenantSignupFormView{
				CSRFToken:          webutil.CSRFTokenFromContext(r.Context()),
				Email:              strings.TrimSpace(in.Email),
				TenantName:         in.RequestedTenantName,
				ProjectDescription: in.ProjectDescription,
				StudioName:         in.StudioName,
				FieldErrors:        map[string]string{"tenant_name": "That tenant name is taken. Choose another."},
			}))
			return
		}
		webutil.InternalError(w, "tenant signup: create", err)
		return
	}
	// Anti-enumeration: identical acknowledgement whether the row was created or
	// silently dropped (an email that already has a request), so the form can't
	// be used to probe which emails have applied.
	webutil.Render(r, w, TenantSignupAcknowledgePage())
}

// createTenantSignupRequest inserts the request. A duplicate email is a silent
// no-op (anti-enumeration), but a name-index race returns errSignupNameTaken so
// the caller can tell the requester to pick another name.
func (h *Handler) createTenantSignupRequest(ctx context.Context, email string, in tenantSignupInput) error {
	var studio *string
	if in.StudioName != "" {
		studio = &in.StudioName
	}
	err := h.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		_, qerr := sqlcgen.New(tx).CreateTenantSignupRequest(ctx, sqlcgen.CreateTenantSignupRequestParams{
			Email:               email,
			RequestedTenantName: in.RequestedTenantName,
			ProjectDescription:  in.ProjectDescription,
			StudioName:          studio,
		})
		return qerr
	})
	if constraint, ok := uniqueViolationConstraint(err); ok {
		if constraint == "tenant_signup_requests_live_name_key" {
			return errSignupNameTaken
		}
		return nil // duplicate email → silent no-op
	}
	return err
}

// uniqueViolationConstraint returns the violated constraint/index name when err
// is a Postgres unique violation (SQLSTATE 23505).
func uniqueViolationConstraint(err error) (string, bool) {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return pgErr.ConstraintName, true
	}
	return "", false
}

func (h *Handler) renderSignupClosed(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusNotFound)
	webutil.Render(r, w, TenantSignupClosedPage())
}

// --- platform-admin review -------------------------------------------------

func (h *Handler) tenantSignupRequestsPage(w http.ResponseWriter, r *http.Request) {
	session, _ := sessionFromContext(r.Context())
	var (
		enabled bool
		rows    []sqlcgen.ListPendingTenantSignupRequestsRow
	)
	if err := h.pool.BootstrapQ(r.Context(), func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		var e error
		if enabled, e = q.GetPublicSignupEnabled(r.Context()); e != nil {
			return e
		}
		rows, e = q.ListPendingTenantSignupRequests(r.Context())
		return e
	}); err != nil {
		webutil.InternalError(w, "tenant signups: list", err)
		return
	}
	reqs := make([]TenantSignupRequestView, 0, len(rows))
	for _, row := range rows {
		v := TenantSignupRequestView{
			ID:                 row.ID,
			Email:              row.Email,
			TenantName:         row.RequestedTenantName,
			ProjectDescription: row.ProjectDescription,
			CreatedAt:          row.CreatedAt.Time,
		}
		if row.StudioName != nil {
			v.StudioName = *row.StudioName
		}
		reqs = append(reqs, v)
	}
	webutil.Render(r, w, TenantSignupRequestsPage(TenantSignupRequestsView{
		UserEmail:     session.User.Email,
		CSRFToken:     session.CSRFToken,
		SignupEnabled: enabled,
		Requests:      reqs,
		Message:       r.URL.Query().Get("flash"),
	}))
}

func (h *Handler) setPublicSignupEnabledHandler(w http.ResponseWriter, r *http.Request) {
	if !webutil.ParseForm(w, r) {
		return
	}
	session, _ := sessionFromContext(r.Context())
	enabled := r.Form.Get("enabled") == "on" || r.Form.Get("enabled") == "true"
	err := h.pool.BootstrapQ(r.Context(), func(tx pgx.Tx) error {
		if e := sqlcgen.New(tx).SetPublicSignupEnabled(r.Context(), sqlcgen.SetPublicSignupEnabledParams{
			Enabled:   enabled,
			UpdatedBy: &session.User.ID,
		}); e != nil {
			return e
		}
		action := "control_panel.tenant_signup.config_disable"
		if enabled {
			action = "control_panel.tenant_signup.config_enable"
		}
		return auditlog.WritePlatform(r.Context(), tx, session.User.ID, action, "platform_signup_config", map[string]any{"enabled": enabled})
	})
	if err != nil {
		webutil.InternalError(w, "tenant signup: toggle", err)
		return
	}
	msg := "Public tenant sign-up disabled."
	if enabled {
		msg = "Public tenant sign-up enabled."
	}
	h.redirectSignupAdmin(w, r, msg)
}

func (h *Handler) approveTenantSignupHandler(w http.ResponseWriter, r *http.Request) {
	id, ok := parsePathID(w, r, "id")
	if !ok {
		return
	}
	if !webutil.ParseForm(w, r) {
		return
	}
	session, _ := sessionFromContext(r.Context())
	finalName := strings.TrimSpace(r.Form.Get("tenant_name"))
	if !validTenantName(finalName) {
		h.redirectSignupAdmin(w, r, fmt.Sprintf("Enter a valid tenant name (%d–%d characters).", tenantNameMin, tenantNameMax))
		return
	}
	taken, err := h.tenantNameTaken(r.Context(), finalName, id)
	if err != nil {
		webutil.InternalError(w, "tenant signup: approve name check", err)
		return
	}
	if taken {
		h.redirectSignupAdmin(w, r, "That tenant name is taken; edit it and try again.")
		return
	}

	code, err := verifycode.GenerateInviteCode()
	if err != nil {
		webutil.InternalError(w, "tenant signup: invite code", err)
		return
	}
	codeHash := verifycode.Hash(nil, code)
	expires := h.now().Add(verifycode.InviteTTL)

	var (
		reqEmail string
		affected int64
	)
	err = h.pool.BootstrapQ(r.Context(), func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		n, aerr := q.ApproveTenantSignupRequest(r.Context(), sqlcgen.ApproveTenantSignupRequestParams{
			FinalTenantName:  &finalName,
			CodeHash:         codeHash,
			CodeExpiresAt:    pgtype.Timestamptz{Time: expires, Valid: true},
			ReviewedByUserID: &session.User.ID,
			ID:               id,
		})
		if aerr != nil {
			if isUniqueViolation(aerr) {
				return errSignupNameTaken
			}
			return aerr
		}
		affected = n
		if n == 0 {
			return nil
		}
		req, gerr := q.GetTenantSignupRequestByID(r.Context(), id)
		if gerr != nil {
			return gerr
		}
		reqEmail = req.Email
		return auditlog.WritePlatform(r.Context(), tx, session.User.ID, "control_panel.tenant_signup.approve",
			strconv.FormatInt(id, 10), map[string]any{"tenant_name": finalName, "email": req.Email})
	})
	if errors.Is(err, errSignupNameTaken) {
		h.redirectSignupAdmin(w, r, "That tenant name is taken; edit it and try again.")
		return
	}
	if err != nil {
		webutil.InternalError(w, "tenant signup: approve", err)
		return
	}
	if affected == 0 {
		h.redirectSignupAdmin(w, r, "That request was already handled.")
		return
	}
	h.sendSignupApprovalEmail(r.Context(), reqEmail, finalName, code, expires)
	h.redirectSignupAdmin(w, r, "Approved "+finalName+" — invite emailed to "+reqEmail+".")
}

func (h *Handler) denyTenantSignupHandler(w http.ResponseWriter, r *http.Request) {
	id, ok := parsePathID(w, r, "id")
	if !ok {
		return
	}
	if !webutil.ParseForm(w, r) {
		return
	}
	session, _ := sessionFromContext(r.Context())
	reason := strings.TrimSpace(r.Form.Get("reason"))
	var reasonPtr *string
	if reason != "" {
		reasonPtr = &reason
	}

	var (
		reqEmail string
		affected int64
	)
	err := h.pool.BootstrapQ(r.Context(), func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		n, derr := q.DenyTenantSignupRequest(r.Context(), sqlcgen.DenyTenantSignupRequestParams{
			ReviewReason:     reasonPtr,
			ReviewedByUserID: &session.User.ID,
			ID:               id,
		})
		if derr != nil {
			return derr
		}
		affected = n
		if n == 0 {
			return nil
		}
		req, gerr := q.GetTenantSignupRequestByID(r.Context(), id)
		if gerr != nil {
			return gerr
		}
		reqEmail = req.Email
		return auditlog.WritePlatform(r.Context(), tx, session.User.ID, "control_panel.tenant_signup.deny",
			strconv.FormatInt(id, 10), map[string]any{"email": req.Email, "reason": reason})
	})
	if err != nil {
		webutil.InternalError(w, "tenant signup: deny", err)
		return
	}
	if affected == 0 {
		h.redirectSignupAdmin(w, r, "That request was already handled.")
		return
	}
	h.sendSignupDenialEmail(r.Context(), reqEmail, reason)
	h.redirectSignupAdmin(w, r, "Request denied.")
}

func (h *Handler) redirectSignupAdmin(w http.ResponseWriter, r *http.Request, msg string) {
	htmxRedirect(w, r, pathAdminTenantSignups+queryFlash+url.QueryEscape(msg))
}

// --- invite acceptance → create tenant -------------------------------------

type signupLookup struct {
	Email      string
	TenantName string
	ExpiresAt  time.Time
	IsExisting bool
}

// lookupSignupRequest resolves an approved request by its hashed code, mirroring
// lookupInvite: any non-acceptable state maps to a generic not-found.
func (h *Handler) lookupSignupRequest(ctx context.Context, code string) (signupLookup, error) {
	if code == "" {
		return signupLookup{}, errInviteNotFound
	}
	codeHash := verifycode.Hash(nil, code)

	var out signupLookup
	err := h.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		req, qerr := q.GetTenantSignupRequestByCodeHash(ctx, codeHash)
		if errors.Is(qerr, pgx.ErrNoRows) {
			return errInviteNotFound
		}
		if qerr != nil {
			return qerr
		}
		out.Email = req.Email
		out.TenantName = signupTenantName(req.RequestedTenantName, req.FinalTenantName)
		out.ExpiresAt = req.CodeExpiresAt.Time
		existing, gerr := q.GetControlPanelUserAnyStatusByEmail(ctx, req.Email)
		switch {
		case errors.Is(gerr, pgx.ErrNoRows):
			return nil
		case gerr != nil:
			return gerr
		case existing.DisabledAt.Valid:
			return errInviteForDisabledAccount
		default:
			out.IsExisting = true
			return nil
		}
	})
	if err != nil {
		return signupLookup{}, err
	}
	if verifycode.Expired(out.ExpiresAt, h.now()) {
		return signupLookup{}, errInviteExpired
	}
	return out, nil
}

func signupTenantName(requested string, final *string) string {
	if final != nil && strings.TrimSpace(*final) != "" {
		return *final
	}
	return requested
}

type signupAcceptInput struct {
	Code     string
	Password string
}

type signupAcceptResult struct {
	UserID     int64
	Email      string
	IsNewUser  bool
	TenantName string
}

// acceptTenantSignup provisions the requester's control_panel_user (verifying a
// current password for existing accounts), creates the bare tenant with them as
// owner, and marks the request accepted — all in one transaction.
func (h *Handler) acceptTenantSignup(ctx context.Context, in signupAcceptInput) (signupAcceptResult, error) {
	if in.Code == "" {
		return signupAcceptResult{}, errInviteNotFound
	}
	codeHash := verifycode.Hash(nil, in.Code)

	var out signupAcceptResult
	err := h.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		req, qerr := q.GetTenantSignupRequestByCodeHash(ctx, codeHash)
		if errors.Is(qerr, pgx.ErrNoRows) {
			return errInviteNotFound
		}
		if qerr != nil {
			return qerr
		}
		if verifycode.Expired(req.CodeExpiresAt.Time, h.now()) {
			return errInviteExpired
		}
		name := signupTenantName(req.RequestedTenantName, req.FinalTenantName)

		userID, email, isNew, uerr := h.resolveSignupUser(ctx, q, req.Email, in.Password)
		if uerr != nil {
			return uerr
		}

		tenantID, cerr := q.ControlPanelCreateTenantBare(ctx, sqlcgen.ControlPanelCreateTenantBareParams{
			ActorUserID: userID,
			TenantName:  name,
		})
		if cerr != nil {
			if isUniqueViolation(cerr) {
				return errSignupNameTaken
			}
			return fmt.Errorf("create tenant bare: %w", cerr)
		}
		if h.cfg.EnforceNewTenantQuotas {
			if err := q.SetTenantEnforceQuotas(ctx, sqlcgen.SetTenantEnforceQuotasParams{
				TenantID:      tenantID,
				EnforceQuotas: true,
			}); err != nil {
				return fmt.Errorf("set enforce_quotas: %w", err)
			}
		}
		if h.rbac != nil {
			if err := h.rbac.SetControlPanelMembershipRoleTx(ctx, tx, userID, tenantID, roleOwner); err != nil {
				return fmt.Errorf("rbac signup owner: %w", err)
			}
		}
		if err := q.MarkTenantSignupAccepted(ctx, sqlcgen.MarkTenantSignupAcceptedParams{
			ID:       req.ID,
			TenantID: &tenantID,
		}); err != nil {
			return err
		}
		out = signupAcceptResult{UserID: userID, Email: email, IsNewUser: isNew, TenantName: name}
		return nil
	})
	if err != nil {
		return signupAcceptResult{}, err
	}
	h.reloadRBACPolicy(ctx)
	return out, nil
}

// resolveSignupUser returns the control panel user for the request email:
// creating a verified account for a new email (enforcing the password floor),
// or verifying the CURRENT password for an existing one so mere possession of
// the magic link can't hijack the account.
func (h *Handler) resolveSignupUser(ctx context.Context, q *sqlcgen.Queries, email, password string) (int64, string, bool, error) {
	user, gerr := q.GetControlPanelUserAnyStatusByEmail(ctx, email)
	switch {
	case errors.Is(gerr, pgx.ErrNoRows):
		if len(password) < minControlPanelPassLen {
			return 0, "", false, errWeakPassword
		}
		pwHash, herr := bcrypt.GenerateFromPassword([]byte(password), bcryptCost)
		if herr != nil {
			return 0, "", false, fmt.Errorf("signup bcrypt: %w", herr)
		}
		created, cerr := q.CreateVerifiedControlPanelUser(ctx, sqlcgen.CreateVerifiedControlPanelUserParams{
			Email:           email,
			PasswordHash:    pwHash,
			IsPlatformAdmin: false,
		})
		if cerr != nil {
			return 0, "", false, fmt.Errorf("signup create user: %w", cerr)
		}
		return created.ID, created.Email, true, nil
	case gerr != nil:
		return 0, "", false, gerr
	case user.DisabledAt.Valid:
		return 0, "", false, errInviteForDisabledAccount
	}
	if bcrypt.CompareHashAndPassword(user.PasswordHash, []byte(password)) != nil {
		return 0, "", false, errInvalidCredentials
	}
	return user.ID, user.Email, false, nil
}

func (h *Handler) tenantSignupAcceptPage(w http.ResponseWriter, r *http.Request) {
	code := r.URL.Query().Get("code")
	res, err := h.lookupSignupRequest(r.Context(), code)
	if err != nil {
		h.renderSignupAcceptError(w, r, err)
		return
	}
	webutil.Render(r, w, TenantSignupAcceptPage(TenantSignupAcceptView{
		Code:       code,
		Email:      res.Email,
		TenantName: res.TenantName,
		NewUser:    !res.IsExisting,
		ExpiresAt:  res.ExpiresAt,
		CSRFToken:  webutil.CSRFTokenFromContext(r.Context()),
	}))
}

func (h *Handler) tenantSignupAcceptHandler(w http.ResponseWriter, r *http.Request) {
	if !webutil.ParseForm(w, r) {
		return
	}
	code := r.Form.Get("code")
	password := r.Form.Get("password")

	res, err := h.acceptTenantSignup(r.Context(), signupAcceptInput{Code: code, Password: password})
	if err != nil {
		lookup, lerr := h.lookupSignupRequest(r.Context(), code)
		if lerr != nil {
			h.renderSignupAcceptError(w, r, lerr)
			return
		}
		view := TenantSignupAcceptView{
			Code:       code,
			Email:      lookup.Email,
			TenantName: lookup.TenantName,
			NewUser:    !lookup.IsExisting,
			ExpiresAt:  lookup.ExpiresAt,
			CSRFToken:  webutil.CSRFTokenFromContext(r.Context()),
		}
		status := http.StatusInternalServerError
		switch {
		case errors.Is(err, errWeakPassword):
			status = http.StatusUnprocessableEntity
			view.FieldErrors = map[string]string{"password": "Password must be at least 12 characters."}
		case errors.Is(err, errInvalidCredentials):
			status = http.StatusUnauthorized
			view.FieldErrors = map[string]string{"password": "Incorrect password for that account."}
		case errors.Is(err, errSignupNameTaken):
			status = http.StatusConflict
			view.Error = "That tenant name is no longer available. Contact your platform admin."
		case errors.Is(err, errInviteExpired):
			status = http.StatusGone
			view.Error = "This invite has expired. Ask a platform admin to re-approve."
		case errors.Is(err, errInviteNotFound):
			status = http.StatusNotFound
			view.Error = "Invite not found or already used."
		case errors.Is(err, errInviteForDisabledAccount):
			status = http.StatusForbidden
			view.Error = "This account has been disabled. Contact your platform admin."
		default:
			slog.ErrorContext(r.Context(), "accept tenant signup", "err", err)
			view.Error = "Could not create your tenant."
		}
		w.WriteHeader(status)
		webutil.Render(r, w, TenantSignupAcceptPage(view))
		return
	}
	// Auto-login through the shared 2FA gate, then land on their (empty) tenant.
	h.finishLogin(w, r, controlPanelUser{ID: res.UserID, Email: res.Email})
}

func (h *Handler) renderSignupAcceptError(w http.ResponseWriter, r *http.Request, err error) {
	var (
		status int
		msg    string
	)
	switch {
	case errors.Is(err, errInviteExpired):
		status = http.StatusGone
		msg = "This invite has expired."
	case errors.Is(err, errInviteNotFound):
		status = http.StatusNotFound
		msg = "Invite not found or already used."
	case errors.Is(err, errInviteForDisabledAccount):
		status = http.StatusForbidden
		msg = "This account has been disabled. Contact your platform admin."
	default:
		status = http.StatusInternalServerError
		msg = "Could not load this invite."
	}
	w.WriteHeader(status)
	webutil.Render(r, w, TenantSignupAcceptPage(TenantSignupAcceptView{Error: msg, CSRFToken: webutil.CSRFTokenFromContext(r.Context())}))
}

// --- email -----------------------------------------------------------------

func (h *Handler) signupAcceptURL(code string) string {
	base := strings.TrimRight(h.cfg.BaseURL, "/")
	return base + "/v1/control-panel/request-access/accept?code=" + url.QueryEscape(code)
}

func (h *Handler) sendSignupApprovalEmail(ctx context.Context, email, tenantName, code string, expires time.Time) {
	if h.mailer == nil || h.cfg.MailFrom == "" {
		slog.WarnContext(ctx, "tenant signup approval: no mailer configured", "email", email)
		return
	}
	body := strings.TrimSpace(fmt.Sprintf(
		"Your ggscale tenant request for %q was approved.\n\nClick to set up your account and create the tenant (expires %s):\n%s",
		tenantName, expires.UTC().Format("2006-01-02 15:04 UTC"), h.signupAcceptURL(code),
	))
	if err := h.mailer.Send(ctx, mailer.Message{
		From:    h.cfg.MailFrom,
		To:      []string{email},
		Subject: "Your ggscale tenant request was approved",
		Body:    body,
	}); err != nil {
		slog.ErrorContext(ctx, "tenant signup approval mailer", "err", err)
	}
}

func (h *Handler) sendSignupDenialEmail(ctx context.Context, email, reason string) {
	if h.mailer == nil || h.cfg.MailFrom == "" {
		slog.WarnContext(ctx, "tenant signup denial: no mailer configured", "email", email)
		return
	}
	body := "Thanks for your interest in ggscale. After review, we're not able to approve your tenant request at this time."
	if reason != "" {
		body += "\n\nReason: " + reason
	}
	if err := h.mailer.Send(ctx, mailer.Message{
		From:    h.cfg.MailFrom,
		To:      []string{email},
		Subject: "About your ggscale tenant request",
		Body:    body,
	}); err != nil {
		slog.ErrorContext(ctx, "tenant signup denial mailer", "err", err)
	}
}
