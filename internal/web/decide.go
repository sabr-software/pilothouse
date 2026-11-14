package web

import (
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"database/sql"
	"errors"
	"math/big"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/sabr-software/pilothouse/internal/api"
)

const (
	// SignatureContext prefixes every signed decision so a signature can't
	// be replayed as anything else.
	SignatureContext = "pilothouse-decide:v1"
	// MaxClockSkew is how far a decision's ts may be from the daemon clock.
	MaxClockSkew = 120 * time.Second
	maxMessage   = 2000
)

// CanonicalDecision is the exact byte string a device signs:
//
//	pilothouse-decide:v1\n{id}\n{behavior}\n{scope}\n{ts}\n{nonce}
//
// ts is Unix milliseconds (JavaScript Date.now()); scope is "once" or
// "session" (empty is normalized to "once" before signing). There is no
// message in it, so a v1 decision carries none (the signed /respond does).
func CanonicalDecision(id string, b api.Behavior, sc api.Scope, ts int64, nonce string) string {
	if sc == "" {
		sc = api.ScopeOnce
	}
	return SignatureContext + "\n" + id + "\n" + string(b) + "\n" + string(sc) + "\n" + strconv.FormatInt(ts, 10) + "\n" + nonce
}

// VerifyP1363 checks an ECDSA P-256/SHA-256 signature in IEEE P1363 form
// (r||s, 64 bytes), which is what WebCrypto's sign() returns.
func VerifyP1363(pub *ecdsa.PublicKey, msg, sig []byte) bool {
	if len(sig) != 64 {
		return false
	}
	h := sha256.Sum256(msg)
	r := new(big.Int).SetBytes(sig[:32])
	s := new(big.Int).SetBytes(sig[32:])
	return ecdsa.Verify(pub, h[:], r, s)
}

var nonceRE = regexp.MustCompile(`^[A-Za-z0-9_-]{16,64}$`)

type decideBody struct {
	Behavior api.Behavior `json:"behavior"`
	Scope    api.Scope    `json:"scope,omitempty"`
	Message  string       `json:"message,omitempty"`
	TS       int64        `json:"ts"`
	Nonce    string       `json:"nonce"`
	Sig      string       `json:"sig"`
}

func (s *Server) handleDecide(w http.ResponseWriter, r *http.Request) {
	d := deviceOf(r)
	id := r.PathValue("id")
	var b decideBody
	if !decodeBody(w, r, &b) {
		return
	}
	if b.Scope == "" {
		b.Scope = api.ScopeOnce
	}
	switch {
	case b.Behavior != api.BehaviorAllow && b.Behavior != api.BehaviorDeny:
		writeErr(w, http.StatusBadRequest, "bad_request", "behavior must be allow or deny")
		return
	case b.Scope != api.ScopeOnce && b.Scope != api.ScopeSession:
		writeErr(w, http.StatusBadRequest, "bad_request", "scope must be once or session")
		return
	case b.Scope == api.ScopeSession && b.Behavior != api.BehaviorAllow:
		writeErr(w, http.StatusBadRequest, "bad_request", "only allow can apply to the whole session")
		return
	case strings.TrimSpace(b.Message) != "":
		// The v1 signature does not cover a message, and a message is
		// handed to the agent as the person's words.
		writeErr(w, http.StatusBadRequest, "bad_request", "a message must be signed: send it through /respond")
		return
	case !nonceRE.MatchString(b.Nonce):
		writeErr(w, http.StatusBadRequest, "bad_request", "nonce must be 16-64 base64url characters")
		return
	}
	if !s.verifyDeviceSig(w, r, d, CanonicalDecision(id, b.Behavior, b.Scope, b.TS, b.Nonce), b.Sig, b.TS, b.Nonce) {
		return
	}
	if !s.wholeRequest(w, r, id) {
		return
	}
	now := s.now()
	req, err := s.d.Core.Decide(r.Context(), api.Decision{
		RequestID: id,
		Behavior:  b.Behavior,
		Scope:     b.Scope,
		By:        "device:" + d.ID,
		At:        now.UTC(),
	})
	if err != nil {
		s.writeCoreErr(w, err)
		return
	}
	s.log.Info("request decided", "request", id, "behavior", b.Behavior, "scope", b.Scope, "by", "device:"+d.ID)
	writeJSON(w, http.StatusOK, req)
}

// DenyURL is a one-time URL that refuses a request with one POST (the Deny
// button on an ntfy notification). Empty if no public URL is configured.
// The token is 32 random bytes, stored hashed, valid until the request
// expires. There is no such link for allowing: whoever holds the URL can use
// it, and an approval is signed by a paired device.
func (s *Server) DenyURL(requestID string) string {
	pub := s.PublicURL()
	if pub == "" || requestID == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := s.d.Core.Request(ctx, requestID)
	if err != nil {
		s.log.Warn("deny url: load request", "request", requestID, "err", err)
		return ""
	}
	exp := req.ExpiresAt
	if exp.IsZero() || exp.Before(s.now()) {
		return ""
	}
	tok := randomToken(32)
	if _, err := s.d.DB.ExecContext(ctx, `INSERT INTO web_action_tokens (token_hash, request_id, behavior, expires_at) VALUES (?, ?, ?, ?)`,
		hashSecret(tok), requestID, string(api.BehaviorDeny), ms(exp)); err != nil {
		s.log.Warn("deny url: store token", "err", err)
		return ""
	}
	return pub + "/api/actions/" + tok
}

// handleAction refuses a request from an ntfy Deny button. The token is the
// credential (a capability URL); there is no cookie or Origin, so it can
// only deny. A token stored as "allow" by an older version is never spent.
func (s *Server) handleAction(w http.ResponseWriter, r *http.Request) {
	tok := r.PathValue("token")
	if len(tok) < 40 || len(tok) > 64 {
		writeErr(w, http.StatusNotFound, "invalid_token", "unknown action")
		return
	}
	now := ms(s.now())
	tx, err := s.d.DB.BeginTx(r.Context(), nil)
	if err != nil {
		s.writeCoreErr(w, err)
		return
	}
	defer func() { _ = tx.Rollback() }()
	var reqID string
	err = tx.QueryRowContext(r.Context(),
		`UPDATE web_action_tokens SET used_at = ? WHERE token_hash = ? AND behavior = ? AND used_at IS NULL AND expires_at > ? RETURNING request_id`,
		now, hashSecret(tok), string(api.BehaviorDeny), now).Scan(&reqID)
	if errors.Is(err, sql.ErrNoRows) {
		writeErr(w, http.StatusGone, "invalid_token", "this action was already used or has expired")
		return
	}
	if err != nil {
		s.writeCoreErr(w, err)
		return
	}
	if _, err := tx.ExecContext(r.Context(), `UPDATE web_action_tokens SET used_at = ? WHERE request_id = ? AND used_at IS NULL`, now, reqID); err != nil {
		s.writeCoreErr(w, err)
		return
	}
	if err := tx.Commit(); err != nil {
		s.writeCoreErr(w, err)
		return
	}
	req, err := s.d.Core.Decide(r.Context(), api.Decision{
		RequestID: reqID,
		Behavior:  api.BehaviorDeny,
		Scope:     api.ScopeOnce,
		By:        "ntfy",
		At:        s.now().UTC(),
	})
	if err != nil {
		s.writeCoreErr(w, err)
		return
	}
	s.log.Info("request decided", "request", reqID, "behavior", api.BehaviorDeny, "by", "ntfy")
	writeJSON(w, http.StatusOK, req)
}
