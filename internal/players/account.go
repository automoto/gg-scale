package players

// Global player-account auth for the player site. Accounts are
// platform-global (no tenant): every query runs through BootstrapQ against
// player_accounts / player_account_sessions. See docs/temp/player-accounts.md.

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"golang.org/x/crypto/bcrypt"

	sqlcgen "github.com/ggscale/ggscale/internal/db/sqlc"
	"github.com/ggscale/ggscale/internal/mailer"
	"github.com/ggscale/ggscale/internal/observability"
	"github.com/ggscale/ggscale/internal/remoteaddr"
	"github.com/ggscale/ggscale/internal/verifycode"
	"github.com/ggscale/ggscale/internal/webutil"
)

const (
	accountSessionCookieName = "ggscale_account_session"
	accountVerifyCookieName  = "ggscale_account_verify"
	accountBasePath          = "/v1/players/account"
)

var (
	errAccountVerificationDelivery = errors.New("player account: verification email delivery failed")
	errAccountVerifyResendTooSoon  = errors.New("player account: verification resend too soon")
)

func toPgUUID(u uuid.UUID) pgtype.UUID { return pgtype.UUID{Bytes: u, Valid: true} }
func fromPgUUID(p pgtype.UUID) uuid.UUID {
	return uuid.UUID(p.Bytes)
}

// accountSession is the public view of a signed-in global account.
type accountSession struct {
	AccountID   uuid.UUID
	Email       string
	DisplayName string
}

// --- page handlers ---------------------------------------------------------

func (h *Handler) accountLoginPage(w http.ResponseWriter, r *http.Request) {
	webutil.Render(r, w, AccountLoginPage(AccountLoginView{CSRFToken: h.csrf(r)}))
}

func (h *Handler) accountSignupPage(w http.ResponseWriter, r *http.Request) {
	webutil.Render(r, w, AccountSignupPage(AccountSignupView{CSRFToken: h.csrf(r)}))
}

func (h *Handler) accountVerifyPage(w http.ResponseWriter, r *http.Request) {
	p, ok := h.accountVerifyCookie(r)
	if !ok {
		http.Redirect(w, r, accountBasePath+"/login", http.StatusSeeOther)
		return
	}
	webutil.Render(r, w, AccountVerifyPage(AccountVerifyView{Email: p.Email, CSRFToken: h.csrf(r)}))
}

func (h *Handler) accountHomePage(w http.ResponseWriter, r *http.Request) {
	sess, ok := h.accountSessionFromRequest(r)
	if !ok {
		http.Redirect(w, r, accountBasePath+"/login", http.StatusSeeOther)
		return
	}
	projects, err := h.listAccountLinkedProjects(r.Context(), sess.AccountID)
	if err != nil {
		webutil.InternalError(w, "account home: linked projects", err)
		return
	}
	var addrs sqlcgen.GetPlayerAccountRemoteAddrsRow
	if err := h.pool.BootstrapQ(r.Context(), func(tx pgx.Tx) error {
		var e error
		addrs, e = sqlcgen.New(tx).GetPlayerAccountRemoteAddrs(r.Context(), toPgUUID(sess.AccountID))
		return e
	}); err != nil {
		webutil.InternalError(w, "account home: remote addrs", err)
		return
	}
	view := AccountHomeView{
		Email:           sess.Email,
		DisplayName:     sess.DisplayName,
		Projects:        projects,
		CSRFToken:       h.csrf(r),
		Flash:           r.URL.Query().Get("flash"),
		FlashError:      r.URL.Query().Get("error"),
		RemoteAddrCount: remoteAddrCount(addrs),
	}
	webutil.Render(r, w, AccountHomePage(view))
}

// remoteAddrFormRows is the fixed row count on the account form: one input
// per slot (LAN IP, public IP, DNS, iroh).
const remoteAddrFormRows = 4

type connectionAddressSlot string

const (
	connectionSlotIPLAN    connectionAddressSlot = "ip-lan"
	connectionSlotIPPublic connectionAddressSlot = "ip-public"
	connectionSlotDNS      connectionAddressSlot = "dns"
	connectionSlotIroh     connectionAddressSlot = "iroh"
)

func (h *Handler) getAccountRemoteAddrs(ctx context.Context, accountID uuid.UUID) (sqlcgen.GetPlayerAccountRemoteAddrsRow, error) {
	var addrs sqlcgen.GetPlayerAccountRemoteAddrsRow
	err := h.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		var qerr error
		addrs, qerr = sqlcgen.New(tx).GetPlayerAccountRemoteAddrs(ctx, toPgUUID(accountID))
		return qerr
	})
	return addrs, err
}

// errRemoteAddrSlotOccupied signals that a create/update would land on a slot
// that already holds an address; handlers turn it into a 422, not a 500.
var errRemoteAddrSlotOccupied = errors.New("players: connection address slot already set")

// mutateAccountRemoteAddrs runs a read-modify-write over the four address
// columns in a single transaction, locking the account row first so concurrent
// per-slot edits serialize instead of clobbering each other's columns.
func (h *Handler) mutateAccountRemoteAddrs(ctx context.Context, accountID uuid.UUID, mutate func(*sqlcgen.GetPlayerAccountRemoteAddrsRow) error) error {
	return h.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		if _, err := tx.Exec(ctx, `SELECT 1 FROM player_accounts WHERE id = $1 FOR UPDATE`, toPgUUID(accountID)); err != nil {
			return err
		}
		addrs, err := q.GetPlayerAccountRemoteAddrs(ctx, toPgUUID(accountID))
		if err != nil {
			return err
		}
		if err := mutate(&addrs); err != nil {
			return err
		}
		return q.SetPlayerAccountRemoteAddrs(ctx, sqlcgen.SetPlayerAccountRemoteAddrsParams{
			ID:                 toPgUUID(accountID),
			RemoteAddrIpLan:    addrs.RemoteAddrIpLan,
			RemoteAddrIpPublic: addrs.RemoteAddrIpPublic,
			RemoteAddrDns:      addrs.RemoteAddrDns,
			RemoteAddrIroh:     addrs.RemoteAddrIroh,
		})
	})
}

