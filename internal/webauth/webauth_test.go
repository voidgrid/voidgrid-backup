package webauth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"

	"github.com/voidgrid/voidgrid-backup/internal/tidy"
)

// fakeProvider is a minimal OIDC provider: discovery, JWKS and a token
// endpoint. It has no /authorize of its own; tests build the authorization
// code themselves (see idTokenFor) since nothing here drives a real browser
// through a consent screen.
type fakeProvider struct {
	baseURL string
	key     *ecdsa.PrivateKey
	jwks    jose.JSONWebKeySet
}

func newFakeProvider(t *testing.T) *fakeProvider {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return &fakeProvider{
		key: key,
		jwks: jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
			Algorithm: "ES256", Use: "sign", KeyID: "test", Key: key.Public(),
		}}},
	}
}

func (p *fakeProvider) mux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		tidy.Check("fake provider: discovery reply", json.NewEncoder(w).Encode(map[string]any{
			"issuer": p.baseURL, "authorization_endpoint": p.baseURL + "/authorize",
			"token_endpoint": p.baseURL + "/token", "jwks_uri": p.baseURL + "/jwks",
			"id_token_signing_alg_values_supported": []string{"ES256"},
		}))
	})
	mux.HandleFunc("GET /jwks", func(w http.ResponseWriter, r *http.Request) {
		tidy.Check("fake provider: keys reply", json.NewEncoder(w).Encode(p.jwks))
	})
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		code := r.FormValue("code")
		if code == "" || r.FormValue("code_verifier") == "" {
			http.Error(w, "missing code or PKCE verifier", http.StatusBadRequest)
			return
		}
		// The test packs the claims it wants issued into the authorization
		// code itself, standing in for whatever the provider would have
		// remembered from a real authorize step.
		claimsJSON, err := base64.RawURLEncoding.DecodeString(code)
		if err != nil {
			http.Error(w, "bad code", http.StatusBadRequest)
			return
		}
		var claims map[string]any
		if err := json.Unmarshal(claimsJSON, &claims); err != nil {
			http.Error(w, "bad code", http.StatusBadRequest)
			return
		}
		payload, err := json.Marshal(claims)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: p.key}, &jose.SignerOptions{
			ExtraHeaders: map[jose.HeaderKey]any{"kid": "test"},
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		jws, err := signer.Sign(payload)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		idToken, err := jws.CompactSerialize()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		tidy.Check("fake provider: token reply", json.NewEncoder(w).Encode(map[string]any{
			"access_token": "test-access-token", "token_type": "Bearer",
			"expires_in": 3600, "id_token": idToken,
		}))
	})
	return mux
}

