package web

import (
	"net/http"

	"github.com/xasdta/pilothouse/internal/api"
	"github.com/xasdta/pilothouse/internal/core"
)

// Open a session on the computer (core.Desktop): a herdr tab or a
// terminal window attached to it.
//
//	GET  /api/desktop                  → api.DesktopInfo ({} when it can't)
//	POST /api/sessions/{id}/desktop    signed "desktop", target = session id, payload {}
//	  → api.DesktopOpened
//
// Opening is signed like any other action: it puts a terminal that can
// type into the agent on the computer's screen. Repeats are cheap: herdr
// reuses the session's tab, and a terminal window opens at most once
// every few seconds per session.

func (s *Server) handleDesktopInfo(w http.ResponseWriter, r *http.Request) {
	dc, ok := s.d.Core.(core.Desktop)
	if !ok {
		writeJSON(w, http.StatusOK, api.DesktopInfo{})
		return
	}
	writeJSON(w, http.StatusOK, dc.DesktopInfo(r.Context()))
}

func (s *Server) handleOpenOnDesktop(w http.ResponseWriter, r *http.Request) {
	dc, ok := s.d.Core.(core.Desktop)
	if !ok {
		writeUnsupported(w, "this computer can't open sessions on its screen")
		return
	}
	id := r.PathValue("id")
	var b struct{}
	if !s.readSigned(w, r, ActionDesktop, id, &b) {
		return
	}
	sess, err := s.d.Core.Session(r.Context(), id)
	if err != nil {
		s.writeCoreErr(w, err)
		return
	}
	if sess.ID != id { // a prefix: signatures name full ids
		writeErr(w, http.StatusNotFound, "not_found", "not found")
		return
	}
	res, err := dc.OpenOnDesktop(r.Context(), id, by(r))
	if err != nil {
		s.writeCoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}
