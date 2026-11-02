package web

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/sabr-software/pilothouse/internal/api"
	"github.com/sabr-software/pilothouse/internal/core"
)

// Storage for linked machines (link.go). Tokens are random 256-bit
// secrets: each side stores the token it sends in full (it must present
// it) and only a hash of the token it receives.

// linkRow is a machine linked to this one (hub side).
type linkRow struct {
	ID        string
	Machine   string
	URL       string
	PeerToken string
	CreatedAt time.Time
}

// hubRow is the hub this machine reports to (linked side).
type hubRow struct {
	ID        string
	Machine   string
	URL       string
	HubToken  string
	CreatedAt time.Time
}

// linkDeviceIn is a hub device as the hub sends it.
type linkDeviceIn struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	PublicKey string     `json:"public_key"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
}

var errNoLink = errors.New("no such link")

// errStaleDevices: a device list older than the one applied (its explicit
// revocations were still applied). The hub resends with a newer version.
var errStaleDevices = errors.New("stale device list")

// maxDevVersionAhead is how far a hub's clock may run ahead of this one.
const maxDevVersionAhead = 24 * time.Hour

// devicesVersion is the newest device-list version applied here.
func (s *Server) devicesVersion(ctx context.Context) int64 {
	var v int64
	_ = s.d.DB.QueryRowContext(ctx, `SELECT devices_version FROM web_link_hub`).Scan(&v)
	return v
}

func newLinkID() string { return "l_" + idEncoding.EncodeToString(randomBytes(7))[:10] }

// keyFingerprint is a short, comparable form of a public key: 64 bits,
// too many to find a lookalike key for.
func keyFingerprint(pub string) string {
	h := sha256.Sum256([]byte(pub))
	x := hex.EncodeToString(h[:8])
	return x[0:4] + " " + x[4:8] + " " + x[8:12] + " " + x[12:16]
}

// ---- hub side ----

func (s *Server) insertLink(ctx context.Context, l linkRow, hubToken string) error {
	_, err := s.d.DB.ExecContext(ctx, `INSERT INTO web_links (id, machine, url, peer_token, hub_token_hash, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		l.ID, l.Machine, l.URL, l.PeerToken, hashSecret(hubToken), ms(l.CreatedAt))
	return err
}

func (s *Server) listLinks(ctx context.Context) ([]linkRow, error) {
	rows, err := s.d.DB.QueryContext(ctx, `SELECT id, machine, url, peer_token, created_at FROM web_links ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []linkRow
	for rows.Next() {
		var l linkRow
		var created int64
		if err := rows.Scan(&l.ID, &l.Machine, &l.URL, &l.PeerToken, &created); err != nil {
			return nil, err
		}
		l.CreatedAt = time.UnixMilli(created).UTC()
		out = append(out, l)
	}
	return out, rows.Err()
}

// linkByToken finds the linked machine presenting token (hub side).
func (s *Server) linkByToken(ctx context.Context, token string) (*linkRow, error) {
	var l linkRow
	var created int64
	err := s.d.DB.QueryRowContext(ctx, `SELECT id, machine, url, peer_token, created_at FROM web_links WHERE hub_token_hash = ?`,
		hashSecret(token)).Scan(&l.ID, &l.Machine, &l.URL, &l.PeerToken, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errNoLink
	}
	if err != nil {
		return nil, err
	}
	l.CreatedAt = time.UnixMilli(created).UTC()
	return &l, nil
}

func (s *Server) deleteLink(ctx context.Context, id string) error {
	res, err := s.d.DB.ExecContext(ctx, `DELETE FROM web_links WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errNoLink
	}
	return nil
}

// ---- linked side ----

