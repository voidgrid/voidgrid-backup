// Package notify sends a run's outcome to whichever channels are
// configured: Notifarr, a Discord webhook, and/or email. Every channel is
// independent and optional; a channel with Enabled false, or a nil Config,
// is silently skipped. Sending is best-effort: a failure on one channel
// never stops the others, and callers are expected to log Results rather
// than treat them as fatal to whatever triggered the notification.
package notify

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/smtp"
	"strings"
	"time"
)

// Config holds every channel's settings. All three are independently
// optional; leave a pointer nil (or Enabled false) to turn that channel off.
type Config struct {
	Notifarr *NotifarrConfig `json:"notifarr,omitempty"`
	Discord  *DiscordConfig  `json:"discord,omitempty"`
	Email    *EmailConfig    `json:"email,omitempty"`
	// URL, if set, is this server's own base URL (e.g.
	// https://backup.example.com), used to link back to the job page from
	// a notification. Empty omits the link.
	URL string `json:"url,omitempty"`
}

// NotifarrConfig sends through Notifarr's passthrough integration:
// https://notifiarr.wiki/pages/integrations/passthrough/
type NotifarrConfig struct {
	Enabled bool   `json:"enabled"`
	APIKey  string `json:"api_key"`
	// ChannelID is the Discord channel Notifarr delivers to; required by
	// Notifarr's own API.
	ChannelID int64 `json:"channel_id"`
	// Color is a 6-digit hex code for the Discord embed, e.g. "b3261e".
	// Empty uses Notifarr's own default.
	Color string `json:"color,omitempty"`
}

// DiscordConfig posts straight to a Discord webhook, no Notifarr involved.
type DiscordConfig struct {
	Enabled    bool   `json:"enabled"`
	WebhookURL string `json:"webhook_url"`
}

// EmailConfig sends over SMTP.
type EmailConfig struct {
	Enabled bool   `json:"enabled"`
	Host    string `json:"host"`
	Port    int    `json:"port"`
	// ImplicitTLS: connect with TLS from the start (port 465), instead of
	// starting plaintext and upgrading with STARTTLS if the server offers
	// it (the usual case on 587, and the default here).
	ImplicitTLS bool     `json:"implicit_tls,omitempty"`
	Username    string   `json:"username,omitempty"`
	Password    string   `json:"password,omitempty"`
	From        string   `json:"from"`
	To          []string `json:"to"`
}

func (c *NotifarrConfig) enabled() bool { return c != nil && c.Enabled }
func (c *DiscordConfig) enabled() bool  { return c != nil && c.Enabled }
func (c *EmailConfig) enabled() bool    { return c != nil && c.Enabled }

// Enabled reports whether any channel is configured and on.
func (c Config) Enabled() bool {
	return c.Notifarr.enabled() || c.Discord.enabled() || c.Email.enabled()
}

// Event is what a run's outcome looks like to a notification channel.
type Event struct {
	Job     string // job name
	Kind    string // "backup", "check", "restore"
	Status  string // catalog.RunPartial or catalog.RunFailed; Send doesn't send on success
	Summary string
	Error   string
	JobID   string // for building the link back, if Config.URL is set
}

// Result is one channel's outcome, for callers (like a "send test
// notification" button) that want to report per-channel success/failure.
type Result struct {
	Channel string
	Err     error
}

func (r Result) String() string {
	if r.Err != nil {
		return r.Channel + ": " + r.Err.Error()
	}
	return r.Channel + ": ok"
}

// httpClient and notifarrURL are swappable in tests.
var httpClient = &http.Client{Timeout: 15 * time.Second}

const defaultNotifarrURL = "https://notifiarr.com/api/v1/notification/passthrough/"

var notifarrURL = defaultNotifarrURL

// Send delivers ev to every enabled channel and returns one Result per
// channel attempted (none, if nothing is enabled).
func Send(ctx context.Context, cfg Config, ev Event) []Result {
	var out []Result
	if cfg.Notifarr.enabled() {
		out = append(out, Result{"Notifarr", sendNotifarr(ctx, cfg, ev)})
	}
	if cfg.Discord.enabled() {
		out = append(out, Result{"Discord", sendDiscord(ctx, cfg, ev)})
	}
	if cfg.Email.enabled() {
		out = append(out, Result{"Email", sendEmail(cfg.Email, cfg, ev)})
	}
	return out
}

