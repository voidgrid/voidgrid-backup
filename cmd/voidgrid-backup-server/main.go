// Command voidgrid-backup-server runs the web UI, API, scheduler and agent
// controller. `voidgrid-backup-server healthcheck` probes a running server.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata" // TZ works in the distroless image, which has no zoneinfo

	"github.com/voidgrid/voidgrid-backup/internal/catalog"
	"github.com/voidgrid/voidgrid-backup/internal/envflag"
	"github.com/voidgrid/voidgrid-backup/internal/health"
	"github.com/voidgrid/voidgrid-backup/internal/logbuf"
	"github.com/voidgrid/voidgrid-backup/internal/pki"
	"github.com/voidgrid/voidgrid-backup/internal/server"
	"github.com/voidgrid/voidgrid-backup/internal/version"
	"github.com/voidgrid/voidgrid-backup/internal/webauth"
)

func main() {
	listen := envflag.String("listen", "VB_LISTEN", ":8080", "HTTP address for the UI, API and /healthz")
	data := envflag.String("data", "VB_DATA", "/data", "directory for the catalog and the CA")
	poll := envflag.Duration("poll", "VB_POLL", 30*time.Second, "how often to check agents")
	oidcIssuer := envflag.String("oidc-issuer", "VB_OIDC_ISSUER", "", "OIDC provider issuer URL; empty disables login and leaves every route open")
	oidcClientID := envflag.String("oidc-client-id", "VB_OIDC_CLIENT_ID", "", "OIDC client ID")
	oidcClientSecret := envflag.String("oidc-client-secret", "VB_OIDC_CLIENT_SECRET", "", "OIDC client secret")
	oidcRedirectURL := envflag.String("oidc-redirect-url", "VB_OIDC_REDIRECT_URL", "", "OIDC callback URL as the provider sees it, e.g. https://host/auth/callback")
	oidcAllowedEmails := envflag.String("oidc-allowed-emails", "VB_OIDC_ALLOWED_EMAILS", "", "comma-separated allowed identities; empty allows any the provider authenticates")

	args := os.Args[1:]
	healthcheck := len(args) > 0 && args[0] == "healthcheck"
	if healthcheck {
		args = args[1:]
	}
	flag.CommandLine.Parse(args)
	if healthcheck {
		url, err := health.LocalURL(*listen, "/healthz")
		if err != nil {
			slog.Error("healthcheck", "err", err)
			os.Exit(1)
		}
		health.Probe(url)
	}
	oidcCfg := webauth.Config{
		Issuer: *oidcIssuer, ClientID: *oidcClientID, ClientSecret: *oidcClientSecret, RedirectURL: *oidcRedirectURL,
		AllowedEmails: splitCommaList(*oidcAllowedEmails),
	}
	// Keep the recent log in memory too, for the Logs page. What goes to
	// stderr (docker logs) is unchanged.
	logs := logbuf.New(2000)
	slog.SetDefault(slog.New(logs.Handler(logbuf.NewStderrHandler(os.Stderr, slog.LevelInfo))))
	if err := run(*listen, *data, *poll, oidcCfg, logs); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func splitCommaList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func run(listen, data string, poll time.Duration, oidcCfg webauth.Config, logs *logbuf.Buffer) error {
	if err := os.MkdirAll(data, 0o700); err != nil {
		return err
	}
	cat, err := catalog.Open(filepath.Join(data, "catalog.db"))
	if err != nil {
		return err
	}
	defer cat.Close()
	pkiDir := filepath.Join(data, "pki")
	ca, err := pki.LoadOrCreateCA(pkiDir)
	if err != nil {
		return err
	}
	clientCert, err := pki.LoadOrCreateClientCert(pkiDir, ca)
	if err != nil {
		return err
	}
	key, err := webauth.LoadOrCreateSessionKey(data)
	if err != nil {
		return fmt.Errorf("session key: %w", err)
	}
	ctrl := &server.Controller{Catalog: cat, CA: ca, ClientCert: clientCert, SessionKey: key, FlagOIDCConfig: oidcCfg, LogBuf: logs}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Signing in is always required -- with OIDC if configured (as flags
	// here, or saved earlier through the setup wizard / the authentication
	// settings page), with a recovery code alone if not. There is no
	// "every route open" mode. ReloadAuth is also what those two pages
	// call after saving a change, so OIDC can be turned on, off, or
	// reconfigured without restarting the process.
	if err := ctrl.ReloadAuth(ctx); err != nil {
		return fmt.Errorf("oidc: %w", err)
	}
	if ctrl.Auth().OIDCEnabled() {
		slog.Info("OIDC login enabled", "issuer", ctrl.Auth().Issuer())
	} else {
		slog.Info("no OIDC issuer configured: signing in is by recovery code only")
	}
	if done, err := ctrl.SetupComplete(ctx); err != nil {
		return fmt.Errorf("check setup status: %w", err)
	} else if !done {
		tok, err := ctrl.SetupToken(ctx)
		if err != nil {
			return fmt.Errorf("setup token: %w", err)
		}
		slog.Warn("setup is not complete: nothing works until it is",
			"visit", "/setup", "setup_token", tok)
	}
	if n, err := cat.FailInterruptedRuns(ctx, time.Now()); err != nil {
		return err
	} else if n > 0 {
		slog.Warn("marked runs interrupted by the last shutdown as failed", "count", n)
	}
	go ctrl.Poll(ctx, poll)
	go ctrl.Schedule(ctx, 30*time.Second)

	srv := &http.Server{Addr: listen, Handler: server.NewHandler(ctrl), ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	slog.Info("voidgrid-backup-server listening", "addr", listen, "version", version.Version)

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
