package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/voidgrid/voidgrid-backup/internal/webauth"
)

// startFakeIssuer is fakeIssuer (see http_setup_test.go) wired up and
// running, for tests that build their own webauth.Config against it rather
// than attaching it directly via newAuthController.
func startFakeIssuer(t *testing.T) string {
	t.Helper()
	issuer := &fakeIssuer{}
	srv := httptest.NewServer(issuer)
	t.Cleanup(srv.Close)
	issuer.baseURL = srv.URL
	return srv.URL
}

func TestInitialSetupConfiguresOIDC(t *testing.T) {
	ctx := context.Background()
	c := newController(t)
	srv := httptest.NewServer(NewHandler(c))
	defer srv.Close()
	issuerURL := startFakeIssuer(t)

	if c.Auth().OIDCEnabled() {
		t.Fatal("OIDC enabled before setup")
	}
	tok, err := c.SetupToken(ctx)
	if err != nil {
		t.Fatal(err)
	}
	form := url.Values{
		"token": {tok}, "oidc_enabled": {"on"}, "oidc_issuer": {issuerURL},
		"oidc_client_id": {"test-client"}, "oidc_client_secret": {"test-secret"},
		"oidc_redirect_url": {"https://backup.example/auth/callback"},
	}
	resp, err := http.PostForm(srv.URL+"/setup", form)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("setup with OIDC fields: status %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Took effect immediately: no restart, no rebuilding NewHandler.
	if !c.Auth().OIDCEnabled() {
		t.Fatal("OIDC not enabled after setup submitted it")
	}
	if c.Auth().Issuer() != issuerURL {
		t.Fatalf("issuer = %q, want %q", c.Auth().Issuer(), issuerURL)
	}
	stored, err := c.OIDCConfig(ctx)
	if err != nil || stored.Issuer != issuerURL || stored.ClientID != "test-client" {
		t.Fatalf("stored OIDC config: %+v %v", stored, err)
	}

	// The sign-in chooser now offers SSO, over the same running server.
	body, err := http.Get(srv.URL + "/auth/login")
	if err != nil {
		t.Fatal(err)
	}
	defer body.Body.Close()
	if body.StatusCode != http.StatusOK {
		t.Fatalf("GET /auth/login: status %d", body.StatusCode)
	}
}

func TestInitialSetupRejectsBadIssuer(t *testing.T) {
	ctx := context.Background()
	c := newController(t)
	srv := httptest.NewServer(NewHandler(c))
	defer srv.Close()

	tok, err := c.SetupToken(ctx)
	if err != nil {
		t.Fatal(err)
	}
	form := url.Values{
		"token": {tok}, "oidc_enabled": {"on"}, "oidc_issuer": {"http://127.0.0.1:1/does-not-exist"},
		"oidc_client_id": {"x"}, "oidc_client_secret": {"y"},
		"oidc_redirect_url": {"https://backup.example/auth/callback"},
	}
	resp, err := http.PostForm(srv.URL+"/setup", form)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("setup with a bad issuer: status %d, want 400", resp.StatusCode)
	}
	// And setup itself is still incomplete: a bad OIDC config didn't
	// silently eat the token or otherwise half-complete the bootstrap.
	if done, err := c.SetupComplete(ctx); err != nil || done {
		t.Fatalf("setup complete after a rejected OIDC config: %v %v", done, err)
	}

	// The same (still-valid) token, without OIDC fields, now succeeds.
	resp, err = http.PostForm(srv.URL+"/setup", url.Values{"token": {tok}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("retrying setup without OIDC: status %d", resp.StatusCode)
	}
}

func TestSaveAuthSettingsRequiresSession(t *testing.T) {
	c := newController(t)
	if _, err := c.GenerateRecoveryCodes(context.Background()); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(NewHandler(c))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/authentication")
	if err != nil {
		t.Fatal(err)
	}
	if resp.Request.URL.Path != "/auth/login" {
		t.Fatalf("unauthenticated /authentication should land on /auth/login, got %s", resp.Request.URL.Path)
	}
}

func TestSaveAuthSettingsTakesEffectImmediately(t *testing.T) {
	ctx := context.Background()
	c := newController(t)
	if _, err := c.GenerateRecoveryCodes(ctx); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(NewHandler(c))
	defer srv.Close()
	client := authedClient(t, c, srv)
	issuerURL := startFakeIssuer(t)

	if c.Auth().OIDCEnabled() {
		t.Fatal("OIDC enabled before saving settings")
	}
	form := url.Values{
		"oidc_enabled": {"on"}, "oidc_issuer": {issuerURL}, "oidc_client_id": {"test-client"},
		"oidc_client_secret": {"test-secret"}, "oidc_redirect_url": {"https://backup.example/auth/callback"},
	}
	resp, err := client.PostForm(srv.URL+"/authentication", form)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("save authentication settings: status %d", resp.StatusCode)
	}
	if !c.Auth().OIDCEnabled() {
		t.Fatal("OIDC not enabled immediately after saving")
	}

	// GET /auth/start now redirects toward the provider instead of 404ing
	// -- proving the route (registered once, at NewHandler time) dispatches
	// to whichever Authenticator is *currently* installed, not the one
	// that existed when NewHandler ran.
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, _ := http.NewRequest("GET", srv.URL+"/auth/start", nil)
	req.Header.Set("Cookie", "") // deliberately unauthenticated; /auth/* is always reachable
	startResp, err := noRedirect.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if startResp.StatusCode != http.StatusSeeOther || !strings.HasPrefix(startResp.Header.Get("Location"), issuerURL) {
		t.Fatalf("GET /auth/start after enabling OIDC: status %d location %q", startResp.StatusCode, startResp.Header.Get("Location"))
	}
}

