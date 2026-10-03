// Package webauth is the server's own login. Signing in is always required
// — there is no "every route is open" mode — either with OpenID Connect
// (point it at whatever provider you run or trust) or, if none is
// configured, with a recovery code alone. See New and Config.Enabled.
package webauth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/voidgrid/voidgrid-backup/internal/pki"
)

// Config is the server's OIDC settings. An empty Issuer just means OIDC
// itself is off (Enabled reports false) — signing in is still required,
// by a recovery code instead; see the setup wizard this package's caller
// builds on top of it (the server package's /setup).
type Config struct {
	Issuer       string `json:"issuer"`
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	// RedirectURL is the full callback URL as the provider sees it, e.g.
	// https://backup.example.com/auth/callback. It must exactly match what
	// the provider has on file for this client.
	RedirectURL string `json:"redirect_url"`
	// AllowedEmails, if non-empty, is the only set of identities permitted
	// to sign in; anything else is rejected after a successful OIDC login.
	// Empty means any identity the provider vouches for is let in - fine
	// for a single-operator homelab, worth setting once more than one
	// person could reach this provider.
	AllowedEmails []string `json:"allowed_emails,omitempty"`
}

func (c Config) Enabled() bool { return c.Issuer != "" }

// sessionCookie, oauthState and oauthVerifier name the cookies this package
// sets. oauthState/oauthVerifier are short-lived, scoped to /auth, and
// removed once the callback consumes them.
const (
	sessionCookie   = "vb_session"
	oauthState      = "vb_oauth_state"
	oauthVerifier   = "vb_oauth_verifier"
	oauthNonce      = "vb_oauth_nonce"
	oauthReturnTo   = "vb_oauth_return"
	sessionMaxAge   = 30 * 24 * time.Hour
	transientMaxAge = 5 * time.Minute
)

// Authenticator protects the server's routes: always with a session
// requirement, optionally with OIDC as the way to obtain one.
type Authenticator struct {
	oidcEnabled bool
	cfg         Config
	provider    *oidc.Provider
	verifier    *oidc.IDTokenVerifier
	oauth       oauth2.Config
	allowed     map[string]bool // lowercased emails; nil means allow any
	key         []byte          // HMAC key for session/transient cookies
}

// New returns an Authenticator ready to protect routes. key signs session
// cookies; use LoadOrCreateSessionKey to persist one across restarts. If
// cfg.Enabled() (an issuer is set), New also performs OIDC discovery
// against it, and OIDCEnabled reports true on the result; otherwise the
// Authenticator still works, just without the OIDC endpoints -- a
// recovery code (the server package's job, not this package's) is then
// the only way to sign in.
func New(ctx context.Context, cfg Config, key []byte) (*Authenticator, error) {
	if len(key) < 32 {
		return nil, errors.New("webauth: session key must be at least 32 bytes")
	}
	a := &Authenticator{cfg: cfg, key: key}
	if !cfg.Enabled() {
		return a, nil
	}
	if cfg.ClientID == "" || cfg.ClientSecret == "" || cfg.RedirectURL == "" {
		return nil, errors.New("webauth: issuer, client ID, client secret and redirect URL are all required")
	}
	provider, err := oidc.NewProvider(ctx, cfg.Issuer)
	if err != nil {
		return nil, fmt.Errorf("webauth: discover %s: %w", cfg.Issuer, err)
	}
	var allowed map[string]bool
	if len(cfg.AllowedEmails) > 0 {
		allowed = make(map[string]bool, len(cfg.AllowedEmails))
		for _, e := range cfg.AllowedEmails {
			if e = strings.ToLower(strings.TrimSpace(e)); e != "" {
				allowed[e] = true
			}
		}
	}
	a.oidcEnabled = true
	a.provider = provider
	a.verifier = provider.Verifier(&oidc.Config{ClientID: cfg.ClientID})
	a.oauth = oauth2.Config{
		ClientID:     cfg.ClientID,
		ClientSecret: cfg.ClientSecret,
		RedirectURL:  cfg.RedirectURL,
		Endpoint:     provider.Endpoint(),
		Scopes:       []string{oidc.ScopeOpenID, "email", "profile"},
	}
	a.allowed = allowed
	return a, nil
}

// OIDCEnabled reports whether an OIDC provider is configured, i.e. whether
// there's a real SSO option to offer next to "use a recovery code".
func (a *Authenticator) OIDCEnabled() bool { return a.oidcEnabled }

