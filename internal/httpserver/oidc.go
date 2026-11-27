package httpserver

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/i2bskn/astovia/internal/auth"
	"github.com/i2bskn/astovia/internal/config"
	"github.com/i2bskn/astovia/internal/store"
	"github.com/i2bskn/authlume/oidc"
)

const (
	// oidcLoginPath starts the sign-in. It takes a form POST of this origin.
	oidcLoginPath = "/auth/oidc/login"
	// oidcFormLimit bounds the sign-in form body in bytes.
	oidcFormLimit = 16 << 10
	// maxReturnTo bounds return_to in bytes after decoding, so that the
	// consent screen of an MCP client with a long request fits. It is kept
	// by authlume on the server (oidc.MaxAppData), not in a cookie.
	maxReturnTo = 8300
	// maxScreenReturnTo bounds a return_to to any other screen, whose
	// address is short: anyone can start a sign-in, so the rows of the
	// sign-ins in progress are kept small.
	maxScreenReturnTo = 1024
	// signinErrorPath is the screen that explains a failed sign-in.
	signinErrorPath = "/signin-error"
)

// Timeouts of the OIDC client, as in Kordovia's signin package.
const (
	// oidcInitTimeout bounds one discovery. It does not use the request
	// context, so that a client going away does not count as a failure.
	oidcInitTimeout = 5 * time.Second
	// oidcRetryInterval is the minimum time between two failed discoveries,
	// so that sign-in attempts cannot make many requests to the provider.
	oidcRetryInterval = 30 * time.Second
	// oidcRequestTimeout bounds the start and the callback, discovery
	// included.
	oidcRequestTimeout = 10 * time.Second
)

// Reasons of /signin-error. The screen shows a message for each.
const (
	reasonFailed        = "failed"
	reasonUnavailable   = "unavailable"
	reasonDisabled      = "disabled"
	reasonReturnTooLong = "return_too_long"
	reasonNotAllowed    = "not_allowed"
	reasonNotVerified   = "email_not_verified"
	reasonUnsupported   = "unsupported_account"
	reasonRateLimited   = "rate_limited"
)

// gmailDomain is the only domain whose Google accounts need no hd claim.
const gmailDomain = "gmail.com"

// oidcLogin is the OIDC relying party of the server: authlume's client,
// created on first use instead of at startup, so that the server starts
// while the provider cannot be reached. Until discovery succeeds, sign-ins
// fail with oidc.ErrProviderUnavailable.
type oidcLogin struct {
	cfg   oidc.Config
	store oidc.Store
	now   func() time.Time
	// create is oidc.New; tests replace it.
	create func(context.Context, oidc.Config, oidc.Store) (*oidc.Client, error)

	// mu serializes discoveries and guards client and failedAt.
	mu       sync.Mutex
	client   *oidc.Client
	failedAt time.Time
}

// newOIDCLogin returns the relying party of c, keeping sign-ins in progress
// in st. now is the clock of the retry interval, the server's. It does not
// contact the provider.
func newOIDCLogin(c *config.OIDCClient, st oidc.Store, now func() time.Time) *oidcLogin {
	return &oidcLogin{
		cfg: oidc.Config{
			Issuer:       c.Issuer,
			ClientID:     c.ClientID,
			ClientSecret: c.Secret,
			RedirectURL:  c.RedirectURI,
			// RS256 only. openid is always requested; only sub, email,
			// email_verified and hd are read.
			Scopes: []string{"email"},
		},
		store:  st,
		now:    now,
		create: oidc.New,
	}
}

// begin starts a sign-in that keeps appData and redirects to the
// authorization endpoint with a 303.
func (l *oidcLogin) begin(w http.ResponseWriter, r *http.Request, appData []byte) error {
	ctx, cancel := context.WithTimeout(r.Context(), oidcRequestTimeout)
	defer cancel()
	c, err := l.get()
	if err != nil {
		return err
	}
	return c.BeginWithOptions(w, r.WithContext(ctx), oidc.AuthorizationOptions{AppData: appData})
}

// complete finishes the sign-in at the callback and returns the verified
// identity and the app data given to begin.
func (l *oidcLogin) complete(w http.ResponseWriter, r *http.Request) (oidc.Identity, []byte, error) {
	ctx, cancel := context.WithTimeout(r.Context(), oidcRequestTimeout)
	defer cancel()
	c, err := l.get()
	if err != nil {
		return oidc.Identity{}, nil, err
	}
	return c.CompleteWithAppData(w, r.WithContext(ctx))
}