func TestSaveAuthSettingsRejectsBadIssuer(t *testing.T) {
	ctx := context.Background()
	c := newController(t)
	if _, err := c.GenerateRecoveryCodes(ctx); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(NewHandler(c))
	defer srv.Close()
	client := authedClient(t, c, srv)

	form := url.Values{
		"oidc_enabled": {"on"}, "oidc_issuer": {"http://127.0.0.1:1/does-not-exist"},
		"oidc_client_id": {"test-client"}, "oidc_client_secret": {"test-secret"},
		"oidc_redirect_url": {"https://backup.example/auth/callback"},
	}
	resp, err := client.PostForm(srv.URL+"/authentication", form)
	if err != nil {
		t.Fatal(err)
	}
	// Exactly 400, not just "not 200": a bad issuer is a bad *input*, not
	// a server fault, and this is precisely the assertion a looser "!= 200"
	// check let slip through once already (it rendered as a 500).
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("an unreachable issuer: status %d, want 400", resp.StatusCode)
	}
	if c.Auth().OIDCEnabled() {
		t.Fatal("OIDC ended up enabled despite a rejected config")
	}
	if cfg, err := c.OIDCConfig(ctx); err != nil || cfg.Enabled() {
		t.Fatalf("a rejected config was persisted: %+v %v", cfg, err)
	}
}

func TestFlagOIDCConfigWinsOverCatalog(t *testing.T) {
	ctx := context.Background()
	c := newController(t)
	flagIssuer := startFakeIssuer(t)
	catalogIssuer := startFakeIssuer(t)

	// Save a catalog config pointing at one provider...
	if err := c.SetOIDCConfig(ctx, webauth.Config{
		Issuer: catalogIssuer, ClientID: "catalog-client", ClientSecret: "s",
		RedirectURL: "https://backup.example/auth/callback",
	}); err != nil {
		t.Fatal(err)
	}
	// ...but flags, given, point at a different one and must win.
	c.FlagOIDCConfig = webauth.Config{
		Issuer: flagIssuer, ClientID: "flag-client", ClientSecret: "s",
		RedirectURL: "https://backup.example/auth/callback",
	}
	if err := c.ReloadAuth(ctx); err != nil {
		t.Fatal(err)
	}
	if got := c.Auth().Issuer(); got != flagIssuer {
		t.Fatalf("issuer = %q, want the flag-provided one %q", got, flagIssuer)
	}
}

func TestReloadAuthFallsBackWhenOIDCUnreachable(t *testing.T) {
	c := newController(t)
	c.FlagOIDCConfig = webauth.Config{
		Issuer: "http://127.0.0.1:1/does-not-exist", ClientID: "x", ClientSecret: "y",
		RedirectURL: "https://backup.example/auth/callback",
	}
	if err := c.ReloadAuth(context.Background()); err != nil {
		t.Fatalf("ReloadAuth should fall back, not fail outright: %v", err)
	}
	if c.Auth() == nil {
		t.Fatal("no Authenticator installed after a failed OIDC reload")
	}
	if c.Auth().OIDCEnabled() {
		t.Fatal("OIDCEnabled true despite discovery having failed")
	}
}