// Issuer returns the configured OIDC issuer URL, or "" if OIDCEnabled is
// false.
func (a *Authenticator) Issuer() string { return a.cfg.Issuer }

// StartOIDC begins the OIDC flow (redirects to the provider). 404s if OIDC
// isn't configured on this Authenticator -- the server package registers
// this unconditionally at /auth/start and dispatches to whichever
// Authenticator is currently installed, so that turning OIDC on later
// (the setup wizard, or the authentication settings page) doesn't need a
// new route added to work.
func (a *Authenticator) StartOIDC(w http.ResponseWriter, r *http.Request) {
	if !a.oidcEnabled {
		http.NotFound(w, r)
		return
	}
	a.handleLogin(w, r)
}

// CompleteOIDC finishes the OIDC flow (the provider's redirect back). 404s
// if OIDC isn't configured on this Authenticator.
func (a *Authenticator) CompleteOIDC(w http.ResponseWriter, r *http.Request) {
	if !a.oidcEnabled {
		http.NotFound(w, r)
		return
	}
	a.handleCallback(w, r)
}

// Logout ends the current session, however it was obtained (OIDC or a
// recovery code).
func (a *Authenticator) Logout(w http.ResponseWriter, r *http.Request) {
	a.handleLogout(w, r)
}

// Middleware requires a valid session for everything except /healthz,
// /favicon.ico, /setup and /auth/*, redirecting browsers to sign in and
// refusing everything else (the JSON API included) with 401. /setup is
// exempted unconditionally here; the server package's own handler enforces
// whether it's actually reachable (it isn't, once setup is done and the
// visitor has no session). /favicon.ico is exempted because a browser
// requests it automatically alongside the page you actually asked for --
// left unexempted, that request (unauthenticated, same as the real one)
// races it to set the "return here after signing in" cookie, and can win,
// landing you on /favicon.ico instead of the page you meant to reach.
func (a *Authenticator) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" || r.URL.Path == "/favicon.ico" || r.URL.Path == "/setup" || strings.HasPrefix(r.URL.Path, "/auth/") {
			next.ServeHTTP(w, r)
			return
		}
		if _, ok := a.CurrentUser(r); ok {
			next.ServeHTTP(w, r)
			return
		}
		if wantsJSON(r) {
			http.Error(w, "not signed in", http.StatusUnauthorized)
			return
		}
		a.setCookie(w, r, oauthReturnTo, r.URL.RequestURI(), transientMaxAge)
		http.Redirect(w, r, "/auth/login", http.StatusSeeOther)
	})
}

func wantsJSON(r *http.Request) bool {
	return strings.HasPrefix(r.URL.Path, "/api/")
}

// IssueSession starts a session for identity, exactly as a successful OIDC
// login would. It's exported so another sign-in path this package doesn't
// know about (a recovery code, say) can grant the same kind of session.
func (a *Authenticator) IssueSession(w http.ResponseWriter, r *http.Request, identity string) {
	a.setCookie(w, r, sessionCookie, sessionToken(a.key, identity), sessionMaxAge)
}

// CurrentUser returns the signed-in identity, if any.
func (a *Authenticator) CurrentUser(r *http.Request) (email string, ok bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return "", false
	}
	payload, ok := verifyToken(a.key, c.Value)
	if !ok {
		return "", false
	}
	email, expiry, ok := strings.Cut(payload, "|")
	if !ok {
		return "", false
	}
	exp, err := strconv.ParseInt(expiry, 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return "", false
	}
	return email, true
}

func (a *Authenticator) handleLogin(w http.ResponseWriter, r *http.Request) {
	state, err := randomToken()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	verifier := oauth2.GenerateVerifier()
	nonce, err := randomToken()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	a.setCookie(w, r, oauthState, state, transientMaxAge)
	a.setCookie(w, r, oauthVerifier, verifier, transientMaxAge)
	a.setCookie(w, r, oauthNonce, nonce, transientMaxAge)
	url := a.oauth.AuthCodeURL(state, oauth2.S256ChallengeOption(verifier), oidc.Nonce(nonce))
	http.Redirect(w, r, url, http.StatusSeeOther)
}

