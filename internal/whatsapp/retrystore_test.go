package whatsapp

import "testing"

// A device that couldn't decrypt a sent message (often a WhatsApp Web tab
// that comes online later) asks for it again. The message must still be
// findable after a restart, so it has to live on disk, not only in
// whatsmeow's in-memory cache.
func TestNewKeepsSentMessagesForRetries(t *testing.T) {
	tc := newRealClient(t)
	if !tc.WA.UseRetryMessageStore {
		t.Fatal("UseRetryMessageStore is off: re-send requests after a restart would fail with \"couldn't find message\"")
	}
}