// setHub replaces the hub this machine reports to, and forgets the old
// hub's devices.
func (s *Server) setHub(ctx context.Context, h hubRow, peerToken string) error {
	tx, err := s.d.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, q := range []string{`DELETE FROM web_link_hub`, `DELETE FROM web_link_devices`} {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO web_link_hub (id, machine, url, hub_token, peer_token_hash, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		h.ID, h.Machine, h.URL, h.HubToken, hashSecret(peerToken), ms(h.CreatedAt)); err != nil {
		return err
	}
	return tx.Commit()
}

// hub returns the hub this machine reports to, or nil.
func (s *Server) hub(ctx context.Context) (*hubRow, error) {
	var h hubRow
	var created int64
	err := s.d.DB.QueryRowContext(ctx, `SELECT id, machine, url, hub_token, created_at FROM web_link_hub LIMIT 1`).
		Scan(&h.ID, &h.Machine, &h.URL, &h.HubToken, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	h.CreatedAt = time.UnixMilli(created).UTC()
	return &h, nil
}

// hubByToken returns the hub if token is the one it sends here.
func (s *Server) hubByToken(ctx context.Context, token string) (*hubRow, error) {
	var h hubRow
	var created int64
	err := s.d.DB.QueryRowContext(ctx, `SELECT id, machine, url, hub_token, created_at FROM web_link_hub WHERE peer_token_hash = ?`,
		hashSecret(token)).Scan(&h.ID, &h.Machine, &h.URL, &h.HubToken, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errNoLink
	}
	if err != nil {
		return nil, err
	}
	h.CreatedAt = time.UnixMilli(created).UTC()
	return &h, nil
}

func (s *Server) deleteHub(ctx context.Context) error {
	tx, err := s.d.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, q := range []string{`DELETE FROM web_link_hub`, `DELETE FROM web_link_devices`} {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// syncLinkDevices records the hub's device list: new devices and new
// keys start untrusted; devices missing from the list count as revoked.
// It never trusts anything by itself. A list older than one already
// applied (version) is ignored, so a revoked device or an old key can't
// come back when lists arrive out of order.
func (s *Server) syncLinkDevices(ctx context.Context, ds []linkDeviceIn, version int64) error {
	// Versions are the hub's clock in microseconds. One far ahead of this
	// machine's would freeze the list (every later one looks stale).
	if version > s.now().Add(maxDevVersionAhead).UnixMicro() {
		return core.Invalid("device list version is in the future — check the hub's clock")
	}
	tx, err := s.d.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	now := ms(s.now())
	res, err := tx.ExecContext(ctx, `UPDATE web_link_hub SET devices_version = ? WHERE devices_version < ?`, version, version)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// Stale (or no hub). A revocation in it still applies: revoking
		// is never undone, so its order doesn't matter.
		for _, d := range ds {
			if d.RevokedAt != nil {
				if _, err := tx.ExecContext(ctx, `UPDATE web_link_devices SET revoked_at = COALESCE(revoked_at, ?), updated_at = ? WHERE id = ?`, ms(*d.RevokedAt), now, d.ID); err != nil {
					return err
				}
			}
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		return errStaleDevices
	}
	seen := make([]any, 0, len(ds))
	for _, d := range ds {
		if !strings.HasPrefix(d.ID, "d_") || len(d.ID) > 32 || d.PublicKey == "" {
			continue
		}
		var revoked any
		if d.RevokedAt != nil {
			revoked = ms(*d.RevokedAt)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO web_link_devices (id, name, public_key, revoked_at, updated_at) VALUES (?, ?, ?, ?, ?)
			ON CONFLICT(id) DO UPDATE SET name = excluded.name, public_key = excluded.public_key,
				revoked_at = COALESCE(web_link_devices.revoked_at, excluded.revoked_at), updated_at = excluded.updated_at`,
			d.ID, cleanName(d.Name), d.PublicKey, revoked, now); err != nil {
			return err
		}
		seen = append(seen, d.ID)
	}
	q := `UPDATE web_link_devices SET revoked_at = ?, updated_at = ? WHERE revoked_at IS NULL`
	args := []any{now, now}
	if len(seen) > 0 {
		q += ` AND id NOT IN (?` + strings.Repeat(", ?", len(seen)-1) + `)`
		args = append(args, seen...)
	}
	if _, err := tx.ExecContext(ctx, q, args...); err != nil {
		return err
	}
	return tx.Commit()
}

// trustLinkDevices confirms the keys a person was shown: a device is
// trusted only while its key still has the fingerprint they saw, so a key
// that changed in between stays untrusted. It returns how many.
func (s *Server) trustLinkDevices(ctx context.Context, want []api.LinkTrustDevice) (int, error) {
	tx, err := s.d.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	n := 0
	for _, w := range want {
		var pub, trusted string
		err := tx.QueryRowContext(ctx, `SELECT public_key, trusted_key FROM web_link_devices WHERE id = ? AND revoked_at IS NULL`, w.ID).Scan(&pub, &trusted)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return 0, err
		}
		if pub == trusted || keyFingerprint(pub) != w.Fingerprint {
			continue
		}
		res, err := tx.ExecContext(ctx, `UPDATE web_link_devices SET trusted_key = ?, updated_at = ? WHERE id = ? AND public_key = ?`, pub, ms(s.now()), w.ID, pub)
		if err != nil {
			return 0, err
		}
		if k, _ := res.RowsAffected(); k == 1 {
			n++
		}
	}
	return n, tx.Commit()
}

func (s *Server) listLinkDevices(ctx context.Context) ([]api.LinkDevice, error) {
	rows, err := s.d.DB.QueryContext(ctx, `SELECT id, name, public_key, trusted_key, revoked_at FROM web_link_devices ORDER BY name, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []api.LinkDevice{}
	for rows.Next() {
		var d api.LinkDevice
		var pub, trusted string
		var revoked sql.NullInt64
		if err := rows.Scan(&d.ID, &d.Name, &pub, &trusted, &revoked); err != nil {
			return nil, err
		}
		d.Fingerprint = keyFingerprint(pub)
		d.Trusted = trusted == pub
		d.RevokedAt = fromMS(revoked)
		out = append(out, d)
	}
	return out, rows.Err()
}

// linkDevice loads a hub device as a deviceRow acting through the link.
// A revoked or unknown device is errNoDevice.
func (s *Server) linkDevice(ctx context.Context, id, via string) (*deviceRow, error) {
	var d deviceRow
	var pub, trusted string
	var revoked sql.NullInt64
	err := s.d.DB.QueryRowContext(ctx, `SELECT id, name, public_key, trusted_key, revoked_at FROM web_link_devices WHERE id = ?`, id).
		Scan(&d.ID, &d.Name, &pub, &trusted, &revoked)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && revoked.Valid) {
		return nil, errNoDevice
	}
	if err != nil {
		return nil, err
	}
	d.PublicKey = pub
	d.via = via
	d.untrusted = trusted != pub
	return &d, nil
}