func connectionAddressFromForm(form url.Values) (remoteaddr.Address, map[string]string, error) {
	t, ok := remoteaddr.ParseType(form.Get("addr_type"))
	if !ok {
		return remoteaddr.Address{}, map[string]string{"addr_type": "Pick an address type"}, fmt.Errorf("pick an address type")
	}
	raw := strings.TrimSpace(form.Get("addr_value"))
	addr, err := remoteaddr.Parse(t, raw)
	if err != nil {
		return remoteaddr.Address{}, map[string]string{"addr_value": err.Error()}, err
	}
	return addr, nil, nil
}

func parseConnectionAddressSlot(raw string) (connectionAddressSlot, bool) {
	switch connectionAddressSlot(raw) {
	case connectionSlotIPLAN, connectionSlotIPPublic, connectionSlotDNS, connectionSlotIroh:
		return connectionAddressSlot(raw), true
	default:
		return "", false
	}
}

func slotForConnectionAddress(addr remoteaddr.Address) connectionAddressSlot {
	switch addr.Type {
	case remoteaddr.TypeIP:
		if addr.Scope == remoteaddr.ScopeLAN {
			return connectionSlotIPLAN
		}
		return connectionSlotIPPublic
	case remoteaddr.TypeDNS:
		return connectionSlotDNS
	default:
		return connectionSlotIroh
	}
}

func remoteAddrSlotType(slot connectionAddressSlot) remoteaddr.Type {
	switch slot {
	case connectionSlotDNS:
		return remoteaddr.TypeDNS
	case connectionSlotIroh:
		return remoteaddr.TypeIroh
	default:
		return remoteaddr.TypeIP
	}
}

func remoteAddrSlotLabel(slot connectionAddressSlot) string {
	switch slot {
	case connectionSlotIPLAN:
		return "LAN IP address"
	case connectionSlotIPPublic:
		return "public IP address"
	case connectionSlotDNS:
		return "DNS name"
	default:
		return "Iroh endpoint ID"
	}
}

func remoteAddrSlotValue(addrs sqlcgen.GetPlayerAccountRemoteAddrsRow, slot connectionAddressSlot) *string {
	switch slot {
	case connectionSlotIPLAN:
		return addrs.RemoteAddrIpLan
	case connectionSlotIPPublic:
		return addrs.RemoteAddrIpPublic
	case connectionSlotDNS:
		return addrs.RemoteAddrDns
	default:
		return addrs.RemoteAddrIroh
	}
}

func applyRemoteAddrSlot(addrs *sqlcgen.GetPlayerAccountRemoteAddrsRow, slot connectionAddressSlot, value *string) {
	switch slot {
	case connectionSlotIPLAN:
		addrs.RemoteAddrIpLan = value
	case connectionSlotIPPublic:
		addrs.RemoteAddrIpPublic = value
	case connectionSlotDNS:
		addrs.RemoteAddrDns = value
	case connectionSlotIroh:
		addrs.RemoteAddrIroh = value
	}
}

// remoteAddrCount counts configured address slots without materializing the
// display rows (each non-nil column is exactly one connection address).
func remoteAddrCount(row sqlcgen.GetPlayerAccountRemoteAddrsRow) int {
	n := 0
	for _, v := range []*string{row.RemoteAddrIpLan, row.RemoteAddrIpPublic, row.RemoteAddrDns, row.RemoteAddrIroh} {
		if v != nil {
			n++
		}
	}
	return n
}

func connectionAddressRows(row sqlcgen.GetPlayerAccountRemoteAddrsRow) []ConnectionAddressView {
	set := remoteaddr.SetFromValues(row.RemoteAddrIpLan, row.RemoteAddrIpPublic, row.RemoteAddrDns, row.RemoteAddrIroh)
	rows := make([]ConnectionAddressView, 0, remoteAddrFormRows)
	for _, addr := range set.List() {
		slot := slotForConnectionAddress(addr)
		rows = append(rows, ConnectionAddressView{
			Slot:       string(slot),
			TypeLabel:  remoteAddrSlotLabel(slot),
			Value:      addr.Value,
			ScopeLabel: remoteAddrScopeLabel(addr),
		})
	}
	return rows
}

func (h *Handler) accountRemoteAddrListPage(w http.ResponseWriter, r *http.Request) {
	sess, ok := h.accountSessionFromRequest(r)
	if !ok {
		http.Redirect(w, r, accountBasePath+"/login", http.StatusSeeOther)
		return
	}
	addrs, err := h.getAccountRemoteAddrs(r.Context(), sess.AccountID)
	if err != nil {
		webutil.InternalError(w, "account remote-addr list", err)
		return
	}
	rows := connectionAddressRows(addrs)
	webutil.Render(r, w, ConnectionAddressesPage(ConnectionAddressesView{
		AccountEmail: sess.Email,
		CSRFToken:    h.csrf(r),
		Flash:        r.URL.Query().Get("flash"),
		FlashError:   r.URL.Query().Get("error"),
		Addresses:    rows,
		CanAdd:       len(rows) < remoteAddrFormRows,
	}))
}

