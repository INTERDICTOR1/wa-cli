package whatsapp

import (
	"testing"

	"go.mau.fi/whatsmeow/types"

	"github.com/codebyoketch/wa-cli/internal/chatstore"
	"github.com/codebyoketch/wa-cli/internal/msgstore"
)

func TestCanonicalChat(t *testing.T) {
	tc := newStoreClient(t)
	withDevice := ashaLID
	withDevice.Device = 12
	pnWithDevice := ashaPN
	pnWithDevice.Device = 3

	cases := []struct {
		name string
		in   types.JID
		want types.JID
	}{
		{"lid with a known phone number", ashaLID, ashaPN},
		{"lid with a device part", withDevice, ashaPN},
		{"lid without a mapping stays", strangerLID, strangerLID},
		{"phone number is already canonical", ashaPN, ashaPN},
		{"phone number loses its device part", pnWithDevice, ashaPN},
		{"group is unchanged", groupJID, groupJID},
		{"broadcast is unchanged", types.StatusBroadcastJID, types.StatusBroadcastJID},
	}
	for _, c := range cases {
		if got := tc.canonicalChat(c.in); got != c.want {
			t.Errorf("%s: canonicalChat(%s) = %s, want %s", c.name, c.in, got, c.want)
		}
	}
	if got := tc.canonicalChatString(ashaLID.String()); got != ashaPN.String() {
		t.Errorf("canonicalChatString = %q", got)
	}
	if got := tc.canonicalChatString("not a jid"); got != "not a jid" {
		t.Errorf("a non-JID was changed to %q", got)
	}

	// Without any mapping source (a bare Client), nothing is rewritten.
	bare := &Client{log: testLogger()}
	if got := bare.canonicalChat(ashaLID); got != ashaLID {
		t.Errorf("bare client: %s", got)
	}
}

func TestSameUser(t *testing.T) {
	tc := newStoreClient(t)
	same := [][2]string{
		{ashaLID.String(), ashaPN.String()},
		{"me", meJID.String()},
		{"me", meLID.String()},
		{meLID.String(), meJID.String()},
		{ashaPN.String(), "919541988522:14@s.whatsapp.net"},
	}
	for _, pair := range same {
		if !tc.sameUser(pair[0], pair[1]) || !tc.sameUser(pair[1], pair[0]) {
			t.Errorf("%s and %s should be the same person", pair[0], pair[1])
		}
	}
	different := [][2]string{
		{ashaPN.String(), bilalPN.String()},
		{strangerLID.String(), ashaPN.String()},
		{"me", ashaLID.String()},
	}
	for _, pair := range different {
		if tc.sameUser(pair[0], pair[1]) {
			t.Errorf("%s and %s should be different people", pair[0], pair[1])
		}
	}
}

// A message that arrives addressed to the @lid lands in the phone-number
// chat, whichever of the ways the mapping is known.
func TestIngest_OneChatPerPerson(t *testing.T) {
	tc := newStoreClient(t)

	// The mapping is in the LID store.
	tc.ingestMessage(liveEvent(ashaLID, ashaLID, "M1", text("via lid"), false))
	tc.ingestMessage(liveEvent(ashaPN, ashaPN, "M2", text("via phone number"), false))

	// The mapping only travels with the message itself.
	unknownLID := types.NewJID("300000000000003", types.HiddenUserServer)
	incoming := liveEvent(unknownLID, unknownLID, "M3", text("hello"), false)
	incoming.Info.SenderAlt = bilalPN
	tc.ingestMessage(incoming)
	outgoing := liveEvent(unknownLID, meJID, "M4", text("hi"), true)
	outgoing.Info.RecipientAlt = bilalPN
	tc.ingestMessage(outgoing)

	// No mapping at all: the chat stays under the LID.
	tc.ingestMessage(liveEvent(strangerLID, strangerLID, "M5", text("who is this"), false))

	if n := tc.rowCount(t, ashaPN); n != 2 {
		t.Errorf("Asha's chat has %d messages, want both", n)
	}
	if n := tc.rowCount(t, bilalPN); n != 2 {
		t.Errorf("Bilal's chat has %d messages, want both", n)
	}
	if n := tc.rowCount(t, strangerLID); n != 1 {
		t.Errorf("the stranger's chat has %d messages, want 1", n)
	}
	chats, err := tc.chats.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(chats) != 3 {
		t.Errorf("%d chats stored, want 3 (one per person): %+v", len(chats), chats)
	}
	if chat := tc.mustChat(t, ashaPN); chat.UnreadCount != 2 {
		t.Errorf("Asha's chat: unread %d, want 2", chat.UnreadCount)
	}
	// Senders are kept as they arrived.
	if m := tc.mustGet(t, ashaPN, "M1"); m.SenderJID != ashaLID.String() {
		t.Errorf("sender = %q, want it as received", m.SenderJID)
	}
}

