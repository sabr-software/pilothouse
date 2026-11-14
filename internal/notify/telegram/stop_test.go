package telegram

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sabr-software/pilothouse/internal/api"
	"github.com/sabr-software/pilothouse/internal/core"
)

// stopCore lets a test stop the bot while a button press is half handled:
// the first Request after arm waits for release, and fails like any call
// whose context was cancelled.
type stopCore struct {
	core.Core
	armed   atomic.Bool
	entered chan struct{}
	release chan struct{}
}

func (c *stopCore) Request(ctx context.Context, id string) (api.Request, error) {
	if c.armed.CompareAndSwap(true, false) {
		close(c.entered)
		<-c.release
	}
	if err := ctx.Err(); err != nil {
		return api.Request{}, err
	}
	return c.Core.Request(ctx, id)
}

// The daemon restarts the bot when its settings change (the first time
// right after pairing). A press being handled at that moment has already
// spent its button, so it must still answer the agent: otherwise the press
// is lost and the redelivered one finds "This button was already used."
func TestStoppingTheBotKeepsAPressBeingHandled(t *testing.T) {
	sc := &stopCore{entered: make(chan struct{}), release: make(chan struct{})}
	f := newFixture(t, func(o *Options) { sc.Core = o.Core; o.Core = sc })
	r := f.open(api.Request{ID: "r_stop", Checkpoint: api.CheckpointPermission, Tool: "Bash", ToolKind: api.ToolShell, Command: "git push"})
	msg := f.notifyRequest(r)
	deny := button(t, msg.buttons(), "❌ Deny")

	sc.armed.Store(true)
	f.mock.push(press(chatID, chatID, msg.ID, deny))
	ctx, cancel := context.WithCancel(bg)
	done := make(chan struct{})
	go func() { f.bot.Run(ctx); close(done) }()
	select {
	case <-sc.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the press never reached the bot")
	}
	cancel() // the daemon swaps in a new bot and stops this one
	close(sc.release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop")
	}
	if d := f.decision(r.ID); d == nil || d.Behavior != api.BehaviorDeny {
		t.Errorf("the press was lost: decision = %+v, answers = %d", d, f.mock.answerCount())
	}
	if off := f.bot.db.offset(bg); off != 2 {
		t.Errorf("offset = %d: the handled press would be delivered again", off)
	}
}
