package api

// Desktop targets: where "Open on this computer" shows a wrapped session.
const (
	DesktopHerdr    = "herdr"    // a new tab in the running herdr
	DesktopTerminal = "terminal" // a window of the terminal app (macOS)
)

// DesktopInfo says where the computer would open a session; Target is
// empty when it can't (no herdr running and no terminal app, or turned off).
type DesktopInfo struct {
	Target  string `json:"target,omitempty"`
	Label   string `json:"label,omitempty"`   // "herdr", "Terminal", "iTerm"
	Machine string `json:"machine,omitempty"` // the computer it opens on
}

// DesktopOpened is the result of opening a session on the computer.
type DesktopOpened struct {
	Target string `json:"target"`
	Label  string `json:"label"`
	// Where names the herdr workspace the tab was added to, if known.
	Where string `json:"where,omitempty"`
	// Reused: the session was already open there and was brought forward.
	Reused  bool   `json:"reused,omitempty"`
	Machine string `json:"machine,omitempty"`
}
