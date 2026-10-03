package events

import (
	"fmt"
	"testing"
	"time"
)

// TestSubscribeReplayIsBoundedByQueueDepth is a regression test for a deadlock.
//
// Subscribe replays retained events into the new subscriber's channel while
// holding the bus lock. The replay ring is normally far larger than a
// subscriber's queue, so a client that reconnects after a long gap can owe more
// events than the channel holds. When the send was unbounded, the subscriber
// blocked the writer, and because the bus lock was held, every Publish in the
// process blocked behind it — one phone reconnecting froze event delivery for
// the whole gateway.
//
// The test hangs if the bug returns, which the package timeout turns into a
// failure rather than a mystery.
func TestSubscribeReplayIsBoundedByQueueDepth(t *testing.T) {
	const (
		replayDepth = 1024
		queueDepth  = 16
		published   = 100
	)

	bus := New(Config{Replay: replayDepth, Queue: queueDepth})
	for i := 0; i < published; i++ {
		bus.Publish(TypeSessionMessage, "s1", map[string]any{"n": i})
	}

	done := make(chan struct{})
	var resumed Resume
	go func() {
		defer close(done)
		// A cursor at 1 asks for every event after the first: 99 of them, far
		// more than the channel can hold.
		resumed = bus.Subscribe(Cursor{Generation: bus.Generation(), Seq: 1}, nil)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Subscribe blocked: the replay batch is not bounded by the queue depth")
	}
	sub := resumed.Subscription
	defer sub.Close()

	if resumed.Resumable() {
		t.Error("the resume was reported as complete, but the replay was truncated; the " +
			"client would render an incomplete stream with no indication anything was lost")
	}

	// The retained events must be the newest ones, because that is what the
	// client's view of the present depends on.
	var got []Event
	for {
		select {
		case e := <-sub.Events():
			got = append(got, e)
			continue
		default:
		}
		break
	}

	if len(got) == 0 {
		t.Fatal("no events were replayed")
	}
	if len(got) > queueDepth {
		t.Errorf("replayed %d events into a channel of depth %d", len(got), queueDepth)
	}
	if want := uint64(published); got[len(got)-1].Seq != want {
		t.Errorf("newest replayed seq = %d, want %d; the oldest events were kept instead "+
			"of the newest", got[len(got)-1].Seq, want)
	}
	for i := 1; i < len(got); i++ {
		if got[i].Seq != got[i-1].Seq+1 {
			t.Errorf("replay gap between seq %d and %d", got[i-1].Seq, got[i].Seq)
		}
	}
}

// TestSubscribeReplaysRetainedEvents covers the normal case: enough room, so the
// client gets exactly what it asked for and is not told anything is missing.
func TestSubscribeReplaysRetainedEvents(t *testing.T) {
	bus := New(Config{Replay: 64, Queue: 64})
	for i := 0; i < 10; i++ {
		bus.Publish(TypeSessionMessage, "s1", map[string]any{"n": i})
	}

	resumed := bus.Subscribe(Cursor{Generation: bus.Generation(), Seq: 3}, nil)
	sub := resumed.Subscription
	defer sub.Close()

	if !resumed.Resumable() {
		t.Errorf("resume reported %q, but every requested event was still retained",
			resumed.Reason)
	}
	if resumed.From != 3 {
		t.Errorf("resumed from %d, want 3", resumed.From)
	}

	var seqs []uint64
	for {
		select {
		case e := <-sub.Events():
			seqs = append(seqs, e.Seq)
			continue
		default:
		}
		break
	}
	// Events 4..10 inclusive.
	if len(seqs) != 7 {
		t.Fatalf("replayed %d events, want 7: %v", len(seqs), seqs)
	}
	if seqs[0] != 4 {
		t.Errorf("first replayed seq = %d, want 4 (events at or below the cursor must be skipped)", seqs[0])
	}
}