func (h *Handler) accountRemoteAddrNewPage(w http.ResponseWriter, r *http.Request) {
	sess, ok := h.accountSessionFromRequest(r)
	if !ok {
		http.Redirect(w, r, accountBasePath+"/login", http.StatusSeeOther)
		return
	}
	webutil.Render(r, w, ConnectionAddressFormPage(ConnectionAddressFormView{
		AccountEmail: sess.Email,
		CSRFToken:    h.csrf(r),
		TypeValue:    string(remoteaddr.TypeIP),
	}))
}

func (h *Handler) accountRemoteAddrCreate(w http.ResponseWriter, r *http.Request) {
	sess, ok := h.accountSessionFromRequest(r)
	if !ok {
		http.Redirect(w, r, accountBasePath+"/login", http.StatusSeeOther)
		return
	}
	if !webutil.ParseForm(w, r) {
		return
	}
	addr, fieldErrors, err := connectionAddressFromForm(r.Form)
	if err != nil {
		w.WriteHeader(http.StatusUnprocessableEntity)
		webutil.Render(r, w, ConnectionAddressFormPage(ConnectionAddressFormView{
			AccountEmail: sess.Email,
			CSRFToken:    h.csrf(r),
			TypeValue:    r.Form.Get("addr_type"),
			Value:        strings.TrimSpace(r.Form.Get("addr_value")),
			Error:        "Fix the address and try again.",
			FieldErrors:  fieldErrors,
		}))
		return
	}
	slot := slotForConnectionAddress(addr)
	err = h.mutateAccountRemoteAddrs(r.Context(), sess.AccountID, func(addrs *sqlcgen.GetPlayerAccountRemoteAddrsRow) error {
		if remoteAddrSlotValue(*addrs, slot) != nil {
			return errRemoteAddrSlotOccupied
		}
		applyRemoteAddrSlot(addrs, slot, &addr.Value)
		return nil
	})
	if errors.Is(err, errRemoteAddrSlotOccupied) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		webutil.Render(r, w, ConnectionAddressFormPage(ConnectionAddressFormView{
			AccountEmail: sess.Email,
			CSRFToken:    h.csrf(r),
			TypeValue:    string(addr.Type),
			Value:        addr.Value,
			Error:        "That connection address type is already set.",
			FieldErrors:  map[string]string{"addr_type": remoteAddrSlotLabel(slot) + " is already set"},
		}))
		return
	}
	if err != nil {
		webutil.InternalError(w, "account remote-addr create", err)
		return
	}
	h.redirectRemoteAddrs(w, r, "Connection address added.", "")
}

