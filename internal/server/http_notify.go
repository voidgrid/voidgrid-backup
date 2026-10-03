package server

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/voidgrid/voidgrid-backup/internal/notify"
)

type notificationsPage struct {
	base
	Cfg         notify.Config
	Form        url.Values
	TestResults []notify.Result
	TestError   string
}

func (u *ui) notifications(w http.ResponseWriter, r *http.Request) {
	cfg, err := u.c.NotifyConfig(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	u.render(w, http.StatusOK, "notifications", notificationsPage{
		base: u.newBase("Notifications", "notifications", r), Cfg: cfg, Form: formFromConfig(cfg),
	})
}

// formFromConfig seeds the form with everything except secrets, which stay
// blank: the "leave blank to keep the current value" convention below means
// a blank secret field is never actually "no secret", except on the very
// first save.
func formFromConfig(cfg notify.Config) url.Values {
	f := url.Values{"url": {cfg.URL}}
	if cfg.Discord != nil {
		if cfg.Discord.Enabled {
			f.Set("discord_enabled", "on")
		}
	}
	if cfg.Notifarr != nil {
		if cfg.Notifarr.Enabled {
			f.Set("notifarr_enabled", "on")
		}
		f.Set("notifarr_channel_id", formatInt64(cfg.Notifarr.ChannelID))
		f.Set("notifarr_color", cfg.Notifarr.Color)
	} else {
		f.Set("notifarr_color", "b3261e")
	}
	if cfg.Email != nil {
		if cfg.Email.Enabled {
			f.Set("email_enabled", "on")
		}
		f.Set("email_host", cfg.Email.Host)
		f.Set("email_port", strconv.Itoa(cfg.Email.Port))
		if cfg.Email.ImplicitTLS {
			f.Set("email_implicit_tls", "on")
		}
		f.Set("email_username", cfg.Email.Username)
		f.Set("email_from", cfg.Email.From)
		f.Set("email_to", strings.Join(cfg.Email.To, ", "))
	} else {
		f.Set("email_port", "587")
	}
	return f
}

func formatInt64(n int64) string {
	if n == 0 {
		return ""
	}
	return strconv.FormatInt(n, 10)
}

func (u *ui) saveNotifications(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	existing, err := u.c.NotifyConfig(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	cfg, err := notifyConfigFromForm(r.PostForm, existing)
	if err != nil {
		p := notificationsPage{base: u.newBase("Notifications", "notifications", r), Cfg: existing, Form: r.PostForm}
		p.Error = err.Error()
		u.render(w, http.StatusBadRequest, "notifications", p)
		return
	}
	if err := u.c.SetNotifyConfig(ctx, cfg); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	redirectNotice(w, r, "/notifications", "Notification settings saved.")
}

// notifyConfigFromForm builds a notify.Config from submitted form values. A
// blank secret field (webhook URL, API key, SMTP password) keeps whatever
// was already saved for that channel, so re-saving other fields never
// requires retyping it; the way to actually change one is to type a new
// value, and the way to turn a channel off is to uncheck it, not blank its
// secret.
func notifyConfigFromForm(f url.Values, existing notify.Config) (notify.Config, error) {
	cfg := notify.Config{URL: strings.TrimSpace(f.Get("url"))}

	if f.Get("discord_enabled") != "" {
		webhook := strings.TrimSpace(f.Get("discord_webhook_url"))
		if webhook == "" && existing.Discord != nil {
			webhook = existing.Discord.WebhookURL
		}
		if webhook == "" {
			return cfg, errors.New("discord: webhook URL is required")
		}
		cfg.Discord = &notify.DiscordConfig{Enabled: true, WebhookURL: webhook}
	}

	if f.Get("notifarr_enabled") != "" {
		apiKey := strings.TrimSpace(f.Get("notifarr_api_key"))
		if apiKey == "" && existing.Notifarr != nil {
			apiKey = existing.Notifarr.APIKey
		}
		if apiKey == "" {
			return cfg, errors.New("notifarr: API key is required")
		}
		channelID, err := strconv.ParseInt(strings.TrimSpace(f.Get("notifarr_channel_id")), 10, 64)
		if err != nil || channelID == 0 {
			return cfg, errors.New("notifarr: a numeric Discord channel ID is required")
		}
		cfg.Notifarr = &notify.NotifarrConfig{
			Enabled: true, APIKey: apiKey, ChannelID: channelID,
			Color: strings.TrimPrefix(strings.TrimSpace(f.Get("notifarr_color")), "#"),
		}
	}

	if f.Get("email_enabled") != "" {
		port, err := strconv.Atoi(strings.TrimSpace(f.Get("email_port")))
		if err != nil {
			return cfg, errors.New("email: port must be a number")
		}
		password := f.Get("email_password")
		if password == "" && existing.Email != nil {
			password = existing.Email.Password
		}
		var to []string
		for _, addr := range strings.Split(f.Get("email_to"), ",") {
			if addr = strings.TrimSpace(addr); addr != "" {
				to = append(to, addr)
			}
		}
		if strings.TrimSpace(f.Get("email_host")) == "" || strings.TrimSpace(f.Get("email_from")) == "" || len(to) == 0 {
			return cfg, errors.New("email: host, from address and at least one recipient are required")
		}
		cfg.Email = &notify.EmailConfig{
			Enabled: true, Host: strings.TrimSpace(f.Get("email_host")), Port: port,
			ImplicitTLS: f.Get("email_implicit_tls") != "", Username: strings.TrimSpace(f.Get("email_username")),
			Password: password, From: strings.TrimSpace(f.Get("email_from")), To: to,
		}
	}
	return cfg, nil
}

func (u *ui) testNotifications(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	cfg, err := u.c.NotifyConfig(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	p := notificationsPage{base: u.newBase("Notifications", "notifications", r), Cfg: cfg, Form: formFromConfig(cfg)}
	if !cfg.Enabled() {
		p.TestError = "No channel is enabled yet."
		u.render(w, http.StatusOK, "notifications", p)
		return
	}
	p.TestResults = notify.Send(ctx, cfg, notify.Event{
		Job: "test", Kind: "test", Status: "failed",
		Summary: "This is a test notification from voidgrid-backup.",
	})
	u.render(w, http.StatusOK, "notifications", p)
}
