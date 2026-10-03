package server

import (
	"net/url"
	"testing"

	"github.com/voidgrid/voidgrid-backup/internal/notify"
)

func TestNotifyConfigFromFormKeepsSecretsWhenBlank(t *testing.T) {
	existing := notify.Config{
		Discord:  &notify.DiscordConfig{Enabled: true, WebhookURL: "https://discord.example/old"},
		Notifarr: &notify.NotifarrConfig{Enabled: true, APIKey: "old-key", ChannelID: 111},
		Email:    &notify.EmailConfig{Enabled: true, Host: "smtp.example", Port: 587, Password: "old-pw", From: "a@x", To: []string{"b@x"}},
	}
	f := url.Values{
		"discord_enabled":  {"on"},                                 // webhook left blank
		"notifarr_enabled": {"on"}, "notifarr_channel_id": {"222"}, // api key left blank, channel changed
		"email_enabled": {"on"}, "email_host": {"smtp.example"}, "email_port": {"587"},
		"email_from": {"a@x"}, "email_to": {"b@x"}, // password left blank
	}
	cfg, err := notifyConfigFromForm(f, existing)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Discord.WebhookURL != "https://discord.example/old" {
		t.Fatalf("discord webhook not kept: %+v", cfg.Discord)
	}
	if cfg.Notifarr.APIKey != "old-key" || cfg.Notifarr.ChannelID != 222 {
		t.Fatalf("notifarr not merged correctly: %+v", cfg.Notifarr)
	}
	if cfg.Email.Password != "old-pw" {
		t.Fatalf("email password not kept: %+v", cfg.Email)
	}
}

func TestNotifyConfigFromFormRequiresSecretOnFirstSave(t *testing.T) {
	f := url.Values{"discord_enabled": {"on"}} // no webhook, no existing config
	if _, err := notifyConfigFromForm(f, notify.Config{}); err == nil {
		t.Fatal("discord enabled with no webhook and nothing stored: accepted")
	}
}

func TestNotifyConfigFromFormDisabledChannelOmitted(t *testing.T) {
	cfg, err := notifyConfigFromForm(url.Values{}, notify.Config{
		Discord: &notify.DiscordConfig{Enabled: true, WebhookURL: "https://discord.example/old"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Discord != nil {
		t.Fatalf("unchecked channel should be dropped, not kept enabled: %+v", cfg.Discord)
	}
}

func TestNotifyConfigFromFormValidatesEmailFields(t *testing.T) {
	base := url.Values{"email_enabled": {"on"}, "email_host": {"smtp.example"}, "email_port": {"587"}, "email_from": {"a@x"}, "email_to": {"b@x"}}
	if _, err := notifyConfigFromForm(url.Values{"email_enabled": {"on"}}, notify.Config{}); err == nil {
		t.Fatal("email with no host/from/to accepted")
	}
	if _, err := notifyConfigFromForm(base, notify.Config{}); err != nil {
		t.Fatalf("valid email config rejected: %v", err)
	}
	bad := url.Values{}
	for k, v := range base {
		bad[k] = v
	}
	bad.Set("email_port", "not-a-number")
	if _, err := notifyConfigFromForm(bad, notify.Config{}); err == nil {
		t.Fatal("non-numeric port accepted")
	}
}