// idTokenCode builds a fake authorization code that the token endpoint above
// decodes into the ID token claims it will sign and return.
func idTokenCode(t *testing.T, issuer, clientID, nonce, email string, emailVerified bool) string {
	t.Helper()
	now := time.Now()
	claims := map[string]any{
		"iss": issuer, "sub": "user-1", "aud": clientID,
		"exp": now.Add(time.Hour).Unix(), "iat": now.Unix(),
		"nonce": nonce, "email": email, "email_verified": emailVerified,
	}
	b, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

const clientID = "test-client"

func newTestAuth(t *testing.T, allowed ...string) (*Authenticator, *fakeProvider) {
	t.Helper()
	p := newFakeProvider(t)
	srv := httptest.NewServer(p.mux())
	t.Cleanup(srv.Close)
	p.baseURL = srv.URL

	key := make([]byte, 32)
	rand.Read(key)
	a, err := New(context.Background(), Config{
		Issuer: srv.URL, ClientID: clientID, ClientSecret: "test-secret",
		RedirectURL: "https://backup.example/auth/callback", AllowedEmails: allowed,
	}, key)
	if err != nil {
		t.Fatal(err)
	}
	return a, p
}

// startLogin drives handleLogin and returns the cookies it set (state,
// verifier, nonce) so a test can build the matching callback request.
func startLogin(t *testing.T, a *Authenticator) []*http.Cookie {
	t.Helper()
	req := httptest.NewRequest("GET", "/auth/login", nil)
	rec := httptest.NewRecorder()
	a.handleLogin(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("login: status %d", rec.Code)
	}
	if loc := rec.Result().Header.Get("Location"); !strings.Contains(loc, "client_id="+clientID) {
		t.Fatalf("login redirect missing client_id: %s", loc)
	}
	return rec.Result().Cookies()
}

func cookieValue(cookies []*http.Cookie, name string) string {
	for _, c := range cookies {
		if c.Name == name {
			return c.Value
		}
	}
	return ""
}

func callback(a *Authenticator, transient []*http.Cookie, query url.Values) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", "/auth/callback?"+query.Encode(), nil)
	for _, c := range transient {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	a.handleCallback(rec, req)
	return rec
}

func TestLoginCallbackRoundTrip(t *testing.T) {
	a, p := newTestAuth(t)
	transient := startLogin(t, a)
	code := idTokenCode(t, p.baseURL, clientID, cookieValue(transient, oauthNonce), "person@example.com", true)

	rec := callback(a, transient, url.Values{"state": {cookieValue(transient, oauthState)}, "code": {code}})
	if rec.Code != http.StatusSeeOther || rec.Result().Header.Get("Location") != "/" {
		t.Fatalf("callback: status %d location %q body %q", rec.Code, rec.Result().Header.Get("Location"), rec.Body.String())
	}
	session := cookieValue(rec.Result().Cookies(), sessionCookie)
	if session == "" {
		t.Fatal("no session cookie set")
	}

	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: session})
	email, ok := a.CurrentUser(req)
	if !ok || email != "person@example.com" {
		t.Fatalf("CurrentUser: %q %v", email, ok)
	}
}

func TestCallbackReturnsToOriginalPath(t *testing.T) {
	a, p := newTestAuth(t)
	req := httptest.NewRequest("GET", "/auth/login", nil)

	// Two recorders so each is only read via Result() (which httptest
	// memoizes) after all its writes are done.
	returnRec := httptest.NewRecorder()
	a.setCookie(returnRec, req, oauthReturnTo, "/jobs/abc123", transientMaxAge)
	loginRec := httptest.NewRecorder()
	a.handleLogin(loginRec, req)
	transient := append(loginRec.Result().Cookies(), returnRec.Result().Cookies()...)

	code := idTokenCode(t, p.baseURL, clientID, cookieValue(transient, oauthNonce), "person@example.com", true)
	cb := callback(a, transient, url.Values{"state": {cookieValue(transient, oauthState)}, "code": {code}})
	if got := cb.Result().Header.Get("Location"); got != "/jobs/abc123" {
		t.Fatalf("return-to location: %q", got)
	}
}

func TestCallbackRejectsStateMismatch(t *testing.T) {
	a, p := newTestAuth(t)
	transient := startLogin(t, a)
	code := idTokenCode(t, p.baseURL, clientID, cookieValue(transient, oauthNonce), "person@example.com", true)
	rec := callback(a, transient, url.Values{"state": {"not-the-right-state"}, "code": {code}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("state mismatch: status %d", rec.Code)
	}
}

func TestCallbackRejectsNonceMismatch(t *testing.T) {
	a, p := newTestAuth(t)
	transient := startLogin(t, a)
	code := idTokenCode(t, p.baseURL, clientID, "wrong-nonce", "person@example.com", true)
	rec := callback(a, transient, url.Values{"state": {cookieValue(transient, oauthState)}, "code": {code}})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("nonce mismatch: status %d body %q", rec.Code, rec.Body.String())
	}
}

