package daemon

import (
	"sync"
	"testing"
	"time"

	"github.com/sabr-software/pilothouse/internal/api"
)

// The person comes back after a permission hook decided to hold its
// request and before the request is stored. The release pass cannot close
// a request that is not there yet, so the hook has to notice by itself:
// otherwise the agent waits out the whole hold limit with the person
// sitting at the terminal.
func TestPersonBackBeforeHeldRequestIsStored(t *testing.T) {
	h := newHarness(t)
	h.cfg.Approvals.HoldLimit.Duration = 30 * time.Second
	h.presence.setAway(true)
	var once sync.Once
	var d *Daemon
	ready := make(chan struct{})
	h.opts = func(o *Options) {
		o.afterPlan = func() {
			once.Do(func() {
				<-ready
				h.presence.setAway(false)
				deadline := time.Now().Add(5 * time.Second)
				for d.Presence().State != api.PresencePresent && time.Now().Before(deadline) {
					time.Sleep(5 * time.Millisecond)
				}
				// Let the release pass that follows the change finish.
				time.Sleep(100 * time.Millisecond)
			})
		}
	}
	h.start(t)
	d = h.d
	waitFor(t, "away", func() bool { return h.d.Presence().State == api.PresenceAway })
	close(ready)

	result := make(chan fakeOutcome, 1)
	go func() { result <- h.hook(bg, t, permission("npm publish", "")) }()
	select {
	case out := <-result:
		if out.Behavior != "" {
			t.Fatalf("hook answered %+v, want no opinion (the terminal asks)", out)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("request held although the person is back at the desk")
	}
	reqs, err := h.c.Requests(bg, "")
	if err != nil || len(reqs) != 1 {
		t.Fatalf("requests = %+v, %v", reqs, err)
	}
	if reqs[0].Status != api.RequestSteppedAside {
		t.Fatalf("status = %s, want stepped_aside", reqs[0].Status)
	}
}
