package lifecycle

import (
	"sync"
	"testing"
	"time"

	"github.com/huanghantao/dsh-gateway/internal/app/events"
	"github.com/huanghantao/dsh-gateway/internal/errx"
	"github.com/huanghantao/dsh-gateway/internal/logx"
)

// TestDrainIsIdempotentAndAnnouncedOnce guards the property the frame exists
// for: a redeploy is one event, not one per caller that noticed.
//
// Several shutdown paths race to drain — the signal handler, a supervisor, a
// future health check that gives up — and a client that received two notices
// for one deploy would be told the gateway is flapping.
func TestDrainIsIdempotentAndAnnouncedOnce(t *testing.T) {
	bus := events.New(events.Config{Replay: 32, Queue: 32})
	sub := bus.Subscribe(events.Cursor{}, func(e events.Event) bool {
		return e.Type == events.TypeDraining
	}).Subscription
	defer sub.Close()

	life := New(bus, logx.Discard())

	life.Drain(ReasonDeploy)
	life.Drain(ReasonShutdown)
	life.Drain(ReasonDeploy)

	if life.Serving() {
		t.Error("Serving() = true after Drain")
	}
	if !life.Draining() {
		t.Error("Draining() = false after Drain")
	}

	var got []events.Event
	for {
		select {
		case e := <-sub.Events():
			got = append(got, e)
			continue
		default:
		}
		break
	}
	if len(got) != 1 {
		t.Fatalf("published %d draining frames, want exactly 1", len(got))
	}
	payload, ok := got[0].Data.(events.Draining)
	if !ok {
		t.Fatalf("draining payload is %T, want events.Draining", got[0].Data)
	}
	if payload.Reason != string(ReasonDeploy) {
		t.Errorf("reason = %q, want the first one (%q)", payload.Reason, ReasonDeploy)
	}
}

// TestAdmitRefusesWhileDraining pins the door. A prompt accepted by a process
// that is about to close the agent underneath it is a turn the drain did not
// wait for: accepted with a 202 and then destroyed.
func TestAdmitRefusesWhileDraining(t *testing.T) {
	life := New(nil, logx.Discard())

	if err := life.Admit(); err != nil {
		t.Fatalf("Admit() before draining = %v, want nil", err)
	}

	life.Drain(ReasonShutdown)

	err := life.Admit()
	if err == nil {
		t.Fatal("Admit() while draining = nil, want a refusal")
	}
	if kind := errx.KindOf(err); kind != errx.KindUnavailable {
		t.Errorf("refusal kind = %v, want KindUnavailable (a retryable 503)", kind)
	}
	if code := errx.CodeOf(err); code != "gateway_draining" {
		t.Errorf("refusal code = %q, want gateway_draining", code)
	}
}

// TestWaitForGivesUpWhenTheWindowExpires is the honest half of the drain: a turn
// that outlives the window does not block a deploy forever.
func TestWaitForGivesUpWhenTheWindowExpires(t *testing.T) {
	var mu sync.Mutex
	busy := true

	started := time.Now()
	done := WaitFor(neverDone{}, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return busy
	}, 150*time.Millisecond, 10*time.Millisecond)

	if done {
		t.Error("WaitFor reported idle while a turn was still running")
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Errorf("WaitFor took %s, want it to stop at the window", elapsed)
	}
}

// TestWaitForReturnsAsSoonAsTheWorkIsDone is the common case, and the reason the
// window is a ceiling rather than a delay: a deploy with nothing in flight must
// not take two minutes.
func TestWaitForReturnsAsSoonAsTheWorkIsDone(t *testing.T) {
	var mu sync.Mutex
	busy := true
	go func() {
		time.Sleep(30 * time.Millisecond)
		mu.Lock()
		busy = false
		mu.Unlock()
	}()

	started := time.Now()
	done := WaitFor(neverDone{}, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return busy
	}, time.Minute, 5*time.Millisecond)

	if !done {
		t.Fatal("WaitFor reported the gateway never became idle")
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Errorf("WaitFor took %s to notice idle work, want it to return promptly", elapsed)
	}
}

// TestWaitForWithNoWindowDoesNotWait pins the "redeploy instantly" setting.
func TestWaitForWithNoWindowDoesNotWait(t *testing.T) {
	started := time.Now()
	if WaitFor(neverDone{}, func() bool { return true }, 0, time.Millisecond) {
		t.Error("WaitFor with a zero window reported idle while work was in flight")
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Errorf("WaitFor with a zero window took %s", elapsed)
	}
}

// neverDone is a Done that is never closed, so a test's cancellation path does
// not accidentally decide the outcome.
type neverDone struct{}

func (neverDone) Done() <-chan struct{} { return make(chan struct{}) }
