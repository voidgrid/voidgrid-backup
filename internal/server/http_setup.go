package server

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/voidgrid/voidgrid-backup/internal/webauth"
)

type signInPage struct {
	base
	OIDCEnabled bool
}

// signIn is the chooser page a broken OIDC provider never has to be
// reached to see: SSO (when configured), or a recovery code (always).
// Authenticator.Middleware redirects here (at /auth/login) whenever a
// session is required and missing; the actual OIDC redirect lives at
// /auth/start.
func (u *ui) signIn(w http.ResponseWriter, r *http.Request) {
	u.render(w, http.StatusOK, "signin", signInPage{u.newBase("Sign in", "", r), u.c.Auth().OIDCEnabled()})
}

type recoveryLoginPage struct{ base }

func (u *ui) recoveryLogin(w http.ResponseWriter, r *http.Request) {
	u.render(w, http.StatusOK, "recovery-login", recoveryLoginPage{u.newBase("Recovery code", "", r)})
}

// failDelay blunts naive automated guessing against a wrong setup token or
// recovery code without needing real rate-limiting infrastructure: both
// secrets are long and random enough that brute force isn't realistic
// regardless, this just costs an attacker a little wall-clock time per try.
const failDelay = 300 * time.Millisecond

func (u *ui) recoveryLoginSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	code := strings.TrimSpace(r.PostForm.Get("code"))
	ok, err := u.c.VerifyRecoveryCode(r.Context(), code)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !ok {
		time.Sleep(failDelay)
		p := recoveryLoginPage{u.newBase("Recovery code", "", r)}
		p.Error = "Invalid or already-used code."
		u.render(w, http.StatusUnauthorized, "recovery-login", p)
		return
	}
	u.c.Auth().IssueSession(w, r, "recovery code")
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

type setupPage struct {
	base
	Done         bool // recovery codes have been generated at least once
	InitialSetup bool // this generation just satisfied requireSetup, not a later regeneration
	Total        int
	Remaining    int
	Codes        []string // set only immediately after (re)generating; never re-shown

	// OIDC, offered only on the way to InitialSetup (afterwards it's
	// /authentication's job -- see authSettings).
	OIDCManagedByFlags bool // VB_OIDC_* env vars are set; the form is hidden
	OIDCForm           url.Values
}