func TestCallbackEnforcesAllowedEmails(t *testing.T) {
	a, p := newTestAuth(t, "ok@example.com")
	transient := startLogin(t, a)
	code := idTokenCode(t, p.baseURL, clientID, cookieValue(transient, oauthNonce), "someone-else@example.com", true)
	rec := callback(a, transient, url.Values{"state": {cookieValue(transient, oauthState)}, "code": {code}})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("disallowed email: status %d", rec.Code)
	}
	if cookieValue(rec.Result().Cookies(), sessionCookie) != "" {
		t.Fatal("session cookie set for a rejected email")
	}
}

func TestCallbackAllowsListedEmailCaseInsensitively(t *testing.T) {
	a, p := newTestAuth(t, "OK@Example.com")
	transient := startLogin(t, a)
	code := idTokenCode(t, p.baseURL, clientID, cookieValue(transient, oauthNonce), "ok@example.com", true)
	rec := callback(a, transient, url.Values{"state": {cookieValue(transient, oauthState)}, "code": {code}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("allowed email rejected: status %d body %q", rec.Code, rec.Body.String())
	}
}

func TestCallbackMissingTransientCookies(t *testing.T) {
	a, _ := newTestAuth(t)
	rec := callback(a, nil, url.Values{"state": {"x"}, "code": {"y"}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing cookies: status %d", rec.Code)
	}
}

func TestSessionTokenTamperDetected(t *testing.T) {
	a, p := newTestAuth(t)
	transient := startLogin(t, a)
	code := idTokenCode(t, p.baseURL, clientID, cookieValue(transient, oauthNonce), "person@example.com", true)
	rec := callback(a, transient, url.Values{"state": {cookieValue(transient, oauthState)}, "code": {code}})
	session := cookieValue(rec.Result().Cookies(), sessionCookie)

	tampered := session[:len(session)-1] + "x"
	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: tampered})
	if _, ok := a.CurrentUser(req); ok {
		t.Fatal("tampered session cookie accepted")
	}
}

func TestSessionTokenExpired(t *testing.T) {
	a, _ := newTestAuth(t)
	expired := signToken(a.key, "person@example.com|"+strconv.FormatInt(time.Now().Add(-time.Hour).Unix(), 10))
	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: expired})
	if _, ok := a.CurrentUser(req); ok {
		t.Fatal("expired session cookie accepted")
	}
}

func TestMiddleware(t *testing.T) {
	a, p := newTestAuth(t)
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTeapot) })
	protected := a.Middleware(inner)

	// Unauthenticated UI request: redirected to login, original path saved.
	req := httptest.NewRequest("GET", "/jobs", nil)
	rec := httptest.NewRecorder()
	protected.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther || rec.Result().Header.Get("Location") != "/auth/login" {
		t.Fatalf("unauthenticated UI request: status %d location %q", rec.Code, rec.Result().Header.Get("Location"))
	}
	if cookieValue(rec.Result().Cookies(), oauthReturnTo) != "/jobs" {
		t.Fatal("return-to path not recorded")
	}

	// Unauthenticated API request: 401, no redirect.
	req = httptest.NewRequest("GET", "/api/jobs", nil)
	rec = httptest.NewRecorder()
	protected.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated API request: status %d", rec.Code)
	}

	// /healthz, /favicon.ico and /auth/* always pass through.
	for _, path := range []string{"/healthz", "/favicon.ico", "/auth/login"} {
		req = httptest.NewRequest("GET", path, nil)
		rec = httptest.NewRecorder()
		protected.ServeHTTP(rec, req)
		if rec.Code != http.StatusTeapot {
			t.Fatalf("%s should bypass auth: status %d", path, rec.Code)
		}
	}

	// A browser requests /favicon.ico automatically alongside every page
	// load; unauthenticated, it must never set the return-to cookie, or it
	// can win the race against the real page's request and land the user
	// on /favicon.ico after signing in instead of the page they wanted.
	req = httptest.NewRequest("GET", "/favicon.ico", nil)
	rec = httptest.NewRecorder()
	protected.ServeHTTP(rec, req)
	if cookieValue(rec.Result().Cookies(), oauthReturnTo) != "" {
		t.Fatal("favicon request set the return-to cookie")
	}

	// A valid session passes through to the wrapped handler.
	transient := startLogin(t, a)
	code := idTokenCode(t, p.baseURL, clientID, cookieValue(transient, oauthNonce), "person@example.com", true)
	cb := callback(a, transient, url.Values{"state": {cookieValue(transient, oauthState)}, "code": {code}})
	session := cookieValue(cb.Result().Cookies(), sessionCookie)

	req = httptest.NewRequest("GET", "/jobs", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: session})
	rec = httptest.NewRecorder()
	protected.ServeHTTP(rec, req)
	if rec.Code != http.StatusTeapot {
		t.Fatalf("authenticated request: status %d", rec.Code)
	}
}