// TestSubscribeReportsCursorOlderThanRing is the honest-degradation case: the
// client asked for events the bus no longer has, so it must be told rather than
// silently handed a partial stream.
func TestSubscribeReportsCursorOlderThanRing(t *testing.T) {
	bus := New(Config{Replay: 8, Queue: 8})
	for i := 0; i < 50; i++ {
		bus.Publish(TypeSessionMessage, "s1", nil)
	}

	resumed := bus.Subscribe(Cursor{Generation: bus.Generation(), Seq: 1}, nil)
	defer resumed.Subscription.Close()

	if resumed.Resumable() {
		t.Error("the resume was reported as complete, but the requested cursor fell off the ring")
	}
	if resumed.Reason != ResyncWindowExceeded {
		t.Errorf("reason = %q, want %q", resumed.Reason, ResyncWindowExceeded)
	}
}

// TestSubscribeFencesACursorFromAnotherGeneration is the regression test for the
// silent failure this contract exists to prevent.
//
// The sequence space belongs to one run of the process. A client that was on
// `seq: 900` when the gateway restarted reconnects with the cursor it holds and
// no generation, or with a generation that is no longer ours. The old code
// asked "is this cursor older than what I still hold?" — a question whose answer
// is no — and handed back a connection that looked healthy while delivering
// frames the client would discard as stale, because every new sequence number is
// lower than the one it bookmarked. The client never learned to refetch.
//
// Every unusable cursor must now produce the same answer: a resync, with a
// reason, and a snapshot.
func TestSubscribeFencesACursorFromAnotherGeneration(t *testing.T) {
	first := New(Config{Replay: 64, Queue: 64})
	for i := 0; i < 900; i++ {
		first.Publish(TypeSessionMessage, "s1", nil)
	}

	// The same client's cursor, against the process that replaced the one it was
	// talking to.
	restarted := New(Config{Replay: 64, Queue: 64, Generation: "second-run"})

	cases := []struct {
		name   string
		cursor Cursor
		want   ResyncReason
	}{
		{
			name:   "a cursor from the previous generation",
			cursor: Cursor{Generation: first.Generation(), Seq: 900},
			want:   ResyncGenerationChanged,
		},
		{
			name:   "a stale cursor with no generation at all",
			cursor: Cursor{Seq: 900},
			want:   ResyncAheadOfStream,
		},
		{
			name:   "a first connection",
			cursor: Cursor{},
			want:   ResyncNoCursor,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resumed := restarted.Subscribe(tc.cursor, nil)
			defer resumed.Subscription.Close()
			if resumed.Resumable() {
				t.Fatalf("the resume was reported as complete for %s; the client would sit "+
					"on a healthy-looking socket discarding every frame", tc.name)
			}
			if resumed.Reason != tc.want {
				t.Errorf("reason = %q, want %q", resumed.Reason, tc.want)
			}
		})
	}
}

// TestSubscribeResumesWithinItsOwnGeneration pins the other half: a cursor from
// *this* run, including one that has fallen off the ring, must never be mistaken
// for a restart.
func TestSubscribeResumesWithinItsOwnGeneration(t *testing.T) {
	bus := New(Config{Replay: 64, Queue: 64})
	for i := 0; i < 3; i++ {
		bus.Publish(TypeSessionMessage, "s1", nil)
	}

	resumed := bus.Subscribe(Cursor{Generation: bus.Generation(), Seq: 3}, nil)
	defer resumed.Subscription.Close()
	if !resumed.Resumable() {
		t.Fatalf("resume of a live cursor reported %q", resumed.Reason)
	}
}

