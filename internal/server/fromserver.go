package server

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/voidgrid/voidgrid-backup/internal/catalog"
)

// repoServerCreatedKey marks a repository the server initialised itself. No
// agent has opened it yet, so none counts as assigned until one is tested with
// Assign an agent; the legacy rule that lets the first agent through untested
// does not apply to it.
func repoServerCreatedKey(repoID string) string { return "repo_server_created:" + repoID }

func (c *Controller) serverCreated(ctx context.Context, repoID string) bool {
	v, _ := c.Catalog.GetSetting(ctx, repoServerCreatedKey(repoID))
	return v != ""
}

// initFromServer creates (or connects to) the repository r through the
// server's own engine, for storage that only the server can reach. The server
// becomes its maintenance owner and maintains it; no agent is assigned, so one
// must be assigned (which opens the repository from that agent) before a job
// can use it. The record is removed again if the server cannot open it.
func (c *Controller) initFromServer(ctx context.Context, r catalog.Repository, generated string) (catalog.Repository, string, error) {
	er, err := serverRepo(r)
	if err != nil {
		return catalog.Repository{}, "", fmt.Errorf("the server cannot open this storage: %w", err)
	}
	if err := c.Catalog.AddRepository(ctx, r); err != nil {
		return catalog.Repository{}, "", err
	}
	ictx, cancel := context.WithTimeout(ctx, initTimeout)
	defer cancel()
	_, knownHosts, err := c.Engine.Init(ictx, er)
	if err != nil {
		if derr := c.Catalog.DeleteRepository(ctx, r.ID); derr != nil {
			slog.Warn("remove failed repository record", "repo", r.ID, "err", derr)
		}
		return catalog.Repository{}, "", fmt.Errorf("initialize on the server: %w", err)
	}
	if knownHosts != "" && r.Config.SFTP != nil {
		r.Config.SFTP.KnownHosts = knownHosts
	}
	// The repository exists in storage now: a generated password goes back with
	// any later error, since nothing else has it.
	r.InitializedAt = time.Now()
	if err := c.Catalog.MarkRepositoryInitialized(ctx, r.ID, r.Config, r.InitializedAt); err != nil {
		return r, generated, fmt.Errorf("the repository was created at %s, but recording it here failed: %w", r.Config.Location(), err)
	}
	if err := c.Catalog.SetSetting(ctx, repoServerCreatedKey(r.ID), "1"); err != nil {
		return r, generated, err
	}
	// Init made the server the Kopia maintenance owner, so the server maintains it.
	if err := c.Catalog.SetSetting(ctx, repoMaintByKey(r.ID), MaintByServer); err != nil {
		return r, generated, err
	}
	slog.Info("repository created through the server", "repo", r.ID, "name", r.Name)
	return r, generated, nil
}