func (h *Handler) accountRemoteAddrEditPage(w http.ResponseWriter, r *http.Request) {
	sess, ok := h.accountSessionFromRequest(r)
	if !ok {
		http.Redirect(w, r, accountBasePath+"/login", http.StatusSeeOther)
		return
	}
	slot, ok := parseConnectionAddressSlot(chi.URLParam(r, "slot"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	addrs, err := h.getAccountRemoteAddrs(r.Context(), sess.AccountID)
	if err != nil {
		webutil.InternalError(w, "account remote-addr edit", err)
		return
	}
	value := remoteAddrSlotValue(addrs, slot)
	if value == nil {
		http.NotFound(w, r)
		return
	}
	webutil.Render(r, w, ConnectionAddressFormPage(ConnectionAddressFormView{
		AccountEmail: sess.Email,
		CSRFToken:    h.csrf(r),
		Slot:         string(slot),
		TypeValue:    string(remoteAddrSlotType(slot)),
		Value:        *value,
		Editing:      true,
	}))
}

func (h *Handler) accountRemoteAddrUpdateSlot(w http.ResponseWriter, r *http.Request) {
	sess, ok := h.accountSessionFromRequest(r)
	if !ok {
		http.Redirect(w, r, accountBasePath+"/login", http.StatusSeeOther)
		return
	}
	slot, ok := parseConnectionAddressSlot(chi.URLParam(r, "slot"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	if !webutil.ParseForm(w, r) {
		return
	}
	addr, fieldErrors, err := connectionAddressFromForm(r.Form)
	if err != nil {
		w.WriteHeader(http.StatusUnprocessableEntity)
		webutil.Render(r, w, ConnectionAddressFormPage(ConnectionAddressFormView{
			AccountEmail: sess.Email,
			CSRFToken:    h.csrf(r),
			Slot:         string(slot),
			TypeValue:    r.Form.Get("addr_type"),
			Value:        strings.TrimSpace(r.Form.Get("addr_value")),
			Error:        "Fix the address and try again.",
			FieldErrors:  fieldErrors,
			Editing:      true,
		}))
		return
	}
	// The edited value may resolve to a different slot (e.g. a public IP
	// corrected to a LAN IP): move it there when that slot is free rather than
	// forcing a delete-then-add.
	target := slotForConnectionAddress(addr)
	err = h.mutateAccountRemoteAddrs(r.Context(), sess.AccountID, func(addrs *sqlcgen.GetPlayerAccountRemoteAddrsRow) error {
		if target != slot && remoteAddrSlotValue(*addrs, target) != nil {
			return errRemoteAddrSlotOccupied
		}
		if target != slot {
			applyRemoteAddrSlot(addrs, slot, nil)
		}
		applyRemoteAddrSlot(addrs, target, &addr.Value)
		return nil
	})
	if errors.Is(err, errRemoteAddrSlotOccupied) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		webutil.Render(r, w, ConnectionAddressFormPage(ConnectionAddressFormView{
			AccountEmail: sess.Email,
			CSRFToken:    h.csrf(r),
			Slot:         string(slot),
			TypeValue:    r.Form.Get("addr_type"),
			Value:        strings.TrimSpace(r.Form.Get("addr_value")),
			Error:        "You already have a " + remoteAddrSlotLabel(target) + ". Edit or remove it first.",
			FieldErrors:  map[string]string{"addr_value": remoteAddrSlotLabel(target) + " is already set"},
			Editing:      true,
		}))
		return
	}
	if err != nil {
		webutil.InternalError(w, "account remote-addr update", err)
		return
	}
	h.redirectRemoteAddrs(w, r, "Connection address updated.", "")
}

// accountRemoteAddrDeletePage renders a confirmation step before deletion. The
// player site ships no JavaScript, so an in-place confirm dialog can't gate the
// POST — a dedicated GET page is the JS-less equivalent.
func (h *Handler) accountRemoteAddrDeletePage(w http.ResponseWriter, r *http.Request) {
	sess, ok := h.accountSessionFromRequest(r)
	if !ok {
		http.Redirect(w, r, accountBasePath+"/login", http.StatusSeeOther)
		return
	}
	slot, ok := parseConnectionAddressSlot(chi.URLParam(r, "slot"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	addrs, err := h.getAccountRemoteAddrs(r.Context(), sess.AccountID)
	if err != nil {
		webutil.InternalError(w, "account remote-addr delete confirm", err)
		return
	}
	value := remoteAddrSlotValue(addrs, slot)
	if value == nil {
		http.NotFound(w, r)
		return
	}
	webutil.Render(r, w, ConnectionAddressDeletePage(ConnectionAddressDeleteView{
		AccountEmail: sess.Email,
		CSRFToken:    h.csrf(r),
		Slot:         string(slot),
		TypeLabel:    remoteAddrSlotLabel(slot),
		Value:        *value,
	}))
}

func (h *Handler) accountRemoteAddrDelete(w http.ResponseWriter, r *http.Request) {
	sess, ok := h.accountSessionFromRequest(r)
	if !ok {
		http.Redirect(w, r, accountBasePath+"/login", http.StatusSeeOther)
		return
	}
	slot, ok := parseConnectionAddressSlot(chi.URLParam(r, "slot"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := h.mutateAccountRemoteAddrs(r.Context(), sess.AccountID, func(addrs *sqlcgen.GetPlayerAccountRemoteAddrsRow) error {
		applyRemoteAddrSlot(addrs, slot, nil)
		return nil
	}); err != nil {
		webutil.InternalError(w, "account remote-addr delete", err)
		return
	}
	h.redirectRemoteAddrs(w, r, "Connection address removed.", "")
}

func remoteAddrScopeLabel(a remoteaddr.Address) string {
	switch {
	case a.Type == remoteaddr.TypeIP && a.Scope == remoteaddr.ScopeLAN:
		return "LAN only"
	case a.Type == remoteaddr.TypeIP:
		return "Public"
	case a.Scope == remoteaddr.ScopeLAN:
		return "LAN"
	}
	return ""
}

func (h *Handler) redirectRemoteAddrs(w http.ResponseWriter, r *http.Request, flash, flashErr string) {
	target := accountBasePath + "/remote-addrs"
	switch {
	case flash != "":
		target += "?flash=" + url.QueryEscape(flash)
	case flashErr != "":
		target += "?error=" + url.QueryEscape(flashErr)
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// --- mutating handlers -----------------------------------------------------

func (h *Handler) accountSignup(w http.ResponseWriter, r *http.Request) {
	if !webutil.ParseForm(w, r) {
		return
	}
	email := strings.ToLower(strings.TrimSpace(r.Form.Get("email")))
	password := r.Form.Get("password")
	displayName := strings.TrimSpace(r.Form.Get("display_name"))
	view := AccountSignupView{Email: email, DisplayName: displayName, CSRFToken: h.csrf(r)}
	if !validEmail(email) {
		view.FieldErrors = map[string]string{"email": "Enter a valid email."}
		w.WriteHeader(http.StatusUnprocessableEntity)
		webutil.Render(r, w, AccountSignupPage(view))
		return
	}
	if !validPlayerPassword(password) {
		view.FieldErrors = map[string]string{"password": "Password must be between 8 and 72 characters."}
		w.WriteHeader(http.StatusUnprocessableEntity)
		webutil.Render(r, w, AccountSignupPage(view))
		return
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcryptCost)
	if err != nil {
		webutil.InternalError(w, "account signup: bcrypt", err)
		return
	}
	code, err := verifycode.GenerateCode()
	if err != nil {
		webutil.InternalError(w, "account signup: code", err)
		return
	}
	salt, err := verifycode.NewSalt()
	if err != nil {
		webutil.InternalError(w, "account signup: salt", err)
		return
	}
	var displayPtr *string
	if displayName != "" {
		displayPtr = &displayName
	}

	codeHash := verifycode.Hash(salt, code)
	var accountID pgtype.UUID
	err = h.pool.BootstrapQ(r.Context(), func(tx pgx.Tx) error {
		row, qerr := sqlcgen.New(tx).CreatePlayerAccount(r.Context(), sqlcgen.CreatePlayerAccountParams{
			Email:        email,
			PasswordHash: hash,
			DisplayName:  displayPtr,
			CodeHash:     codeHash,
			CodeSalt:     salt,
			ExpiresAt:    pgtype.Timestamptz{Time: h.now().Add(verifycode.CodeTTL), Valid: true},
		})
		if qerr != nil {
			return qerr
		}
		accountID = row.ID
		return nil
	})
	if err != nil {
		if webutil.IsUniqueViolation(err) {
			// Anti-enumeration: a duplicate email must be indistinguishable from a
			// fresh signup, or the form leaks which emails have an account. Notify
			// the real owner out of band and drop the caller into the same verify
			// flow with a decoy id — every downstream code path (verify, resend)
			// treats the unknown account as a wrong code / silent no-op.
			if sendErr := h.notifyDuplicateAccountSignup(r.Context(), email); sendErr != nil {
				slog.ErrorContext(r.Context(), "player account duplicate signup notification delivery", "err", sendErr)
				accountVerificationUnavailable(w)
				return
			}
			h.setAccountVerifyCookie(w, uuid.New(), email)
			http.Redirect(w, r, accountBasePath+"/verify", http.StatusSeeOther)
			return
		}
		webutil.InternalError(w, "account signup: insert", err)
		return
	}
	h.metrics.Signup(observability.SignupAccount)

	if sendErr := h.sendAccountVerifyEmail(r.Context(), email, code); sendErr != nil {
		emptyState := sqlcgen.GetPlayerAccountVerificationStateRow{ID: accountID}
		restoreErr := h.restorePlayerAccountVerification(r.Context(), emptyState, codeHash)
		if restoreErr != nil {
			sendErr = errors.Join(sendErr, fmt.Errorf("restore verification state: %w", restoreErr))
		}
		slog.ErrorContext(r.Context(), "player account signup verification delivery", "err", sendErr)
		accountVerificationUnavailable(w)
		return
	}
	h.setAccountVerifyCookie(w, fromPgUUID(accountID), email)
	http.Redirect(w, r, accountBasePath+"/verify", http.StatusSeeOther)
}

func (h *Handler) accountLogin(w http.ResponseWriter, r *http.Request) {
	if !webutil.ParseForm(w, r) {
		return
	}
	email := strings.ToLower(strings.TrimSpace(r.Form.Get("email")))
	password := r.Form.Get("password")
	if !validPlayerPassword(password) {
		_ = bcrypt.CompareHashAndPassword(dummyPlayerBcryptHash, []byte(password))
		h.renderAccountLoginError(w, r, email)
		return
	}

	var row sqlcgen.GetPlayerAccountByEmailRow
	err := h.pool.BootstrapQ(r.Context(), func(tx pgx.Tx) error {
		var qerr error
		row, qerr = sqlcgen.New(tx).GetPlayerAccountByEmail(r.Context(), email)
		return qerr
	})
	if errors.Is(err, pgx.ErrNoRows) {
		_ = bcrypt.CompareHashAndPassword(dummyPlayerBcryptHash, []byte(password))
		h.renderAccountLoginError(w, r, email)
		return
	}
	if err != nil {
		webutil.InternalError(w, "account login: lookup", err)
		return
	}
	if bcrypt.CompareHashAndPassword(row.PasswordHash, []byte(password)) != nil {
		h.renderAccountLoginError(w, r, email)
		return
	}
	if row.DisabledAt.Valid {
		h.metrics.Login(observability.SurfacePlayer, observability.LoginLocked)
		w.WriteHeader(http.StatusForbidden)
		webutil.Render(r, w, AccountLoginPage(AccountLoginView{Email: email, Error: "This account has been disabled.", CSRFToken: h.csrf(r)}))
		return
	}
	if !row.EmailVerifiedAt.Valid {
		// Same contract as the control panel / per-project login: forward to the
		// verify screen with a fresh code. Cooldown and lockout are not
		// 500s — the verify screen surfaces the lockout on submit.
		if verr := h.startAccountVerification(r.Context(), row.ID, email); verr != nil &&
			!errors.Is(verr, errVerifyAccountLocked) && !errors.Is(verr, errAccountVerifyResendTooSoon) {
			if errors.Is(verr, errAccountVerificationDelivery) {
				slog.ErrorContext(r.Context(), "player account login verification delivery", "err", verr)
				accountVerificationUnavailable(w)
				return
			}
			webutil.InternalError(w, "account login: verification email", verr)
			return
		}
		h.metrics.Login(observability.SurfacePlayer, observability.LoginUnverified)
		h.setAccountVerifyCookie(w, fromPgUUID(row.ID), email)
		http.Redirect(w, r, accountBasePath+"/verify", http.StatusSeeOther)
		return
	}
	h.finishAccountLogin(w, r, row.ID, email, row.SessionEpoch)
}

func (h *Handler) renderAccountLoginError(w http.ResponseWriter, r *http.Request, email string) {
	h.metrics.Login(observability.SurfacePlayer, observability.LoginInvalid)
	w.WriteHeader(http.StatusUnauthorized)
	webutil.Render(r, w, AccountLoginPage(AccountLoginView{Email: email, Error: "Invalid email or password.", CSRFToken: h.csrf(r)}))
}

func (h *Handler) accountVerify(w http.ResponseWriter, r *http.Request) {
	if !webutil.ParseForm(w, r) {
		return
	}
	p, ok := h.accountVerifyCookie(r)
	if !ok {
		http.Redirect(w, r, accountBasePath+"/login", http.StatusSeeOther)
		return
	}
	code := strings.TrimSpace(r.Form.Get("code"))
	view := AccountVerifyView{Email: p.Email, CSRFToken: h.csrf(r)}
	if len(code) != 6 {
		view.Error = "Enter the 6-digit code from your email."
		w.WriteHeader(http.StatusUnprocessableEntity)
		webutil.Render(r, w, AccountVerifyPage(view))
		return
	}
	err := h.confirmAccountCode(r.Context(), toPgUUID(p.AccountID), code)
	switch {
	case errors.Is(err, errBadVerifyCode), errors.Is(err, errVerifyExpired):
		h.metrics.Verification(observability.VerifyInvalid)
	case errors.Is(err, errVerifyLocked), errors.Is(err, errVerifyAccountLocked):
		h.metrics.Verification(observability.VerifyThrottled)
	case err == nil:
		h.metrics.Verification(observability.VerifyOK)
	}
	switch {
	case errors.Is(err, errAlreadyVerified):
		h.clearAccountVerifyCookie(w)
		http.Redirect(w, r, accountBasePath+"/login", http.StatusSeeOther)
		return
	case errors.Is(err, errBadVerifyCode):
		view.Error = "That code is incorrect. Try again."
		w.WriteHeader(http.StatusUnauthorized)
		webutil.Render(r, w, AccountVerifyPage(view))
		return
	case errors.Is(err, errVerifyExpired):
		view.Error = "That code has expired. Request a new one."
		w.WriteHeader(http.StatusGone)
		webutil.Render(r, w, AccountVerifyPage(view))
		return
	case errors.Is(err, errVerifyLocked), errors.Is(err, errVerifyAccountLocked):
		view.Error = "Too many attempts. Request a new code and try again shortly."
		w.WriteHeader(http.StatusTooManyRequests)
		webutil.Render(r, w, AccountVerifyPage(view))
		return
	case err != nil:
		webutil.InternalError(w, "account verify", err)
		return
	}
	h.clearAccountVerifyCookie(w)
	// Fetch the (now verified) account for its current epoch before issuing.
	var epoch int32
	if lookupErr := h.pool.BootstrapQ(r.Context(), func(tx pgx.Tx) error {
		acc, qerr := sqlcgen.New(tx).GetPlayerAccountByID(r.Context(), toPgUUID(p.AccountID))
		if qerr != nil {
			return qerr
		}
		epoch = acc.SessionEpoch
		return nil
	}); lookupErr != nil {
		webutil.InternalError(w, "account verify: reload", lookupErr)
		return
	}
	// Finish through the shared gate so a 2FA-enabled account is still
	// challenged after email verification.
	h.finishAccountLogin(w, r, toPgUUID(p.AccountID), p.Email, epoch)
}

func (h *Handler) accountVerifyResend(w http.ResponseWriter, r *http.Request) {
	if !webutil.ParseForm(w, r) {
		return
	}
	p, ok := h.accountVerifyCookie(r)
	if !ok {
		http.Redirect(w, r, accountBasePath+"/login", http.StatusSeeOther)
		return
	}
	err := h.startAccountVerification(r.Context(), toPgUUID(p.AccountID), p.Email)
	switch {
	case err == nil,
		errors.Is(err, errVerifyAccountLocked),
		errors.Is(err, errAccountVerifyResendTooSoon):
		http.Redirect(w, r, accountBasePath+"/verify", http.StatusSeeOther)
	case errors.Is(err, errAccountVerificationDelivery):
		slog.ErrorContext(r.Context(), "player account verification resend delivery", "err", err)
		accountVerificationUnavailable(w)
	default:
		webutil.InternalError(w, "account verify resend", err)
	}
}

func (h *Handler) accountLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(accountSessionCookieName); err == nil {
		hash := sha256.Sum256([]byte(c.Value))
		_ = h.pool.BootstrapQ(r.Context(), func(tx pgx.Tx) error {
			return sqlcgen.New(tx).RevokePlayerAccountSession(r.Context(), hash[:])
		})
		h.metrics.PlayerSessionClosed()
	}
	h.clearAccountSessionCookie(w)
	http.Redirect(w, r, accountBasePath+"/login", http.StatusSeeOther)
}

// --- verification helpers (mirror players/control-panel) -----------------------

func (h *Handler) startAccountVerification(ctx context.Context, accountID pgtype.UUID, email string) error {
	var state sqlcgen.GetPlayerAccountVerificationStateRow
	if err := h.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		var qerr error
		state, qerr = sqlcgen.New(tx).GetPlayerAccountVerificationState(ctx, accountID)
		return qerr
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // decoy / unknown account: silent no-op, send no email
		}
		return err
	}
	if state.EmailVerificationLockedUntil.Valid && verifycode.AccountLocked(state.EmailVerificationLockedUntil.Time, h.now()) {
		return errVerifyAccountLocked
	}
	if !verifycode.CanResend(state.EmailVerificationLastSentAt.Time, h.now()) {
		return errAccountVerifyResendTooSoon
	}
	code, err := verifycode.GenerateCode()
	if err != nil {
		return err
	}
	salt, err := verifycode.NewSalt()
	if err != nil {
		return err
	}
	codeHash := verifycode.Hash(salt, code)
	if err := h.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		return sqlcgen.New(tx).SetPlayerAccountVerificationCode(ctx, sqlcgen.SetPlayerAccountVerificationCodeParams{
			ID:        accountID,
			CodeHash:  codeHash,
			CodeSalt:  salt,
			ExpiresAt: pgtype.Timestamptz{Time: h.now().Add(verifycode.CodeTTL), Valid: true},
		})
	}); err != nil {
		return err
	}
	if sendErr := h.sendAccountVerifyEmail(ctx, email, code); sendErr != nil {
		restoreErr := h.restorePlayerAccountVerification(ctx, state, codeHash)
		if restoreErr != nil {
			return errors.Join(sendErr, fmt.Errorf("restore verification state: %w", restoreErr))
		}
		return sendErr
	}
	return nil
}

func (h *Handler) restorePlayerAccountVerification(ctx context.Context, state sqlcgen.GetPlayerAccountVerificationStateRow, expectedCodeHash []byte) error {
	return h.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		return sqlcgen.New(tx).RestorePlayerAccountVerificationCode(ctx, sqlcgen.RestorePlayerAccountVerificationCodeParams{
			PreviousCodeHash:   state.EmailVerificationCodeHash,
			PreviousCodeSalt:   state.EmailVerificationSalt,
			PreviousExpiresAt:  state.EmailVerificationExpiresAt,
			PreviousAttempts:   state.EmailVerificationAttempts,
			PreviousLastSentAt: state.EmailVerificationLastSentAt,
			ID:                 state.ID,
			ExpectedCodeHash:   expectedCodeHash,
		})
	})
}

func (h *Handler) confirmAccountCode(ctx context.Context, accountID pgtype.UUID, code string) error {
	var locked bool
	err := h.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		state, err := q.GetPlayerAccountVerificationState(ctx, accountID)
		if errors.Is(err, pgx.ErrNoRows) {
			// Decoy / unknown account (anti-enumeration signup): behave exactly
			// like a wrong code.
			return errBadVerifyCode
		}
		if err != nil {
			return err
		}
		if state.EmailVerifiedAt.Valid {
			return errAlreadyVerified
		}
		if state.EmailVerificationLockedUntil.Valid && verifycode.AccountLocked(state.EmailVerificationLockedUntil.Time, h.now()) {
			return errVerifyAccountLocked
		}
		if verifycode.Expired(state.EmailVerificationExpiresAt.Time, h.now()) {
			return errVerifyExpired
		}
		if len(state.EmailVerificationSalt) == 0 || len(state.EmailVerificationCodeHash) == 0 {
			return errVerifyExpired
		}
		reserved, rerr := q.ReservePlayerAccountVerifyAttempt(ctx, sqlcgen.ReservePlayerAccountVerifyAttemptParams{
			ID:          accountID,
			MaxAttempts: int32(verifycode.MaxAttempts),
		})
		if rerr != nil {
			if errors.Is(rerr, pgx.ErrNoRows) {
				return errVerifyLocked
			}
			return rerr
		}
		if verifycode.LifetimeExhausted(int(reserved.EmailVerificationLifetimeAttempts)) {
			if lerr := q.LockPlayerAccountVerification(ctx, sqlcgen.LockPlayerAccountVerificationParams{
				ID:          accountID,
				LockedUntil: pgtype.Timestamptz{Time: h.now().Add(verifycode.LockoutDuration), Valid: true},
			}); lerr != nil {
				return lerr
			}
			locked = true
			return nil
		}
		expected := verifycode.Hash(state.EmailVerificationSalt, code)
		if subtle.ConstantTimeCompare(expected, state.EmailVerificationCodeHash) != 1 {
			return errBadVerifyCode
		}
		return q.MarkPlayerAccountVerified(ctx, accountID)
	})
	if err != nil {
		return err
	}
	if locked {
		return errVerifyAccountLocked
	}
	return nil
}