// TestFilteredSubscriptionGetsOnlyItsSessions checks that the session filter is
// applied during replay as well as live, or a reconnecting client would be
// handed other sessions' events.
func TestFilteredSubscriptionGetsOnlyItsSessions(t *testing.T) {
	bus := New(Config{Replay: 64, Queue: 64})
	for i := 0; i < 6; i++ {
		session := "s1"
		if i%2 == 1 {
			session = "s2"
		}
		bus.Publish(TypeSessionMessage, session, map[string]any{"n": i})
	}

	sub := bus.Subscribe(Cursor{Generation: bus.Generation()}, func(e Event) bool {
		// Gateway-wide events (empty session) are always relevant.
		return e.SessionID == "" || e.SessionID == "s1"
	}).Subscription
	defer sub.Close()

	bus.Publish(TypeSessionMessage, "s2", nil)
	bus.Publish(TypeSessionMessage, "s1", nil)

	var got []string
	for {
		select {
		case e := <-sub.Events():
			got = append(got, e.SessionID)
			continue
		default:
		}
		break
	}
	if len(got) != 1 || got[0] != "s1" {
		t.Errorf("received session ids %v, want exactly [s1]", got)
	}
}

// TestSlowSubscriberIsDroppedNotBlocked covers the property the whole bus exists
// for: a subscriber that never reads must not be able to stall the publisher,
// because the publisher runs on the harness reader goroutine and blocking it
// wedges the agent itself.
func TestSlowSubscriberIsDroppedNotBlocked(t *testing.T) {
	bus := New(Config{Replay: 16, Queue: 4})

	// This subscriber never reads from Events().
	slow := bus.Subscribe(Cursor{Generation: bus.Generation()}, nil).Subscription
	defer slow.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 500; i++ {
			bus.Publish(TypeSessionMessage, "s1", nil)
		}
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Publish blocked on a subscriber that stopped reading; this is the " +
			"failure mode that wedges the harness process")
	}

	if bus.Dropped() == 0 {
		t.Error("Dropped() = 0 after overflowing a subscriber's queue; the drop was " +
			"not accounted for, so an operator could not see backpressure happening")
	}
	if reason := slow.TakeResync(); reason != ResyncFellBehind {
		t.Errorf("a subscriber that lost events was flagged %q; it would render an "+
			"incomplete stream as if it were complete", reason)
	}
}

// TestCloseIsIdempotentAndPublishAfterCloseIsSafe guards the shutdown path.
func TestCloseIsIdempotentAndPublishAfterCloseIsSafe(t *testing.T) {
	bus := New(Config{Replay: 8, Queue: 8})
	sub := bus.Subscribe(Cursor{Generation: bus.Generation()}, nil).Subscription

	sub.Close()
	sub.Close() // must not panic on the double close

	// A publish racing a close must not send on a closed channel.
	bus.Publish(TypeSessionMessage, "s1", nil)

	select {
	case <-sub.Done():
	case <-time.After(time.Second):
		t.Error("Done() was not closed")
	}
}

// TestSeqIsMonotonicAcrossPublishes pins the wire contract clients resume from.
func TestSeqIsMonotonicAcrossPublishes(t *testing.T) {
	bus := New(Config{Replay: 8, Queue: 8})

	var last uint64
	for i := 0; i < 100; i++ {
		e := bus.Publish(TypeSessionMessage, "s1", nil)
		if e.Seq <= last {
			t.Fatalf("seq went from %d to %d", last, e.Seq)
		}
		last = e.Seq
		if e.Time.IsZero() {
			t.Fatal("published event has no timestamp")
		}
	}
}

// TestConcurrentPublishAndSubscribe exercises the locking under -race.
func TestConcurrentPublishAndSubscribe(t *testing.T) {
	bus := New(Config{Replay: 32, Queue: 16})

	stop := make(chan struct{})
	go func() {
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			bus.Publish(TypeSessionMessage, fmt.Sprintf("s%d", i%3), nil)
		}
	}()

	for i := 0; i < 20; i++ {
		sub := bus.Subscribe(Cursor{Generation: bus.Generation(), Seq: uint64(i * 3)}, nil).Subscription
		// Drain whatever was replayed, then leave.
		select {
		case <-sub.Events():
		default:
		}
		sub.Close()
	}
	close(stop)
}