// get returns the authlume client, discovering the provider when there is
// none yet and the last failure is at least oidcRetryInterval ago.
func (l *oidcLogin) get() (*oidc.Client, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.client != nil {
		return l.client, nil
	}
	if !l.failedAt.IsZero() && l.now().Sub(l.failedAt) < oidcRetryInterval {
		return nil, oidc.ErrProviderUnavailable
	}
	ctx, cancel := context.WithTimeout(context.Background(), oidcInitTimeout)
	defer cancel()
	c, err := l.create(ctx, l.cfg, l.store)
	if err != nil {
		// Counted from the end of the attempt, so that a slow failure
		// still waits the whole interval.
		l.failedAt = l.now()
		switch {
		case errors.Is(err, oidc.ErrProviderUnavailable), errors.Is(err, context.DeadlineExceeded):
			log.Print("auth: oidc: discovery failed: provider unavailable")
		default:
			// Invalid configuration or metadata, such as an issuer that
			// differs from the configured one. The errors carry no secret.
			log.Printf("auth: oidc: discovery failed: %v", err)
		}
		return nil, oidc.ErrProviderUnavailable
	}
	l.client = c
	return c, nil
}

// authConfigResponse tells the web UI how to sign in.
type authConfigResponse struct {
	Mode string `json:"mode"`
}

// handleAuthConfig answers the sign-in mode: password or oidc.
func (s *Server) handleAuthConfig(w http.ResponseWriter, _ *http.Request) {
	if s.cfg.OIDC == nil {
		writeJSON(w, 200, authConfigResponse{Mode: config.AuthModePassword})
		return
	}
	writeJSON(w, 200, authConfigResponse{Mode: config.AuthModeOIDC})
}