func (a *Authenticator) handleCallback(w http.ResponseWriter, r *http.Request) {
	fail := func(status int, msg string, err error) {
		if err != nil {
			msg = fmt.Sprintf("%s: %v", msg, err)
		}
		http.Error(w, msg, status)
	}

	stateCookie, err1 := r.Cookie(oauthState)
	verifierCookie, err2 := r.Cookie(oauthVerifier)
	nonceCookie, err3 := r.Cookie(oauthNonce)
	if err1 != nil || err2 != nil || err3 != nil {
		fail(http.StatusBadRequest, "login expired; try again", nil)
		return
	}
	a.clearCookie(w, oauthState)
	a.clearCookie(w, oauthVerifier)
	a.clearCookie(w, oauthNonce)

	if subtle.ConstantTimeCompare([]byte(r.URL.Query().Get("state")), []byte(stateCookie.Value)) != 1 {
		fail(http.StatusBadRequest, "state mismatch", nil)
		return
	}
	if errMsg := r.URL.Query().Get("error"); errMsg != "" {
		fail(http.StatusForbidden, "provider refused: "+errMsg, nil)
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		fail(http.StatusBadRequest, "no authorization code", nil)
		return
	}

	ctx := r.Context()
	token, err := a.oauth.Exchange(ctx, code, oauth2.VerifierOption(verifierCookie.Value))
	if err != nil {
		fail(http.StatusBadGateway, "token exchange failed", err)
		return
	}
	rawIDToken, ok := token.Extra("id_token").(string)
	if !ok || rawIDToken == "" {
		fail(http.StatusBadGateway, "provider did not return an ID token", nil)
		return
	}
	idToken, err := a.verifier.Verify(ctx, rawIDToken)
	if err != nil {
		fail(http.StatusForbidden, "ID token verification failed", err)
		return
	}
	if idToken.Nonce != nonceCookie.Value {
		fail(http.StatusForbidden, "nonce mismatch", nil)
		return
	}
	var claims struct {
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
	}
	if err := idToken.Claims(&claims); err != nil || claims.Email == "" {
		fail(http.StatusBadGateway, "provider did not return an email claim", err)
		return
	}
	if a.allowed != nil && !a.allowed[strings.ToLower(claims.Email)] {
		fail(http.StatusForbidden, "not on the allowed list: "+claims.Email, nil)
		return
	}

	a.IssueSession(w, r, claims.Email)
	returnTo := "/"
	if c, err := r.Cookie(oauthReturnTo); err == nil && strings.HasPrefix(c.Value, "/") && !strings.HasPrefix(c.Value, "//") {
		returnTo = c.Value
	}
	a.clearCookie(w, oauthReturnTo)
	http.Redirect(w, r, returnTo, http.StatusSeeOther)
}

func (a *Authenticator) handleLogout(w http.ResponseWriter, r *http.Request) {
	a.clearCookie(w, sessionCookie)
	http.Redirect(w, r, "/auth/login", http.StatusSeeOther)
}

// secure reports whether cookies should carry the Secure flag: the request
// arrived over TLS, directly or (trusting the operator's own reverse proxy,
// as documented) via X-Forwarded-Proto.
func secure(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func (a *Authenticator) setCookie(w http.ResponseWriter, r *http.Request, name, value string, maxAge time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: value, Path: "/", HttpOnly: true, Secure: secure(r),
		SameSite: http.SameSiteLaxMode, MaxAge: int(maxAge.Seconds()),
	})
}

func (a *Authenticator) clearCookie(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: "/", MaxAge: -1})
}

// --- signed tokens (session cookie payload) ---

func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func sessionToken(key []byte, email string) string {
	payload := email + "|" + strconv.FormatInt(time.Now().Add(sessionMaxAge).Unix(), 10)
	return signToken(key, payload)
}

func signToken(key []byte, payload string) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(payload))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." + sig
}

func verifyToken(key []byte, token string) (payload string, ok bool) {
	encPayload, sig, found := strings.Cut(token, ".")
	if !found {
		return "", false
	}
	raw, err := base64.RawURLEncoding.DecodeString(encPayload)
	if err != nil {
		return "", false
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(raw)
	expected := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if subtle.ConstantTimeCompare([]byte(sig), []byte(expected)) != 1 {
		return "", false
	}
	return string(raw), true
}

// --- session signing key persistence ---

// LoadOrCreateSessionKey loads a 32-byte HMAC key from
// <dir>/session.key, creating one on first use.
func LoadOrCreateSessionKey(dir string) ([]byte, error) {
	path := filepath.Join(dir, "session.key")
	b, err := os.ReadFile(path)
	if err == nil && len(b) == 32 {
		return b, nil
	}
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := pki.WriteFileAtomic(path, key, 0o600); err != nil {
		return nil, err
	}
	return key, nil
}