func (h *Handler) sendAccountVerifyEmail(ctx context.Context, email, code string) error {
	if h.mailer == nil || h.mailFrom == "" {
		return nil
	}
	if err := h.mailer.Send(ctx, mailer.Message{
		From:    h.mailFrom,
		To:      []string{email},
		Subject: verifySubject,
		Body:    fmt.Sprintf("Your ggscale verification code is %s (valid 15 minutes).", code),
	}); err != nil {
		return fmt.Errorf("%w: %v", errAccountVerificationDelivery, err)
	}
	return nil
}

// sendAccountExistsEmail notifies the owner of an existing account that someone
// tried to sign up with their email. Paired with the decoy verify flow so the
// signup form can't be used to enumerate registered emails.
func (h *Handler) sendAccountExistsEmail(ctx context.Context, email string) error {
	if h.mailer == nil || h.mailFrom == "" {
		return nil
	}
	if err := h.mailer.Send(ctx, mailer.Message{
		From:    h.mailFrom,
		To:      []string{email},
		Subject: "ggscale account",
		Body:    "Someone tried to sign up with this email, but an account already exists. If this was you, log in or reset your password instead.",
	}); err != nil {
		return fmt.Errorf("%w: %v", errAccountVerificationDelivery, err)
	}
	return nil
}