// passwordAuth rejects with 409 the endpoints of password sign-in, the
// invitation links and the password resets when OpenID Connect signs users
// in. It runs before the rate limits, so that nothing is counted.
func (s *Server) passwordAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.OIDC != nil {
			fail(w, 409, "password_auth_disabled", "Passwords, invitation links and password resets are disabled; this server signs in with OpenID Connect.")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// handleOIDCLogin starts the sign-in from the form of the sign-in screen:
// after the rate limit per peer and the Origin check it asks authlume to
// start a sign-in that keeps return_to on the server, and redirects to the
// authorization endpoint of the provider with a 303.
func (s *Server) handleOIDCLogin(w http.ResponseWriter, r *http.Request) {
	if !s.oidcLimiter.allow(s.clientAddr(r)) {
		redirectSigninError(w, http.StatusSeeOther, reasonRateLimited)
		return
	}
	if !s.sameOrigin(r) {
		fail(w, 403, "origin_rejected", "Request origin is not allowed.")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, oidcFormLimit)
	if err := r.ParseForm(); err != nil {
		fail(w, 400, "invalid_request", "The sign-in form is malformed.")
		return
	}
	returnTo := "/"
	// Several return_to values are ignored.
	if values := r.PostForm["return_to"]; len(values) == 1 {
		// A return_to that cannot be kept is not replaced silently: the
		// consent screen of an MCP client would be lost.
		if len(values[0]) > maxReturnTo || (len(values[0]) > maxScreenReturnTo && !consentReturnTo(values[0])) {
			redirectSigninError(w, http.StatusSeeOther, reasonReturnTooLong)
			return
		}
		if safeReturnTo(values[0]) {
			returnTo = values[0]
		}
	}
	if s.oidc == nil {
		log.Printf("auth: oidc login: no OIDC client")
		redirectSigninError(w, http.StatusSeeOther, reasonUnavailable)
		return
	}
	if err := s.oidc.begin(w, r, []byte(returnTo)); err != nil {
		log.Printf("auth: oidc login: cannot start the sign-in: %s", oidcErrorKind(err))
		redirectSigninError(w, http.StatusSeeOther, reasonUnavailable)
	}
}

// consentReturnTo reports whether v returns to the consent screen of the
// MCP authorization server, whose query may be long.
func consentReturnTo(v string) bool {
	p, _, _ := strings.Cut(v, "?")
	return strings.TrimSuffix(p, "/") == authorizePath
}

// oidcErrorKind names the category of an error of authlume's oidc for the
// log, without its text.
func oidcErrorKind(err error) string {
	switch {
	case errors.Is(err, oidc.ErrInvalid):
		return "invalid"
	case errors.Is(err, oidc.ErrProviderUnavailable):
		return "provider unavailable"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	}
	return "server error"
}

// handleOIDCCallback finishes the sign-in: authlume checks the state, the
// browser binding and the ID token; the user is the one linked to its iss
// and sub (oidcUser links or creates it on the first sign-in). The session
// is issued only while the user is still linked to that identity. Every
// failure redirects to /signin-error with a reason. Only the reasons are
// logged, never the code, the state, the tokens, the cookies, the email
// address, the sub or the claims.
func (s *Server) handleOIDCCallback(w http.ResponseWriter, r *http.Request) {
	failed := func(reason, detail string) {
		log.Printf("auth: oidc sign-in failed: %s", detail)
		redirectSigninError(w, http.StatusSeeOther, reason)
	}
	if s.oidc == nil || s.cfg.OIDC == nil {
		failed(reasonUnavailable, "no OIDC client")
		return
	}
	identity, appData, err := s.oidc.complete(w, r)
	switch {
	case errors.Is(err, oidc.ErrInvalid):
		failed(reasonFailed, "the sign-in could not be verified")
		return
	case err != nil:
		failed(reasonUnavailable, "the sign-in could not be completed: "+oidcErrorKind(err))
		return
	}
	returnTo := string(appData)
	if !safeReturnTo(returnTo) {
		returnTo = "/"
	}
	u, reason, detail := s.oidcUser(r.Context(), identity)
	if reason != "" {
		failed(reason, detail)
		return
	}
	err = s.issueSession(w, r, u.ID, store.IssueGuard{OIDCIssuer: identity.ID.Issuer, OIDCSubject: identity.ID.Subject})
	if errors.Is(err, store.ErrUnauthorized) {
		// Disabled or unlinked since the lookup.
		failed(reasonFailed, "user "+u.ID+" changed during the sign-in")
		return
	}
	if err != nil {
		log.Printf("auth: oidc sign-in: sign in user=%s: %v", u.ID, err)
		failed(reasonUnavailable, "cannot start the session")
		return
	}
	redirect(w, http.StatusSeeOther, returnTo)
}

// emailVerified reports whether the email_verified claim (nil when absent)
// passes: it must be true, or, when require is false, may be absent.
func emailVerified(claim *bool, require bool) bool {
	if claim == nil {
		return !require
	}
	return *claim
}

// oidcUser returns the enabled user of the verified ID token. Otherwise it
// returns the reason for /signin-error and the detail to log. A disabled
// user is never enabled again here.
//
// A user linked to the iss and sub of the token is returned as it is: its
// email address, email_verified, hd and the invitations are not looked at,
// and the stored address is not updated. Without one this is the first
// sign-in of the identity (firstOIDCUser).
func (s *Server) oidcUser(ctx context.Context, identity oidc.Identity) (store.User, string, string) {
	issuer := identity.ID.Issuer
	u, err := s.auth.Store.UserByOIDC(ctx, issuer, identity.ID.Subject)
	switch {
	case err == nil && u.Disabled != 0:
		return store.User{}, reasonDisabled, "user " + u.ID + " is disabled"
	case err == nil:
		return u, "", ""
	case errors.Is(err, store.ErrInvalidOIDCIdentity):
		return store.User{}, reasonFailed, "empty issuer or sub"
	case !errors.Is(err, sql.ErrNoRows):
		log.Printf("auth: oidc sign-in: find user: %v", err)
		return store.User{}, reasonUnavailable, "database error while finding the user"
	}
	return s.firstOIDCUser(ctx, issuer, identity)
}

// firstOIDCUser handles the first sign-in of an identity: the verified
// email address must belong to a user that is not linked yet and that an
// administrator approved for linking, or have an active invitation, and the
// identity is linked to that user or to the one created from the
// invitation. Nothing is created, linked or accepted when a check fails.
func (s *Server) firstOIDCUser(ctx context.Context, issuer string, identity oidc.Identity) (store.User, string, string) {
	claim, _ := identity.Claims.Text("email")
	email, err := auth.Email(claim)
	if err != nil {
		return store.User{}, reasonFailed, "malformed email claim"
	}
	var verified *bool
	if v, ok := identity.Claims.Bool("email_verified"); ok {
		verified = &v
	}
	hd, _ := identity.Claims.Text("hd")
	if !emailVerified(verified, s.cfg.OIDC.RequireEmailVerified) {
		return store.User{}, reasonNotVerified, "email not verified"
	}
	// Google lets anyone make a personal account with an address of a domain
	// they do not hold; only gmail.com addresses and Google Workspace
	// accounts (hd) are taken as verified by Google. This is checked when
	// Google is the issuer, or when the configuration asks for it because
	// Google accounts sign in through another provider.
	if (issuer == config.GoogleIssuer || s.cfg.OIDC.RequireGoogleHD) && email[strings.LastIndexByte(email, '@')+1:] != gmailDomain && hd == "" {
		return store.User{}, reasonUnsupported, "google account without hd"
	}
	id, err := auth.Random()
	if err != nil {
		return store.User{}, reasonUnavailable, "cannot create a user ID"
	}
	newUser := store.User{ID: id, PasswordHash: store.NoPasswordHash, CreatedAt: s.auth.Now().Unix()}
	u, created, err := s.auth.Store.LinkOIDCUser(ctx, issuer, identity.ID.Subject, email, newUser, s.auth.Now().Unix())
	// The errors of LinkOIDCUser are not logged: they may name the address.
	switch {
	case err == nil && created:
		log.Printf("auth: created user %s from an invitation by an oidc sign-in", u.ID)
		return u, "", ""
	case err == nil:
		log.Printf("auth: linked user %s to an oidc identity", u.ID)
		return u, "", ""
	case errors.Is(err, store.ErrNotInvited):
		return store.User{}, reasonNotAllowed, "not invited"
	case errors.Is(err, store.ErrOIDCLinked):
		return store.User{}, reasonNotAllowed, "email belongs to a user linked to another identity"
	case errors.Is(err, store.ErrOIDCNotApproved):
		return store.User{}, reasonNotAllowed, "email belongs to a user not approved for linking"
	case errors.Is(err, store.ErrUserDisabled):
		return store.User{}, reasonDisabled, "user is disabled"
	case errors.Is(err, store.ErrInvalidOIDCIdentity):
		return store.User{}, reasonFailed, "empty issuer or sub"
	case !errors.Is(err, store.ErrConflict):
		return store.User{}, reasonUnavailable, "cannot link or create the user"
	}
	return s.oidcUserAfterConflict(ctx, issuer, identity.ID.Subject)
}

// oidcUserAfterConflict reads the identity once after a concurrent sign-in
// linked or created a user first (store.ErrConflict). Without a user of the
// identity, another identity took the email address.
func (s *Server) oidcUserAfterConflict(ctx context.Context, issuer, subject string) (store.User, string, string) {
	u, err := s.auth.Store.UserByOIDC(ctx, issuer, subject)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return store.User{}, reasonNotAllowed, "email was taken by another identity during the sign-in"
	case err != nil:
		log.Printf("auth: oidc sign-in: find user: %v", err)
		return store.User{}, reasonUnavailable, "database error while finding the user"
	case u.Disabled != 0:
		return store.User{}, reasonDisabled, "user " + u.ID + " is disabled"
	}
	return u, "", ""
}