func TestMergeLIDChats(t *testing.T) {
	tc := newStoreClient(t)
	lid, pn := ashaLID.String(), ashaPN.String()

	seed := func(chat, id string, ts int64) {
		t.Helper()
		if _, err := tc.msgs.Insert(msgstore.Message{ID: id, ChatJID: chat, SenderJID: chat, Timestamp: ts, Text: id}); err != nil {
			t.Fatal(err)
		}
	}
	seed(lid, "L1", 100)
	seed(lid, "DUP", 150)
	seed(pn, "DUP", 150)
	seed(pn, "P1", 200)
	seed(strangerLID.String(), "S1", 50)
	for _, chat := range []chatstore.Chat{
		{JID: lid, LastMessageAt: 150, LastMessagePreview: "DUP", UnreadCount: 2},
		{JID: pn, Name: "Asha", LastMessageAt: 200, LastMessagePreview: "P1", UnreadCount: 1},
		{JID: strangerLID.String(), LastMessageAt: 50},
		{JID: groupJID.String(), IsGroup: true, Name: "Family"},
	} {
		if err := tc.chats.Upsert(chat); err != nil {
			t.Fatal(err)
		}
	}
	if err := tc.chats.SetPinned(lid, true); err != nil {
		t.Fatal(err)
	}

	if merged := tc.mergeLIDChats(); merged != 1 {
		t.Fatalf("merged %d chats, want 1", merged)
	}
	if _, ok, _ := tc.chats.Get(lid); ok {
		t.Error("the @lid chat still exists")
	}
	chat := tc.mustChat(t, ashaPN)
	if chat.Name != "Asha" || chat.UnreadCount != 3 || !chat.Pinned || chat.LastMessagePreview != "P1" {
		t.Errorf("merged chat = %+v", chat)
	}
	if n := tc.rowCount(t, ashaPN); n != 3 {
		t.Errorf("merged chat has %d messages, want 3 (the duplicate once)", n)
	}
	if n := tc.rowCount(t, ashaLID); n != 0 {
		t.Errorf("%d messages left under the @lid", n)
	}
	// A LID nobody can place is left alone.
	if n := tc.rowCount(t, strangerLID); n != 1 {
		t.Errorf("the unmapped chat lost its messages (%d)", n)
	}
	if merged := tc.mergeLIDChats(); merged != 0 {
		t.Errorf("a second pass merged %d chats", merged)
	}

	// When the stranger's number becomes known, the next pass merges it,
	// creating the phone-number chat.
	tc.lids.add(strangerLID, bilalPN)
	if merged := tc.mergeLIDChats(); merged != 1 {
		t.Fatalf("after learning the mapping: merged %d, want 1", merged)
	}
	if n := tc.rowCount(t, bilalPN); n != 1 {
		t.Errorf("messages under the new phone-number chat = %d, want 1", n)
	}
	if moved := tc.mustChat(t, bilalPN); moved.LastMessageAt != 50 {
		t.Errorf("moved chat = %+v", moved)
	}
}

// A message arriving for an @lid whose chat was stored before the mapping
// was known pulls that old chat over.
func TestIngest_MergesOldLIDChatOnFirstMessage(t *testing.T) {
	tc := newStoreClient(t)
	// Stored back when the stranger's phone number wasn't known.
	tc.ingestMessage(liveEvent(strangerLID, strangerLID, "OLD", text("earlier"), false))
	if n := tc.rowCount(t, strangerLID); n != 1 {
		t.Fatalf("setup: %d rows", n)
	}

	tc.lids.add(strangerLID, bilalPN)
	tc.log.reset()
	tc.ingestMessage(liveEvent(strangerLID, strangerLID, "NEW", text("now"), false))

	if n := tc.rowCount(t, bilalPN); n != 2 {
		t.Errorf("phone-number chat has %d messages, want the old and the new", n)
	}
	if _, ok, _ := tc.chats.Get(strangerLID.String()); ok {
		t.Error("the old @lid chat is still listed")
	}
	if chat := tc.mustChat(t, bilalPN); chat.UnreadCount != 2 {
		t.Errorf("unread = %d, want 2", chat.UnreadCount)
	}
	if !tc.log.has(ChatEvent{}) {
		t.Errorf("expected a reload event after the merge, got %+v", tc.log.all())
	}
}
