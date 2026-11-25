package whatsapp

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	waLog "go.mau.fi/whatsmeow/util/log"
)

// warnLog records Warnf lines; everything else is dropped.
type warnLog struct{ lines []string }

func (w *warnLog) Warnf(msg string, args ...any) {
	w.lines = append(w.lines, fmt.Sprintf(msg, args...))
}
func (w *warnLog) Errorf(msg string, args ...any) {}
func (w *warnLog) Infof(msg string, args ...any)  {}
func (w *warnLog) Debugf(msg string, args ...any) {}
func (w *warnLog) Sub(string) waLog.Logger        { return w }

// A refused send must leave a trace in the log: WhatsApp's error code is
// otherwise only shown on screen for a moment, and gone when it matters.
func TestFailedSendIsLogged(t *testing.T) {
	tc := newStoreClient(t)
	logs := &warnLog{}
	tc.Client.log = logs
	tc.finishSend("120363000000000000@g.us", "3EB0TEST", errors.New("server returned error 479"))
	for _, l := range logs.lines {
		if strings.Contains(l, "3EB0TEST") && strings.Contains(l, "120363000000000000@g.us") && strings.Contains(l, "479") {
			return
		}
	}
	t.Fatalf("no log line with the message, chat and error; got %q", logs.lines)
}