func (ev Event) title() string {
	return fmt.Sprintf("%s %s: %s", ev.Job, ev.Kind, ev.Status)
}

func (ev Event) body(cfg Config) string {
	var b strings.Builder
	if ev.Summary != "" {
		b.WriteString(ev.Summary)
	}
	if ev.Error != "" {
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(ev.Error)
	}
	if cfg.URL != "" && ev.JobID != "" {
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(strings.TrimRight(cfg.URL, "/") + "/jobs/" + ev.JobID)
	}
	return b.String()
}

func postJSON(ctx context.Context, url string, body any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/plain")
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s: %s", url, resp.Status)
	}
	return nil
}

// sendNotifarr posts to Notifarr's passthrough endpoint. Schema per
// https://notifiarr.wiki/pages/integrations/passthrough/.
func sendNotifarr(ctx context.Context, cfg Config, ev Event) error {
	n := cfg.Notifarr
	payload := map[string]any{
		"notification": map[string]any{
			"name":  "voidgrid-backup",
			"event": ev.Job + "-" + ev.Kind,
		},
		"discord": map[string]any{
			"color": n.Color,
			"ids":   map[string]any{"channel": n.ChannelID},
			"text": map[string]any{
				"title":       ev.title(),
				"description": ev.body(cfg),
			},
		},
	}
	return postJSON(ctx, notifarrURL+n.APIKey, payload)
}

// sendDiscord posts a plain Discord webhook message, no Notifarr involved.
func sendDiscord(ctx context.Context, cfg Config, ev Event) error {
	const (
		colorRed    = 0xb3261e // failed
		colorYellow = 0xe0c050 // partial
	)
	color := colorYellow
	if ev.Status == "failed" {
		color = colorRed
	}
	payload := map[string]any{
		"embeds": []map[string]any{{
			"title":       ev.title(),
			"description": ev.body(cfg),
			"color":       color,
		}},
	}
	return postJSON(ctx, cfg.Discord.WebhookURL, payload)
}

// sendEmail sends a plain-text message over SMTP: implicit TLS or
// opportunistic STARTTLS, then AUTH PLAIN if a username is configured.
func sendEmail(c *EmailConfig, cfg Config, ev Event) error {
	addr := net.JoinHostPort(c.Host, fmt.Sprint(c.Port))
	var conn net.Conn
	var err error
	if c.ImplicitTLS {
		conn, err = tls.Dial("tcp", addr, &tls.Config{ServerName: c.Host})
	} else {
		conn, err = net.Dial("tcp", addr)
	}
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	client, err := smtp.NewClient(conn, c.Host)
	if err != nil {
		conn.Close()
		return fmt.Errorf("smtp handshake: %w", err)
	}
	defer client.Close()

	if !c.ImplicitTLS {
		if ok, _ := client.Extension("STARTTLS"); ok {
			if err := client.StartTLS(&tls.Config{ServerName: c.Host}); err != nil {
				return fmt.Errorf("starttls: %w", err)
			}
		}
	}
	if c.Username != "" {
		if ok, _ := client.Extension("AUTH"); ok {
			if err := client.Auth(smtp.PlainAuth("", c.Username, c.Password, c.Host)); err != nil {
				return fmt.Errorf("auth: %w", err)
			}
		}
	}
	if err := client.Mail(c.From); err != nil {
		return fmt.Errorf("mail from: %w", err)
	}
	for _, to := range c.To {
		if err := client.Rcpt(to); err != nil {
			return fmt.Errorf("rcpt %s: %w", to, err)
		}
	}
	w, err := client.Data()
	if err != nil {
		return fmt.Errorf("data: %w", err)
	}
	msg := fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n%s\r\n",
		c.From, strings.Join(c.To, ", "), ev.title(), ev.body(cfg))
	if _, err := w.Write([]byte(msg)); err != nil {
		w.Close()
		return fmt.Errorf("write body: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("close body: %w", err)
	}
	return client.Quit()
}
