package api

import "time"

// Linking machines (`pilothouse link`): one machine, the hub, shows the
// sessions of the machines linked to it, and forwards what a paired
// device does with them. Each linked machine checks the device's
// signature itself, against keys a person confirmed on that machine.

// MachineInfo is one machine in the hub's view.
type MachineInfo struct {
	Name     string     `json:"name"`
	Self     bool       `json:"self,omitempty"` // the machine serving this view (the hub)
	Online   bool       `json:"online"`         // reachable right now
	Version  string     `json:"version,omitempty"`
	LastSeen *time.Time `json:"last_seen,omitempty"`
	Error    string     `json:"error,omitempty"` // why it's offline, for a person
	Removed  bool       `json:"removed,omitempty"`
}

// LinkCode starts linking: run `pilothouse link join <URL> <Code>` on the
// other machine.
type LinkCode struct {
	Code      string    `json:"code"`
	URL       string    `json:"url"` // this machine's web URL ("" until remote access is on)
	ExpiresAt time.Time `json:"expires_at"`
}

// LinkJoin is what `pilothouse link join` asks the local daemon to do.
type LinkJoin struct {
	URL  string `json:"url"`  // the hub's web URL
	Code string `json:"code"` // from `pilothouse link add` on the hub
	// SelfURL overrides this machine's web URL (tests); default
	// web.public_url.
	SelfURL string `json:"self_url,omitempty"`
}

// LinkDevice is a device paired with the hub, as a linked machine sees it.
type LinkDevice struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	Fingerprint string     `json:"fingerprint"` // short hash of its public key, to compare
	Trusted     bool       `json:"trusted"`     // a person confirmed this key on this machine
	RevokedAt   *time.Time `json:"revoked_at,omitempty"`
}

// LinkPeer is the other end of a link.
type LinkPeer struct {
	ID       string     `json:"id"`
	Machine  string     `json:"machine"`
	URL      string     `json:"url"`
	Online   bool       `json:"online"`
	Version  string     `json:"version,omitempty"`
	LastSeen *time.Time `json:"last_seen,omitempty"`
	Error    string     `json:"error,omitempty"`
	Since    time.Time  `json:"since"`
}

// LinkStatus is `pilothouse link ls`: the hub this machine reports to (if
// any), the machines linked to this one, and the hub's devices.
type LinkStatus struct {
	Machine string       `json:"machine"`
	URL     string       `json:"url,omitempty"`
	Hub     *LinkPeer    `json:"hub,omitempty"`
	Linked  []LinkPeer   `json:"linked,omitempty"`
	Devices []LinkDevice `json:"devices,omitempty"` // only on a linked machine
}

// LinkJoined answers a join: the hub, and its devices to confirm.
type LinkJoined struct {
	Hub     LinkPeer     `json:"hub"`
	Devices []LinkDevice `json:"devices"`
}

// LinkTrust trusts the hub's devices on this machine (empty: every
// device not yet trusted).
type LinkTrust struct {
	// Devices are the devices to trust, each with the key fingerprint the
	// person was shown: a key that has changed since isn't trusted.
	Devices []LinkTrustDevice `json:"devices"`
}

// LinkTrustDevice is one device to trust and the fingerprint shown for it.
type LinkTrustDevice struct {
	ID          string `json:"id"`
	Fingerprint string `json:"fingerprint"`
}
