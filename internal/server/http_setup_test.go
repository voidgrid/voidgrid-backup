package server

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"regexp"
	"testing"

	"github.com/voidgrid/voidgrid-backup/internal/webauth"
)

// fakeIssuer serves just enough OIDC discovery for webauth.New to succeed.
// None of these tests drive an actual OIDC callback (that's webauth's own
// job, tested in internal/webauth), so nothing past discovery is needed.
type fakeIssuer struct{ baseURL string }

func (f *fakeIssuer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/.well-known/openid-configuration" {
		http.NotFound(w, r)
		return
	}
	json.NewEncoder(w).Encode(map[string]any{
		"issuer": f.baseURL, "authorization_endpoint": f.baseURL + "/authorize",
		"token_endpoint": f.baseURL + "/token", "jwks_uri": f.baseURL + "/jwks",
	})
}

// newAuthController is newController plus OIDC login wired to a fake
// provider, for the setup-wizard and recovery-code tests below.
func newAuthController(t *testing.T) *Controller {
	t.Helper()
	c := newController(t)
	issuer := &fakeIssuer{}
	srv := httptest.NewServer(issuer)
	t.Cleanup(srv.Close)
	issuer.baseURL = srv.URL

	key := make([]byte, 32)
	rand.Read(key)
	auth, err := webauth.New(context.Background(), webauth.Config{
		Issuer: srv.URL, ClientID: "test-client", ClientSecret: "test-secret",
		RedirectURL: "https://backup.example/auth/callback",
	}, key)
	if err != nil {
		t.Fatal(err)
	}
	c.setAuth(auth)
	return c
}

var recoveryCodeLine = regexp.MustCompile(`<li>([^<]+)</li>`)

func TestSetupWizardAndRecoveryLogin(t *testing.T) {
	ctx := context.Background()
	c := newAuthController(t)
	srv := httptest.NewServer(NewHandler(c))
	defer srv.Close()

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Jar: jar}

	// /healthz must stay reachable even while setup is incomplete, or the
	// container's own health check would fail exactly when OIDC is on and
	// nobody can sign in yet -- the situation this feature exists for.
	if resp, err := client.Get(srv.URL + "/healthz"); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("/healthz before setup: %v %v", resp, err)
	}
	// A browser's automatic /favicon.ico request must not get redirected
	// to /setup either -- caught only after it produced a real 404 for a
	// real user, not by anything short of hitting the actual browser
	// behavior that triggers it.
	if resp, err := client.Get(srv.URL + "/favicon.ico"); err != nil || resp.StatusCode != http.StatusNotFound {
		t.Fatalf("/favicon.ico before setup: %v %v, want a plain 404", resp, err)
	}

	// Everything else, including the OIDC redirect itself, is caught by
	// requireSetup before it can ever reach webauth's own routing -- this
	// is the property that actually prevents the lockout.
	resp, err := client.Get(srv.URL + "/auth/start")
	if err != nil {
		t.Fatal(err)
	}
	if resp.Request.URL.Path != "/setup" {
		t.Fatalf("/auth/start before setup should land on /setup, got %s", resp.Request.URL.Path)
	}

	resp, err = client.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	if resp.Request.URL.Path != "/setup" {
		t.Fatalf("/ before setup should land on /setup, got %s", resp.Request.URL.Path)
	}

	// A wrong token doesn't complete setup.
	resp, err = client.PostForm(srv.URL+"/setup", url.Values{"token": {"wrong"}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("wrong setup token: status %d", resp.StatusCode)
	}
	if done, err := c.SetupComplete(ctx); err != nil || done {
		t.Fatalf("setup complete after a wrong token: %v %v", done, err)
	}

	// The real token does.
	tok, err := c.SetupToken(ctx)
	if err != nil {
		t.Fatal(err)
	}
	resp, err = client.PostForm(srv.URL+"/setup", url.Values{"token": {tok}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("correct setup token: status %d", resp.StatusCode)
	}
	bodyBytes, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	codes := recoveryCodeLine.FindAllStringSubmatch(string(bodyBytes), -1)
	if len(codes) != 10 {
		t.Fatalf("expected 10 recovery codes in the response, got %d: %q", len(codes), bodyBytes)
	}
	if done, err := c.SetupComplete(ctx); err != nil || !done {
		t.Fatalf("setup not complete after the real token: %v %v", done, err)
	}

	// Now / redirects to the sign-in chooser, not /setup, and /setup itself
	// is unreachable anonymously.
	resp, err = client.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	if resp.Request.URL.Path != "/auth/login" {
		t.Fatalf("/ after setup should land on /auth/login, got %s", resp.Request.URL.Path)
	}
	resp, err = client.Get(srv.URL + "/setup")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("/setup after setup, anonymous: status %d, want 404", resp.StatusCode)
	}

	// A generated code signs in...
	code := codes[0][1]
	resp, err = client.PostForm(srv.URL+"/auth/recovery", url.Values{"code": {code}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Request.URL.Path != "/" || resp.StatusCode != http.StatusOK {
		t.Fatalf("recovery login: landed on %s, status %d", resp.Request.URL.Path, resp.StatusCode)
	}
	// ...and the session actually sticks, no more redirect to sign in.
	resp, err = client.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	if resp.Request.URL.Path != "/" || resp.StatusCode != http.StatusOK {
		t.Fatalf("after recovery login: landed on %s, status %d", resp.Request.URL.Path, resp.StatusCode)
	}

	// ...but the same code doesn't work twice.
	resp, err = client.PostForm(srv.URL+"/auth/recovery", url.Values{"code": {code}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("reused recovery code: status %d, want 401", resp.StatusCode)
	}
}

func TestGenerateRecoveryCodesInvalidatesThePreviousBatch(t *testing.T) {
	ctx := context.Background()
	c := newController(t)
	first, err := c.GenerateRecoveryCodes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.GenerateRecoveryCodes(ctx); err != nil {
		t.Fatal(err)
	}
	if ok, err := c.VerifyRecoveryCode(ctx, first[0]); err != nil || ok {
		t.Fatalf("a code from the replaced batch still works: ok=%v err=%v", ok, err)
	}
}

func TestVerifyRecoveryCodeRejectsUnknown(t *testing.T) {
	ctx := context.Background()
	c := newController(t)
	if ok, err := c.VerifyRecoveryCode(ctx, "not-a-real-code"); err != nil || ok {
		t.Fatalf("unknown code accepted: ok=%v err=%v", ok, err)
	}
	if _, err := c.GenerateRecoveryCodes(ctx); err != nil {
		t.Fatal(err)
	}
	if ok, err := c.VerifyRecoveryCode(ctx, "still-not-a-real-code"); err != nil || ok {
		t.Fatalf("unknown code accepted once a batch exists: ok=%v err=%v", ok, err)
	}
}

func TestSetupTokenStableAcrossCalls(t *testing.T) {
	ctx := context.Background()
	c := newController(t)
	a, err := c.SetupToken(ctx)
	if err != nil {
		t.Fatal(err)
	}
	b, err := c.SetupToken(ctx)
	if err != nil || a != b {
		t.Fatalf("setup token not stable: %q vs %q (%v)", a, b, err)
	}
}