func (h *Handler) notifyDuplicateAccountSignup(ctx context.Context, email string) error {
	var account sqlcgen.GetPlayerAccountByEmailRow
	if err := h.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		var qerr error
		account, qerr = sqlcgen.New(tx).GetPlayerAccountByEmail(ctx, email)
		return qerr
	}); err != nil {
		return fmt.Errorf("lookup duplicate player account: %w", err)
	}
	if account.EmailVerifiedAt.Valid {
		return h.sendAccountExistsEmail(ctx, email)
	}
	err := h.startAccountVerification(ctx, account.ID, email)
	if errors.Is(err, errVerifyAccountLocked) || errors.Is(err, errAccountVerifyResendTooSoon) {
		return h.sendAccountExistsEmail(ctx, email)
	}
	return err
}

func accountVerificationUnavailable(w http.ResponseWriter) {
	http.Error(w, "verification email is temporarily unavailable; try again", http.StatusServiceUnavailable)
}

// --- account sessions ------------------------------------------------------

func (h *Handler) issueAccountSession(ctx context.Context, w http.ResponseWriter, accountID pgtype.UUID, epoch int32) error {
	refreshToken, err := webutil.RandomHex("", 32)
	if err != nil {
		return err
	}
	hash := sha256.Sum256([]byte(refreshToken))
	expires := h.now().Add(sessionTTL)
	if err := h.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		_, qerr := sqlcgen.New(tx).CreatePlayerAccountSession(ctx, sqlcgen.CreatePlayerAccountSessionParams{
			PlayerAccountID: accountID,
			RefreshHash:     hash[:],
			SessionEpoch:    epoch,
			ExpiresAt:       pgtype.Timestamptz{Time: expires, Valid: true},
		})
		return qerr
	}); err != nil {
		return err
	}
	h.metrics.PlayerSessionOpened()
	http.SetCookie(w, &http.Cookie{
		Name:     accountSessionCookieName,
		Value:    refreshToken,
		Path:     "/v1/players",
		Expires:  expires,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   h.cfg.CookieSecure,
	})
	return nil
}

