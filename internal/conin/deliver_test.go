package conin

import (
	"reflect"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// collect returns a send function that forwards to a channel, so a test can
// wait for deliveries.
func collect(size int) (send func(tea.Msg), got chan tea.Msg) {
	got = make(chan tea.Msg, size)
	return func(msg tea.Msg) { got <- msg }, got
}

// take reads n messages or fails after a second.
func take(t *testing.T, ch <-chan tea.Msg, n int) []tea.Msg {
	t.Helper()
	var msgs []tea.Msg
	for len(msgs) < n {
		select {
		case msg := <-ch:
			msgs = append(msgs, msg)
		case <-time.After(time.Second):
			t.Fatalf("got only %d of %d messages: %v", len(msgs), n, msgs)
		}
	}
	return msgs
}

// expectNone fails if a message turns up within a short while.
func expectNone(t *testing.T, ch <-chan tea.Msg) {
	t.Helper()
	select {
	case msg := <-ch:
		t.Fatalf("unexpected message %v", msg)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestDeliveryKeepsOrder(t *testing.T) {
	send, got := collect(100)
	d := newDelivery(send)
	defer d.close()

	var want []tea.Msg
	for i := 0; i < 100; i++ {
		want = append(want, i)
		d.push(i)
	}
	if msgs := take(t, got, 100); !reflect.DeepEqual(msgs, want) {
		t.Errorf("messages arrived out of order: %v", msgs)
	}
}

// The read loop must never wait for the UI: push returns at once even while
// send is stuck, and everything is delivered once send moves again.
func TestPushNeverBlocks(t *testing.T) {
	release := make(chan struct{})
	delivered := make(chan tea.Msg, 1000)
	d := newDelivery(func(msg tea.Msg) {
		<-release // a busy UI: Program.Send does not return
		delivered <- msg
	})
	defer d.close()

	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			d.push(i)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("push blocked while send was busy")
	}

	close(release)
	msgs := take(t, delivered, 1000)
	for i, msg := range msgs {
		if msg != i {
			t.Fatalf("message %d is %v", i, msg)
		}
	}
}

func TestDivert(t *testing.T) {
	send, sent := collect(10)
	d := newDelivery(send)
	defer d.close()

	// The divert function takes strings and lets everything else through.
	diverted := make(chan tea.Msg, 10)
	d.setDivert(func(msg tea.Msg) bool {
		if _, ok := msg.(string); ok {
			diverted <- msg
			return true
		}
		return false
	})
	d.push("for the viewer", 42)
	if msgs := take(t, diverted, 1); msgs[0] != "for the viewer" {
		t.Errorf("diverted %v", msgs)
	}
	if msgs := take(t, sent, 1); msgs[0] != 42 {
		t.Errorf("sent %v", msgs)
	}
	expectNone(t, sent) // the diverted message was not also sent

	// Divert(nil) restores normal delivery.
	d.setDivert(nil)
	d.push("normal again")
	if msgs := take(t, sent, 1); msgs[0] != "normal again" {
		t.Errorf("sent %v", msgs)
	}
	expectNone(t, diverted)
}

// The situation Divert exists for: Bubble Tea's event loop is blocked (an
// image viewer is running inside Update), so a message that was already on
// its way is stuck in send. Keys must still reach the viewer.
func TestDivertWorksWhileSendIsStuck(t *testing.T) {
	release := make(chan struct{})
	sent := make(chan tea.Msg, 10)
	d := newDelivery(func(msg tea.Msg) {
		<-release
		sent <- msg
	})
	defer d.close()

	d.push("stuck")                   // send blocks on this one
	time.Sleep(20 * time.Millisecond) // let it get stuck

	diverted := make(chan tea.Msg, 10)
	d.setDivert(func(msg tea.Msg) bool { diverted <- msg; return true })
	d.push("q")
	if msgs := take(t, diverted, 1); msgs[0] != "q" {
		t.Errorf("diverted %v", msgs)
	}

	// The viewer closes; the event loop resumes; the stuck message arrives.
	d.setDivert(nil)
	close(release)
	if msgs := take(t, sent, 1); msgs[0] != "stuck" {
		t.Errorf("sent %v", msgs)
	}
}

// A divert function may remove itself.
func TestDivertCanClearItself(t *testing.T) {
	send, sent := collect(10)
	d := newDelivery(send)
	defer d.close()

	d.setDivert(func(msg tea.Msg) bool {
		d.setDivert(nil)
		return true
	})
	d.push("taken", "passed on")
	if msgs := take(t, sent, 1); msgs[0] != "passed on" {
		t.Errorf("sent %v", msgs)
	}
}

func TestCloseDiscardsAndStops(t *testing.T) {
	send, sent := collect(10)
	d := newDelivery(send)
	d.close()
	d.close() // closing twice is harmless
	d.push("too late")
	expectNone(t, sent)
}
