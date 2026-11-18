package conin

import (
	"sync"

	tea "github.com/charmbracelet/bubbletea"
)

// fifo is a first-in-first-out queue of messages with no size limit. Adding
// to it never waits, which is the whole point: the goroutine that reads the
// console must never be held up by a slow consumer.
type fifo struct {
	mu     sync.Mutex
	wakeup *sync.Cond // signalled when items is added to, or the queue closes
	items  []tea.Msg
	closed bool
}

func newFifo() *fifo {
	q := &fifo{}
	q.wakeup = sync.NewCond(&q.mu)
	return q
}

// push adds messages to the back of the queue. It never blocks.
func (q *fifo) push(msgs ...tea.Msg) {
	if len(msgs) == 0 {
		return
	}
	q.mu.Lock()
	if !q.closed {
		q.items = append(q.items, msgs...)
	}
	q.mu.Unlock()
	q.wakeup.Broadcast()
}

// pop removes and returns the message at the front, waiting for one if the
// queue is empty. ok is false once the queue has been closed.
func (q *fifo) pop() (msg tea.Msg, ok bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for len(q.items) == 0 && !q.closed {
		q.wakeup.Wait()
	}
	if q.closed {
		return nil, false
	}
	msg = q.items[0]
	q.items[0] = nil // let the garbage collector have it
	q.items = q.items[1:]
	return msg, true
}

// close ends the queue: waiting and future pops return ok == false, and
// anything still queued is discarded.
func (q *fifo) close() {
	q.mu.Lock()
	q.closed = true
	q.items = nil
	q.mu.Unlock()
	q.wakeup.Broadcast()
}

// delivery carries messages from the read loop to Bubble Tea.
//
// Bubble Tea's Program.Send waits until the program's event loop accepts the
// message, and that loop is busy whenever the UI is updating or drawing. If
// the read loop called Send itself, a busy UI would make it fall behind;
// several separately typed keys would then pile up in the console's queue,
// be read as one batch, and look like a paste. So there are two queues and
// two goroutines between them:
//
//	read loop --push--> inbox --route--> outbox --forward--> send
//
// route is where Divert hooks in. It is a separate step from forward so that
// a diverted message never has to wait behind a Send that is stuck — the
// image viewer that diverts input is running precisely while Bubble Tea's
// event loop is blocked and Send cannot make progress.
type delivery struct {
	send   func(tea.Msg)
	inbox  *fifo
	outbox *fifo

	mu     sync.Mutex
	divert func(tea.Msg) bool
}

// newDelivery starts the two goroutines and returns the delivery.
func newDelivery(send func(tea.Msg)) *delivery {
	d := &delivery{send: send, inbox: newFifo(), outbox: newFifo()}
	go d.route()
	go d.forward()
	return d
}

// push queues messages for delivery. It never blocks.
func (d *delivery) push(msgs ...tea.Msg) {
	d.inbox.push(msgs...)
}

// setDivert installs (or, with nil, removes) the divert function.
func (d *delivery) setDivert(fn func(tea.Msg) bool) {
	d.mu.Lock()
	d.divert = fn
	d.mu.Unlock()
}

// route offers each message to the divert function, if there is one, and
// passes on those it does not take.
func (d *delivery) route() {
	for {
		msg, ok := d.inbox.pop()
		if !ok {
			return
		}
		d.mu.Lock()
		fn := d.divert
		d.mu.Unlock()
		// fn is called without holding the lock, so it may itself call
		// Divert (for example to remove itself) without deadlocking.
		if fn != nil && fn(msg) {
			continue
		}
		d.outbox.push(msg)
	}
}

// forward hands messages to send, one at a time and in order.
func (d *delivery) forward() {
	for {
		msg, ok := d.outbox.pop()
		if !ok {
			return
		}
		d.send(msg)
	}
}

// close stops both goroutines and discards undelivered messages. A send that
// is already in progress is left to finish on its own.
func (d *delivery) close() {
	d.inbox.close()
	d.outbox.close()
}