func (h *Handler) accountSessionFromRequest(r *http.Request) (accountSession, bool) {
	c, err := r.Cookie(accountSessionCookieName)
	if err != nil {
		return accountSession{}, false
	}
	hash := sha256.Sum256([]byte(c.Value))
	var row sqlcgen.GetPlayerAccountSessionRow
	if err := h.pool.BootstrapQ(r.Context(), func(tx pgx.Tx) error {
		var qerr error
		row, qerr = sqlcgen.New(tx).GetPlayerAccountSession(r.Context(), hash[:])
		return qerr
	}); err != nil {
		return accountSession{}, false
	}
	// Reject revoked, expired, disabled, or epoch-stale sessions. The epoch
	// check is the account-level revocation lever: a password change / disable
	// bumps player_accounts.session_epoch past the session's snapshot.
	if row.RevokedAt.Valid || row.ExpiresAt.Time.Before(h.now()) || row.DisabledAt.Valid {
		return accountSession{}, false
	}
	if row.SnapshotEpoch != row.AccountEpoch {
		return accountSession{}, false
	}
	out := accountSession{AccountID: fromPgUUID(row.PlayerAccountID), Email: row.Email}
	if row.DisplayName != nil {
		out.DisplayName = *row.DisplayName
	}
	return out, true
}

