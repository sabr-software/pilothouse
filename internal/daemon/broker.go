package daemon

import (
	"context"
	"sync"

	"github.com/sabr-software/pilothouse/internal/api"
	"github.com/sabr-software/pilothouse/internal/core"
	"github.com/sabr-software/pilothouse/internal/store"
)

// liveBuffer is how many events a subscriber may fall behind before it is
// dropped and told to resync. Generous, because resync costs a snapshot.
const liveBuffer = 1024

// replayPage is how many logged events are read per query during replay.
const replayPage = 500

// broker fans committed events out to subscribers. publish is called by the
// store's commit hook with the write lock held, so events arrive here in seq
// order and broker.last is always the newest committed seq.
type broker struct {
	db     *store.DB
	mu     sync.Mutex
	last   int64
	subs   map[*subscriber]struct{}
	closed bool
}

type subscriber struct {
	live chan api.Event // closed by the broker when the subscriber lags or shuts down
}

func newBroker(db *store.DB, last int64) *broker {
	return &broker{db: db, last: last, subs: map[*subscriber]struct{}{}}
}

// publish delivers committed events without ever blocking: a subscriber
// whose buffer is full is dropped (its channel closed) and must resync.
func (b *broker) publish(evs []api.Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, ev := range evs {
		b.last = ev.Seq
		for s := range b.subs {
			select {
			case s.live <- ev:
			default:
				b.drop(s)
			}
		}
	}
}

// drop must be called with b.mu held.
func (b *broker) drop(s *subscriber) {
	if _, ok := b.subs[s]; ok {
		delete(b.subs, s)
		close(s.live)
	}
}

func (b *broker) unsubscribe(s *subscriber) {
	b.mu.Lock()
	b.drop(s)
	b.mu.Unlock()
}

// close ends every subscription; clients see their stream end and reload.
func (b *broker) close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	for s := range b.subs {
		b.drop(s)
	}
}

// subscribe returns events with seq > after: first the logged ones (replay),
// then live ones. The subscriber is registered before replay starts, and
// replay stops at the seq that was current at registration, so no event is
// missed or delivered twice. If after is older than retained history, or
// newer than anything this daemon has logged (its database was replaced),
// the subscription comes back with Resync set and C already closed.
//
// C is also closed when the subscriber falls behind or the daemon stops;
// when that happens while the caller's context is still live, the caller
// should treat it as "resync required".
func (b *broker) subscribe(ctx context.Context, after int64) (*core.Subscription, error) {
	pruned, err := b.db.PrunedSeq(ctx)
	if err != nil {
		return nil, err
	}
	b.mu.Lock()
	start := b.last
	if b.closed || after < pruned || after > start || after < 0 {
		b.mu.Unlock()
		c := make(chan api.Event)
		close(c)
		return &core.Subscription{C: c, Resync: true, Close: func() {}}, nil
	}
	s := &subscriber{live: make(chan api.Event, liveBuffer)}
	b.subs[s] = struct{}{}
	b.mu.Unlock()

	ctx, cancel := context.WithCancel(ctx)
	out := make(chan api.Event, 64)
	go b.pump(ctx, s, after, start, out)
	return &core.Subscription{C: out, Close: cancel}, nil
}

func (b *broker) pump(ctx context.Context, s *subscriber, cursor, start int64, out chan<- api.Event) {
	defer close(out)
	defer b.unsubscribe(s)
	send := func(ev api.Event) bool {
		select {
		case out <- ev:
			return true
		case <-ctx.Done():
			return false
		}
	}
	for cursor < start {
		evs, err := b.db.EventsRange(ctx, cursor, start, replayPage)
		if err != nil {
			return // closing C tells the client to resync
		}
		if len(evs) == 0 {
			break // the rest of the range was pruned or is empty
		}
		for _, ev := range evs {
			if !send(ev) {
				return
			}
			cursor = ev.Seq
		}
	}
	for {
		select {
		case ev, ok := <-s.live:
			if !ok {
				return
			}
			if ev.Seq <= cursor {
				continue
			}
			if !send(ev) {
				return
			}
			cursor = ev.Seq
		case <-ctx.Done():
			return
		}
	}
}
