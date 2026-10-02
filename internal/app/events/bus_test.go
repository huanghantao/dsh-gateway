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
	var (
		sub           *Subscription
		replayMissing bool
	)
	go func() {
		defer close(done)
		// since=1 asks for every event after the first: 99 of them, far more
		// than the channel can hold.
		sub, replayMissing = bus.Subscribe(1, nil)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Subscribe blocked: the replay batch is not bounded by the queue depth")
	}
	defer sub.Close()

	if !replayMissing {
		t.Error("replayMissing = false, but the replay was truncated; the client would " +
			"render an incomplete stream with no indication anything was lost")
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

	sub, missing := bus.Subscribe(3, nil)
	defer sub.Close()

	if missing {
		t.Error("replayMissing = true, but every requested event was still retained")
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

	sub, missing := bus.Subscribe(1, nil)
	defer sub.Close()

	if !missing {
		t.Error("replayMissing = false, but the requested cursor fell off the ring")
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

	sub, _ := bus.Subscribe(0, func(e Event) bool {
		// Gateway-wide events (empty session) are always relevant.
		return e.SessionID == "" || e.SessionID == "s1"
	})
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
	slow, _ := bus.Subscribe(0, nil)
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
	if !slow.TakeResync() {
		t.Error("a subscriber that lost events was not flagged for resync; it would " +
			"render an incomplete stream as if it were complete")
	}
}

// TestCloseIsIdempotentAndPublishAfterCloseIsSafe guards the shutdown path.
func TestCloseIsIdempotentAndPublishAfterCloseIsSafe(t *testing.T) {
	bus := New(Config{Replay: 8, Queue: 8})
	sub, _ := bus.Subscribe(0, nil)

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
		sub, _ := bus.Subscribe(uint64(i*3), nil)
		// Drain whatever was replayed, then leave.
		select {
		case <-sub.Events():
		default:
		}
		sub.Close()
	}
	close(stop)
}