func TestLogoutClearsSession(t *testing.T) {
	a, _ := newTestAuth(t)
	req := httptest.NewRequest("POST", "/auth/logout", nil)
	rec := httptest.NewRecorder()
	a.handleLogout(rec, req)
	c := rec.Result().Cookies()
	if len(c) != 1 || c[0].Name != sessionCookie || c[0].MaxAge >= 0 {
		t.Fatalf("logout did not clear the session cookie: %+v", c)
	}
}

func TestLoadOrCreateSessionKeyPersists(t *testing.T) {
	dir := t.TempDir()
	k1, err := LoadOrCreateSessionKey(dir)
	if err != nil || len(k1) != 32 {
		t.Fatalf("first load: %d bytes, %v", len(k1), err)
	}
	fi, err := os.Stat(filepath.Join(dir, "session.key"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("session.key mode: %v %v", fi, err)
	}
	k2, err := LoadOrCreateSessionKey(dir)
	if err != nil || string(k2) != string(k1) {
		t.Fatal("session key not stable across loads")
	}
}

func TestNewWithoutOIDCStillWorks(t *testing.T) {
	a, err := New(context.Background(), Config{}, make([]byte, 32))
	if err != nil {
		t.Fatalf("empty config: %v", err)
	}
	if a.OIDCEnabled() {
		t.Fatal("OIDCEnabled true with no issuer configured")
	}
	rec := httptest.NewRecorder()
	a.StartOIDC(rec, httptest.NewRequest("GET", "/auth/start", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("StartOIDC with no OIDC configured: status %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	a.CompleteOIDC(rec, httptest.NewRequest("GET", "/auth/callback", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("CompleteOIDC with no OIDC configured: status %d", rec.Code)
	}
	// Sessions still work without OIDC -- a recovery-code login (the server
	// package's job) issues one the same way.
	rec = httptest.NewRecorder()
	a.IssueSession(rec, httptest.NewRequest("GET", "/", nil), "someone")
	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(rec.Result().Cookies()[0])
	if _, ok := a.CurrentUser(req); !ok {
		t.Fatal("session from IssueSession not recognized without OIDC configured")
	}
}

func TestNewRequiresCompleteConfigWhenOIDCEnabled(t *testing.T) {
	if _, err := New(context.Background(), Config{Issuer: "https://example.com"}, make([]byte, 32)); err == nil {
		t.Fatal("missing client ID/secret/redirect accepted")
	}
}

func TestNewRequiresSessionKey(t *testing.T) {
	if _, err := New(context.Background(), Config{}, make([]byte, 16)); err == nil {
		t.Fatal("short session key accepted")
	}
}

// An address the provider has not verified identifies nobody, on the allowed
// list or not. A provider that does not send the claim at all is not refused.
func TestCallbackRefusesUnverifiedEmail(t *testing.T) {
	a, p := newTestAuth(t, "boss@example.com")
	transient := startLogin(t, a)
	code := idTokenCode(t, p.baseURL, clientID, cookieValue(transient, oauthNonce), "boss@example.com", false)
	rec := callback(a, transient, url.Values{"state": {cookieValue(transient, oauthState)}, "code": {code}})
	if rec.Code != http.StatusForbidden || cookieValue(rec.Result().Cookies(), sessionCookie) != "" {
		t.Fatalf("unverified email: status %d, cookies %+v", rec.Code, rec.Result().Cookies())
	}
}

// memSignOuts is a SignOuts kept in memory.
type memSignOuts map[string]time.Time

func (m memSignOuts) SignedOutAt(_ context.Context, id string) (time.Time, error) { return m[id], nil }
func (m memSignOuts) SignOut(_ context.Context, id string, at time.Time) error {
	m[id] = at
	return nil
}

func sessionFor(t *testing.T, a *Authenticator, identity string) *http.Cookie {
	t.Helper()
	rec := httptest.NewRecorder()
	a.IssueSession(rec, httptest.NewRequest("GET", "/", nil), identity)
	return &http.Cookie{Name: sessionCookie, Value: cookieValue(rec.Result().Cookies(), sessionCookie)}
}

func signedIn(a *Authenticator, c *http.Cookie) bool {
	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(c)
	_, ok := a.CurrentUser(req)
	return ok
}

// Signing out ends the session on the server: a copy of the cookie is refused
// afterwards, and signing in again works.
func TestLogoutEndsTheSessionOnTheServer(t *testing.T) {
	a, _ := newTestAuth(t)
	a.SetSignOuts(memSignOuts{})
	old := sessionFor(t, a, "person@example.com")
	other := sessionFor(t, a, "other@example.com")
	if !signedIn(a, old) {
		t.Fatal("a fresh session is not accepted")
	}
	req := httptest.NewRequest("POST", "/auth/logout", nil)
	req.AddCookie(old)
	a.Logout(httptest.NewRecorder(), req)
	if signedIn(a, old) {
		t.Fatal("the cookie still works after signing out")
	}
	if !signedIn(a, other) {
		t.Fatal("signing out ended somebody else's session")
	}
	if !signedIn(a, sessionFor(t, a, "person@example.com")) {
		t.Fatal("a session started after signing out is refused")
	}
}

// Taking an address off the allowed list ends its session; a recovery-code
// session is not an address and stays.
func TestSessionEndsWhenTakenOffTheAllowedList(t *testing.T) {
	a, _ := newTestAuth(t, "boss@example.com")
	boss, recovery := sessionFor(t, a, "boss@example.com"), sessionFor(t, a, RecoveryIdentity)
	if !signedIn(a, boss) || !signedIn(a, recovery) {
		t.Fatal("fresh sessions are not accepted")
	}
	cfg := a.cfg
	cfg.AllowedEmails = []string{"someone-else@example.com"}
	a2, err := New(context.Background(), cfg, a.key)
	if err != nil {
		t.Fatal(err)
	}
	if signedIn(a2, boss) {
		t.Fatal("the session of an address taken off the allowed list still works")
	}
	if !signedIn(a2, recovery) {
		t.Fatal("the allowed list ended a recovery-code session")
	}
}

// A cookie from before sessions recorded their start keeps working, and signing
// out ends it like any other.
func TestOldSessionCookieStillWorks(t *testing.T) {
	a, _ := newTestAuth(t)
	a.SetSignOuts(memSignOuts{})
	exp := strconv.FormatInt(time.Now().Add(sessionMaxAge).Unix(), 10)
	old := &http.Cookie{Name: sessionCookie, Value: signToken(a.key, "person@example.com|"+exp)}
	if !signedIn(a, old) {
		t.Fatal("a cookie in the old form is refused")
	}
	req := httptest.NewRequest("POST", "/auth/logout", nil)
	req.AddCookie(old)
	a.Logout(httptest.NewRecorder(), req)
	if signedIn(a, old) {
		t.Fatal("a cookie in the old form still works after signing out")
	}
}
