package catalog

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Registration states. An agent that presented a valid token stays pending
// until an operator approves or rejects it; an approved row is kept just long
// enough for the agent to collect its certificate.
const (
	RegPending  = "pending"
	RegApproved = "approved"
	RegRejected = "rejected"
)

// Registration is an agent that contacted the registration listener.
type Registration struct {
	ID          string // agent ID the server pre-assigned
	Fingerprint string // hex SHA-256 of the agent's self-signed certificate
	AgentCert   []byte // that certificate (DER); its key gets the issued certificate
	Hostname    string // what the agent calls itself, a name suggestion
	Address     string // suggested host:port the server dials
	Version     string
	CreatedAt   time.Time
	LastPoll    time.Time // zero if never polled
	Status      string
	AgentID     string // final agent ID once approved
	IssuedCert  []byte // agent certificate (DER) once approved
}

const registrationCols = `id, fingerprint, agent_cert, hostname, address, version, created_at, last_poll, status, agent_id, issued_cert`

// RegisterAgent records a registration, or refreshes the details of the one
// already held for r.Fingerprint (its ID, status and decision are kept).
func (c *Catalog) RegisterAgent(ctx context.Context, r Registration) (Registration, error) {
	_, err := c.db.ExecContext(ctx, `INSERT INTO registrations
		(id, fingerprint, agent_cert, hostname, address, version, created_at, last_poll)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (fingerprint) DO UPDATE SET
		  hostname = excluded.hostname, address = excluded.address,
		  version = excluded.version, last_poll = excluded.last_poll`,
		r.ID, r.Fingerprint, r.AgentCert, r.Hostname, r.Address, r.Version,
		formatTime(r.CreatedAt), formatTime(r.CreatedAt))
	if err != nil {
		return Registration{}, err
	}
	return c.RegistrationByFingerprint(ctx, r.Fingerprint)
}

func (c *Catalog) RegistrationByFingerprint(ctx context.Context, fingerprint string) (Registration, error) {
	return scanRegistration(c.db.QueryRowContext(ctx, `SELECT `+registrationCols+` FROM registrations WHERE fingerprint = ?`, fingerprint))
}

func (c *Catalog) RegistrationByID(ctx context.Context, id string) (Registration, error) {
	return scanRegistration(c.db.QueryRowContext(ctx, `SELECT `+registrationCols+` FROM registrations WHERE id = ?`, id))
}

// ListRegistrations returns every registration that still needs attention:
// pending and rejected ones, oldest first. Approved rows are plumbing.
func (c *Catalog) ListRegistrations(ctx context.Context) ([]Registration, error) {
	rows, err := c.db.QueryContext(ctx, `SELECT `+registrationCols+` FROM registrations
		WHERE status != ? ORDER BY created_at`, RegApproved)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Registration
	for rows.Next() {
		r, err := scanRegistration(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// TouchRegistration records that the agent polled at the given time.
func (c *Catalog) TouchRegistration(ctx context.Context, id string, at time.Time) error {
	return c.update(ctx, `UPDATE registrations SET last_poll = ? WHERE id = ?`, formatTime(at), id)
}

// ApproveRegistration stores the decision and the issued certificate for the
// agent to collect. Only a pending registration can be approved.
func (c *Catalog) ApproveRegistration(ctx context.Context, id, agentID string, cert []byte) error {
	return c.update(ctx, `UPDATE registrations SET status = ?, agent_id = ?, issued_cert = ?
		WHERE id = ? AND status = ?`, RegApproved, agentID, cert, id, RegPending)
}

// RejectRegistration marks a pending registration rejected. The row stays so
// the agent is told, instead of registering again as a new one.
func (c *Catalog) RejectRegistration(ctx context.Context, id string) error {
	return c.update(ctx, `UPDATE registrations SET status = ? WHERE id = ? AND status = ?`, RegRejected, id, RegPending)
}

// DeleteRegistration removes a registration in any state.
func (c *Catalog) DeleteRegistration(ctx context.Context, id string) error {
	return c.update(ctx, `DELETE FROM registrations WHERE id = ?`, id)
}

// DeleteUndecidedRegistrations removes every pending and rejected
// registration (used when the registration token is rotated).
func (c *Catalog) DeleteUndecidedRegistrations(ctx context.Context) error {
	_, err := c.db.ExecContext(ctx, `DELETE FROM registrations WHERE status != ?`, RegApproved)
	return err
}

// DeleteApprovedRegistrations removes the approved row(s) for an agent once
// the server has reached it with the issued certificate.
func (c *Catalog) DeleteApprovedRegistrations(ctx context.Context, agentID string) error {
	_, err := c.db.ExecContext(ctx, `DELETE FROM registrations WHERE status = ? AND agent_id = ?`, RegApproved, agentID)
	return err
}

func scanRegistration(s scanner) (Registration, error) {
	var r Registration
	var created string
	var poll sql.NullString
	err := s.Scan(&r.ID, &r.Fingerprint, &r.AgentCert, &r.Hostname, &r.Address, &r.Version,
		&created, &poll, &r.Status, &r.AgentID, &r.IssuedCert)
	if errors.Is(err, sql.ErrNoRows) {
		return Registration{}, ErrNotFound
	}
	if err != nil {
		return Registration{}, err
	}
	if r.CreatedAt, err = time.Parse(time.RFC3339Nano, created); err != nil {
		return Registration{}, err
	}
	if poll.Valid {
		if r.LastPoll, err = time.Parse(time.RFC3339Nano, poll.String); err != nil {
			return Registration{}, err
		}
	}
	return r, nil
}

// RenameAgent changes an agent's display name. The name is only a label: the
// agent's ID is its identity.
func (c *Catalog) RenameAgent(ctx context.Context, id, name string) error {
	return c.update(ctx, `UPDATE agents SET name = ? WHERE id = ?`, name, id)
}

// ReplaceAgent points an existing agent at a freshly approved registration:
// new address and certificate, same ID, jobs and snapshots.
func (c *Catalog) ReplaceAgent(ctx context.Context, id, address, hostname, version, fingerprint string) error {
	return c.update(ctx, `UPDATE agents SET address = ?, hostname = ?, version = ?, cert_fingerprint = ?,
		last_error = '', last_seen = NULL, warnings = '' WHERE id = ?`, address, hostname, version, fingerprint, id)
}
