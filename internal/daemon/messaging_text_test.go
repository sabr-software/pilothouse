package daemon

import (
	"testing"

	"github.com/sabr-software/pilothouse/internal/transcript"
)

// The transcript reader recognises a reply sent as a message and shows
// only what the person wrote: the wording here and there must agree.
func TestMessagingTextRoundTrips(t *testing.T) {
	for _, by := range []string{"device:d_1", "local"} {
		text := "Ship it.\n\nThen tell me the URL."
		got, ok := transcript.UnwrapPilothouse(messagingText(text, by))
		if !ok || got != text {
			t.Errorf("by %s: UnwrapPilothouse = %q, %v; want %q", by, got, ok, text)
		}
	}
}