func (h *Handler) clearAccountSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     accountSessionCookieName,
		Value:    "",
		Path:     "/v1/players",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   h.cfg.CookieSecure,
	})
}

// listAccountLinkedProjects reads the SECURITY DEFINER helper via raw SQL
// (sqlc can't resolve the table-function's columns). Same pattern as
// project_player_tenant in issueSession.
func (h *Handler) listAccountLinkedProjects(ctx context.Context, accountID uuid.UUID) ([]LinkedProject, error) {
	var out []LinkedProject
	err := h.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		rows, qerr := tx.Query(ctx,
			`SELECT tenant_id, project_id, project_name, external_id
			 FROM player_account_linked_projects($1)`, toPgUUID(accountID))
		if qerr != nil {
			return qerr
		}
		defer rows.Close()
		for rows.Next() {
			var lp LinkedProject
			if scanErr := rows.Scan(&lp.TenantID, &lp.ProjectID, &lp.ProjectName, &lp.ExternalID); scanErr != nil {
				return scanErr
			}
			out = append(out, lp)
		}
		return rows.Err()
	})
	return out, err
}

// --- verify-pending cookie (account variant) -------------------------------

type accountVerifyPayload struct {
	AccountID uuid.UUID
	Email     string
	ExpiresAt int64
}

func (h *Handler) setAccountVerifyCookie(w http.ResponseWriter, accountID uuid.UUID, email string) {
	expiresAt := h.now().Add(playerVerifyTTL).Unix()
	val := encodeVerifyCookie(verifyCookiePayload{
		AccountID: accountID.String(),
		ExpiresAt: expiresAt,
		Email:     email,
	}, h.verifySigningKey)
	http.SetCookie(w, &http.Cookie{
		Name:     accountVerifyCookieName,
		Value:    val,
		Path:     accountBasePath,
		MaxAge:   int(playerVerifyTTL.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   h.cfg.CookieSecure,
	})
}

func (h *Handler) clearAccountVerifyCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     accountVerifyCookieName,
		Value:    "",
		Path:     accountBasePath,
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   h.cfg.CookieSecure,
	})
}

func (h *Handler) accountVerifyCookie(r *http.Request) (accountVerifyPayload, bool) {
	c, err := r.Cookie(accountVerifyCookieName)
	if err != nil {
		return accountVerifyPayload{}, false
	}
	p, ok := decodeVerifyCookie(c.Value, h.verifySigningKey)
	if !ok {
		return accountVerifyPayload{}, false
	}
	if p.ExpiresAt > 0 && h.now().Unix() > p.ExpiresAt {
		return accountVerifyPayload{}, false
	}
	id, err := uuid.Parse(p.AccountID)
	if err != nil {
		return accountVerifyPayload{}, false
	}
	return accountVerifyPayload{AccountID: id, Email: p.Email, ExpiresAt: p.ExpiresAt}, true
}
