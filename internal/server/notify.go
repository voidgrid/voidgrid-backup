package server

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/voidgrid/voidgrid-backup/internal/notify"
)

// settingNotify is the catalog settings key notify.Config is stored under.
const settingNotify = "notify"

// NotifyConfig returns the server's notification settings, or a zero
// (disabled) Config if none has been saved yet.
func (c *Controller) NotifyConfig(ctx context.Context) (notify.Config, error) {
	raw, err := c.Catalog.GetSetting(ctx, settingNotify)
	if err != nil || raw == "" {
		return notify.Config{}, err
	}
	var cfg notify.Config
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return notify.Config{}, fmt.Errorf("stored notification settings: %w", err)
	}
	return cfg, nil
}

// SetNotifyConfig saves the server's notification settings.
func (c *Controller) SetNotifyConfig(ctx context.Context, cfg notify.Config) error {
	b, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	return c.Catalog.SetSetting(ctx, settingNotify, string(b))
}