// safeReturnTo reports whether v is a path and query of a screen on this
// origin to return to after the sign-in. Anything else, including a path to
// another site, is rejected. Only printable ASCII is accepted: the browser
// sends the path and the query percent-encoded.
func safeReturnTo(v string) bool {
	if len(v) > maxReturnTo || !strings.HasPrefix(v, "/") || strings.HasPrefix(v, "//") || strings.HasPrefix(v, "/\\") || strings.Contains(v, "#") {
		return false
	}
	for i := 0; i < len(v); i++ {
		if v[i] <= 0x20 || v[i] >= 0x7f {
			return false
		}
	}
	u, err := url.Parse(v)
	if err != nil || u.Scheme != "" || u.Host != "" || u.User != nil || u.Opaque != "" || u.Fragment != "" {
		return false
	}
	return screenPath(u.Path) && strings.TrimSuffix(u.Path, "/") != signinErrorPath
}

// redirect redirects to location as it is, without a body. http.Redirect is
// not used because it cleans relative paths and writes HTML.
func redirect(w http.ResponseWriter, status int, location string) {
	w.Header().Set("Location", location)
	w.WriteHeader(status)
}

// redirectSigninError redirects to the sign-in error screen with reason.
func redirectSigninError(w http.ResponseWriter, status int, reason string) {
	redirect(w, status, signinErrorPath+"?reason="+url.QueryEscape(reason))
}