// setup is the wizard: reachable without a session only until recovery
// codes exist (Authenticator.Middleware and requireSetup both special-case
// this path). Once they exist, reaching it again to regenerate a batch
// requires an actual session, same as anything else in the UI; OIDC
// settings move to /authentication once you're past this point.
func (u *ui) setup(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	done, err := u.c.SetupComplete(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if _, authed := u.c.Auth().CurrentUser(r); done && !authed {
		http.NotFound(w, r)
		return
	}
	total, remaining, err := u.c.RemainingRecoveryCodes(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	p := setupPage{base: u.newBase("Setup", "", r), Done: done, Total: total, Remaining: remaining}
	if !done {
		p.OIDCManagedByFlags = u.c.FlagOIDCConfig.Enabled()
		p.OIDCForm = url.Values{}
	}
	u.render(w, http.StatusOK, "setup", p)
}

func (u *ui) generateSetupCodes(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	done, err := u.c.SetupComplete(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	_, authed := u.c.Auth().CurrentUser(r)
	if done && !authed {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if !done {
		tok, err := u.c.SetupToken(ctx)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if r.PostForm.Get("token") != tok {
			time.Sleep(failDelay)
			p := setupPage{base: u.newBase("Setup", "", r), Done: false,
				OIDCManagedByFlags: u.c.FlagOIDCConfig.Enabled(), OIDCForm: r.PostForm}
			p.Error = "Incorrect setup token."
			u.render(w, http.StatusBadRequest, "setup", p)
			return
		}
		// OIDC is part of the one-time bootstrap too, so the wizard
		// actually covers it -- not just recovery codes -- but only when
		// it isn't already fixed by the environment.
		if !u.c.FlagOIDCConfig.Enabled() {
			cfg, err := oidcConfigFromForm(r.PostForm, webauth.Config{})
			if err != nil {
				p := setupPage{base: u.newBase("Setup", "", r), Done: false, OIDCForm: r.PostForm}
				p.Error = err.Error()
				u.render(w, http.StatusBadRequest, "setup", p)
				return
			}
			if err := u.c.SetOIDCConfig(ctx, cfg); err != nil {
				// A validation failure (an unreachable or misconfigured
				// issuer, most likely) is a bad *input*, not a server
				// fault -- render it back on the form, same as any other
				// field error, rather than a generic 500.
				p := setupPage{base: u.newBase("Setup", "", r), Done: false, OIDCForm: r.PostForm}
				p.Error = "OIDC settings: " + err.Error()
				u.render(w, http.StatusBadRequest, "setup", p)
				return
			}
		}
	}
	codes, err := u.c.GenerateRecoveryCodes(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	u.render(w, http.StatusOK, "setup", setupPage{
		base: u.newBase("Setup", "", r), Done: true, InitialSetup: !done,
		Total: len(codes), Remaining: len(codes), Codes: codes,
	})
}

type authSettingsPage struct {
	base
	ManagedByFlags bool // VB_OIDC_* env vars are set; not editable here
	Config         webauth.Config
	Form           url.Values
}

// authSettings is the ongoing OIDC settings page, for once setup is done
// and you're signed in -- turn OIDC on, off, or point it at a different
// provider, without restarting the process (see Controller.ReloadAuth).
// Recovery codes are managed at /setup instead, reachable from here too
// once signed in.
func (u *ui) authSettings(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	cfg, err := u.c.OIDCConfig(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	u.render(w, http.StatusOK, "authentication", authSettingsPage{
		base:           u.newBase("Authentication", "authentication", r),
		ManagedByFlags: u.c.FlagOIDCConfig.Enabled(), Config: cfg, Form: formFromOIDCConfig(cfg),
	})
}

func (u *ui) saveAuthSettings(w http.ResponseWriter, r *http.Request) {
	if u.c.FlagOIDCConfig.Enabled() {
		http.Error(w, "OIDC is set by VB_OIDC_* environment variables, not editable here", http.StatusBadRequest)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	existing, err := u.c.OIDCConfig(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	cfg, err := oidcConfigFromForm(r.PostForm, existing)
	if err != nil {
		p := authSettingsPage{base: u.newBase("Authentication", "authentication", r), Config: existing, Form: r.PostForm}
		p.Error = err.Error()
		u.render(w, http.StatusBadRequest, "authentication", p)
		return
	}
	if err := u.c.SetOIDCConfig(ctx, cfg); err != nil {
		// Same reasoning as generateSetupCodes: a validation failure here
		// is a bad input (an unreachable or misconfigured issuer), not a
		// server fault.
		p := authSettingsPage{base: u.newBase("Authentication", "authentication", r), Config: existing, Form: r.PostForm}
		p.Error = "OIDC settings: " + err.Error()
		u.render(w, http.StatusBadRequest, "authentication", p)
		return
	}
	redirectNotice(w, r, "/authentication", "Authentication settings saved.")
}

// formFromOIDCConfig seeds the form with everything except the client
// secret, which stays blank -- "leave blank to keep the current value" (see
// oidcConfigFromForm), except on the very first save.
func formFromOIDCConfig(cfg webauth.Config) url.Values {
	f := url.Values{}
	if cfg.Enabled() {
		f.Set("oidc_enabled", "on")
	}
	f.Set("oidc_issuer", cfg.Issuer)
	f.Set("oidc_client_id", cfg.ClientID)
	f.Set("oidc_redirect_url", cfg.RedirectURL)
	f.Set("oidc_allowed_emails", strings.Join(cfg.AllowedEmails, ", "))
	return f
}

// oidcConfigFromForm builds a webauth.Config from submitted form values. A
// blank client secret keeps whatever was already saved, the same
// leave-blank-to-keep convention the notifications settings use.
func oidcConfigFromForm(f url.Values, existing webauth.Config) (webauth.Config, error) {
	if f.Get("oidc_enabled") == "" {
		return webauth.Config{}, nil
	}
	issuer := strings.TrimSpace(f.Get("oidc_issuer"))
	clientID := strings.TrimSpace(f.Get("oidc_client_id"))
	redirectURL := strings.TrimSpace(f.Get("oidc_redirect_url"))
	if issuer == "" || clientID == "" || redirectURL == "" {
		return webauth.Config{}, errors.New("issuer, client ID and redirect URL are all required")
	}
	clientSecret := f.Get("oidc_client_secret")
	if clientSecret == "" {
		clientSecret = existing.ClientSecret
	}
	if clientSecret == "" {
		return webauth.Config{}, errors.New("client secret is required")
	}
	var allowed []string
	for _, e := range strings.Split(f.Get("oidc_allowed_emails"), ",") {
		if e = strings.TrimSpace(e); e != "" {
			allowed = append(allowed, e)
		}
	}
	return webauth.Config{
		Issuer: issuer, ClientID: clientID, ClientSecret: clientSecret,
		RedirectURL: redirectURL, AllowedEmails: allowed,
	}, nil
}

// requireSetup forces every request except /healthz, /favicon.ico and
// /setup itself to /setup until recovery codes exist. Always applied,
// outermost: even /auth/start and /auth/callback are blocked by it, so
// there is no way to reach a state where signing in is possible but no
// recovery code has ever been issued. /favicon.ico is exempted so a
// browser's automatic request for it doesn't redirect to /setup and get
// tangled up in the same page load as the real one.
func (u *ui) requireSetup(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" || r.URL.Path == "/favicon.ico" || r.URL.Path == "/setup" {
			next.ServeHTTP(w, r)
			return
		}
		done, err := u.c.SetupComplete(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if !done {
			http.Redirect(w, r, "/setup", http.StatusSeeOther)
			return
		}
		next.ServeHTTP(w, r)
	})
}
