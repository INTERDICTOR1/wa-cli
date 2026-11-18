package whatsapp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	waProto "go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	"github.com/codebyoketch/wa-cli/internal/msgstore"
)

// fakeDecrypter stands in for whatsmeow's message-secret decryption: it
// hands back prepared plaintexts by the ID of the encrypted message.
type fakeDecrypter struct {
	votes     map[string]*waProto.PollVoteMessage
	secrets   map[string]*waProto.Message
	reactions map[string]*waProto.ReactionMessage
}

var errNoSecret = errors.New("original message secret key not found")

func (f *fakeDecrypter) DecryptPollVote(_ context.Context, evt *events.Message) (*waProto.PollVoteMessage, error) {
	if v, ok := f.votes[evt.Info.ID]; ok {
		return v, nil
	}
	return nil, errNoSecret
}

func (f *fakeDecrypter) DecryptSecretEncryptedMessage(_ context.Context, evt *events.Message) (*waProto.Message, error) {
	if m, ok := f.secrets[evt.Info.ID]; ok {
		return m, nil
	}
	return nil, errNoSecret
}

func (f *fakeDecrypter) DecryptReaction(_ context.Context, evt *events.Message) (*waProto.ReactionMessage, error) {
	if r, ok := f.reactions[evt.Info.ID]; ok {
		return r, nil
	}
	return nil, errNoSecret
}

// notifications records desktop notifications instead of showing them.
type notifications struct {
	mu   sync.Mutex
	sent []string
	done chan struct{}
}

func (tc *testClient) recordNotifications() *notifications {
	n := &notifications{done: make(chan struct{}, 64)}
	tc.notifyFn = func(title, body string) error {
		n.mu.Lock()
		n.sent = append(n.sent, title+": "+body)
		n.mu.Unlock()
		n.done <- struct{}{}
		return nil
	}
	tc.SetNotifications(true, true, true)
	return n
}

// wait returns the notifications sent, after giving the background
// sender time to deliver want of them (and any stray extra one).
func (n *notifications) wait(want int) []string {
	deadline := time.After(2 * time.Second)
	for got := 0; got < want; got++ {
		select {
		case <-n.done:
		case <-deadline:
			got = want
		}
	}
	time.Sleep(50 * time.Millisecond) // an extra, unwanted one would arrive now
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.sent...)
}

func reaction(target, emoji string) *waProto.Message {
	return &waProto.Message{ReactionMessage: &waProto.ReactionMessage{
		Key:               key(target),
		Text:              proto.String(emoji),
		SenderTimestampMS: proto.Int64(time.Now().UnixMilli()),
	}}
}

func revoke(target string) *waProto.Message {
	return &waProto.Message{ProtocolMessage: &waProto.ProtocolMessage{
		Type: waProto.ProtocolMessage_REVOKE.Enum(),
		Key:  key(target),
	}}
}

func edit(target string, content *waProto.Message) *waProto.Message {
	return &waProto.Message{ProtocolMessage: &waProto.ProtocolMessage{
		Type:          waProto.ProtocolMessage_MESSAGE_EDIT.Enum(),
		Key:           key(target),
		EditedMessage: content,
	}}
}

func imageMsg(directPath, caption string) *waProto.Message {
	img := &waProto.ImageMessage{
		Mimetype:   proto.String("image/jpeg"),
		DirectPath: proto.String(directPath),
		MediaKey:   []byte("key-" + directPath),
		FileLength: proto.Uint64(1000),
		Width:      proto.Uint32(1600),
		Height:     proto.Uint32(1200),
	}
	if caption != "" {
		img.Caption = proto.String(caption)
	}
	return &waProto.Message{ImageMessage: img}
}

func association(kind waProto.MessageAssociation_AssociationType, parent string) *waProto.MessageContextInfo {
	return &waProto.MessageContextInfo{MessageAssociation: &waProto.MessageAssociation{
		AssociationType:  kind.Enum(),
		ParentMessageKey: key(parent),
	}}
}

func TestFold_ReactionAddReplaceRemove(t *testing.T) {
	tc := newStoreClient(t)
	notes := tc.recordNotifications()
	tc.ingestMessage(liveEvent(ashaPN, ashaPN, "M1", text("hello"), false))

	tc.log.reset()
	tc.ingestMessage(liveEvent(ashaPN, meJID, "R1", reaction("M1", "👍"), true))
	m := tc.mustGet(t, ashaPN, "M1")
	if len(m.Reactions) != 1 || m.Reactions[0].SenderJID != "me" || m.Reactions[0].Emoji != "👍" {
		t.Fatalf("after adding: reactions = %+v", m.Reactions)
	}
	if !tc.log.has(MessageEvent{ChatJID: ashaPN.String(), ID: "M1"}) {
		t.Errorf("expected a change event for M1, got %+v", tc.log.all())
	}

	// A second reaction from the same person replaces the first.
	tc.ingestMessage(liveEvent(ashaPN, meJID, "R2", reaction("M1", "❤️"), true))
	// Someone else's is added beside it — arriving under her LID, it is
	// still recorded under her one identity.
	tc.ingestMessage(liveEvent(ashaLID, ashaLID, "R3", reaction("M1", "😂"), false))
	m = tc.mustGet(t, ashaPN, "M1")
	if len(m.Reactions) != 2 {
		t.Fatalf("after replacing and adding: reactions = %+v", m.Reactions)
	}
	byWho := map[string]string{}
	for _, r := range m.Reactions {
		byWho[r.SenderJID] = r.Emoji
	}
	if byWho["me"] != "❤️" || byWho[ashaPN.String()] != "😂" {
		t.Errorf("reactions by person = %v", byWho)
	}

	// An empty reaction removes that person's.
	tc.ingestMessage(liveEvent(ashaPN, meJID, "R4", reaction("M1", ""), true))
	m = tc.mustGet(t, ashaPN, "M1")
	if len(m.Reactions) != 1 || m.Reactions[0].SenderJID != ashaPN.String() {
		t.Errorf("after removing: reactions = %+v", m.Reactions)
	}

	// None of that is a message: one row, one unread, the preview and the
	// time of the real message, one notification.
	if n := tc.rowCount(t, ashaPN); n != 1 {
		t.Errorf("rows = %d, want 1 (reactions are not rows)", n)
	}
	chat := tc.mustChat(t, ashaPN)
	if chat.UnreadCount != 1 || chat.LastMessagePreview != "hello" || chat.LastMessageAt != m.Timestamp {
		t.Errorf("chat = %+v, want 1 unread, preview hello, time of M1", chat)
	}
	if sent := notes.wait(1); len(sent) != 1 {
		t.Errorf("notifications = %v, want only the one for the real message", sent)
	}
	for _, e := range tc.log.messageEvents() {
		if e.New {
			t.Errorf("a reaction was announced as a new message: %+v", e)
		}
	}
}

func TestFold_Edit(t *testing.T) {
	tc := newStoreClient(t)
	tc.ingestMessage(liveEvent(ashaPN, ashaPN, "M1", text("helo"), false))
	tc.log.reset()

	tc.ingestMessage(liveEvent(ashaPN, ashaPN, "E1", edit("M1", text("hello")), false))
	m := tc.mustGet(t, ashaPN, "M1")
	if m.Text != "hello" || !m.Edited {
		t.Errorf("after edit: text %q edited %v", m.Text, m.Edited)
	}
	if got := contentOf(m).GetConversation(); got != "hello" {
		t.Errorf("stored content = %q, want the edited text", got)
	}
	if n := tc.rowCount(t, ashaPN); n != 1 {
		t.Errorf("rows = %d, want 1", n)
	}
	chat := tc.mustChat(t, ashaPN)
	if chat.UnreadCount != 1 || chat.LastMessagePreview != "hello" {
		t.Errorf("chat = %+v, want 1 unread and the edited preview", chat)
	}
	if !tc.log.has(MessageEvent{ChatJID: ashaPN.String(), ID: "M1"}) {
		t.Errorf("expected a change event for M1, got %+v", tc.log.all())
	}
}

func TestFold_EditOnlyBySender(t *testing.T) {
	tc := newStoreClient(t)
	tc.ingestMessage(liveEvent(groupJID, ashaPN, "M1", text("original"), false))
	// Bilal can't edit Asha's message.
	tc.ingestMessage(liveEvent(groupJID, bilalPN, "E1", edit("M1", text("hijacked")), false))
	if m := tc.mustGet(t, groupJID, "M1"); m.Text != "original" || m.Edited {
		t.Errorf("someone else's edit was applied: %+v", m)
	}
	// Asha can, also when this event names her by LID.
	tc.ingestMessage(liveEvent(groupJID, ashaLID, "E2", edit("M1", text("fixed")), false))
	if m := tc.mustGet(t, groupJID, "M1"); m.Text != "fixed" || !m.Edited {
		t.Errorf("the sender's edit was not applied: %+v", m)
	}
}

func TestFold_CaptionEditKeepsMedia(t *testing.T) {
	tc := newStoreClient(t)
	tc.ingestMessage(liveEvent(ashaPN, ashaPN, "M1", imageMsg("/v/sd", "old caption"), false))

	captionOnly := &waProto.Message{ImageMessage: &waProto.ImageMessage{Caption: proto.String("new caption")}}
	tc.ingestMessage(liveEvent(ashaPN, ashaPN, "E1", edit("M1", captionOnly), false))

	m := tc.mustGet(t, ashaPN, "M1")
	img := contentOf(m).GetImageMessage()
	if m.Text != "new caption" || img.GetCaption() != "new caption" {
		t.Errorf("caption not updated: text %q, proto %q", m.Text, img.GetCaption())
	}
	if img.GetDirectPath() != "/v/sd" || len(img.GetMediaKey()) == 0 || img.GetWidth() != 1600 {
		t.Errorf("the edit lost the media: %+v", img)
	}
	if m.MediaType != "image" || !m.Edited {
		t.Errorf("row = %+v", m)
	}
}

func TestFold_EditKeepsReplyQuote(t *testing.T) {
	tc := newStoreClient(t)
	reply := &waProto.Message{ExtendedTextMessage: &waProto.ExtendedTextMessage{
		Text: proto.String("yes"),
		ContextInfo: &waProto.ContextInfo{
			StanzaID:      proto.String("Q1"),
			Participant:   proto.String(meJID.String()),
			QuotedMessage: text("coming?"),
		},
	}}
	tc.ingestMessage(liveEvent(ashaPN, ashaPN, "M1", reply, false))
	tc.ingestMessage(liveEvent(ashaPN, ashaPN, "E1", edit("M1", text("yes!")), false))

	ext := contentOf(tc.mustGet(t, ashaPN, "M1")).GetExtendedTextMessage()
	if ext.GetText() != "yes!" || ext.GetContextInfo().GetStanzaID() != "Q1" ||
		ext.GetContextInfo().GetQuotedMessage().GetConversation() != "coming?" {
		t.Errorf("edited reply = %+v", ext)
	}
}

func TestFold_Revoke(t *testing.T) {
	tc := newStoreClient(t)
	tc.ingestMessage(liveEvent(ashaPN, ashaPN, "M1", text("first"), false))
	tc.ingestMessage(liveEvent(ashaPN, ashaPN, "M2", text("oops"), false))
	tc.ingestMessage(liveEvent(ashaPN, meJID, "R1", reaction("M2", "👍"), true))
	tc.log.reset()

	// You can't delete her message for everyone...
	tc.ingestMessage(liveEvent(ashaPN, meJID, "D0", revoke("M2"), true))
	if m := tc.mustGet(t, ashaPN, "M2"); m.Deleted {
		t.Fatal("a delete from someone who isn't the sender was applied in a 1:1 chat")
	}
	// ...she can.
	tc.ingestMessage(liveEvent(ashaPN, ashaPN, "D1", revoke("M2"), false))
	m := tc.mustGet(t, ashaPN, "M2")
	if !m.Deleted || m.Text != "" || m.RawProto != "" || len(m.Reactions) != 0 {
		t.Errorf("after delete: %+v", m)
	}
	if n := tc.rowCount(t, ashaPN); n != 2 {
		t.Errorf("rows = %d, want 2 (the deleted message keeps its place)", n)
	}
	chat := tc.mustChat(t, ashaPN)
	if chat.UnreadCount != 2 || chat.LastMessagePreview != "🚫 This message was deleted" {
		t.Errorf("chat = %+v", chat)
	}
	if !tc.log.has(MessageEvent{ChatJID: ashaPN.String(), ID: "M2"}) {
		t.Errorf("expected a change event for M2, got %+v", tc.log.all())
	}
}

func TestFold_RevokeInGroupNeedsAdmin(t *testing.T) {
	tc := newStoreClient(t)
	tc.ingestMessage(liveEvent(groupJID, ashaPN, "M1", text("one"), false))
	tc.ingestMessage(liveEvent(groupJID, ashaPN, "M2", text("two"), false))

	// With the member list loaded, a non-admin's delete is ignored.
	tc.rememberGroup(&types.GroupInfo{JID: groupJID, Participants: []types.GroupParticipant{
		{JID: ashaPN}, {JID: bilalPN, IsAdmin: false},
	}})
	tc.ingestMessage(liveEvent(groupJID, bilalPN, "D1", revoke("M1"), false))
	if tc.mustGet(t, groupJID, "M1").Deleted {
		t.Error("a non-admin deleted someone else's message")
	}
	// An admin's is applied.
	tc.rememberGroup(&types.GroupInfo{JID: groupJID, Participants: []types.GroupParticipant{
		{JID: ashaPN}, {JID: bilalPN, IsAdmin: true},
	}})
	tc.ingestMessage(liveEvent(groupJID, bilalPN, "D2", revoke("M2"), false))
	if !tc.mustGet(t, groupJID, "M2").Deleted {
		t.Error("an admin's delete was not applied")
	}
}

func hdChild(parent string) *waProto.Message {
	hd := imageMsg("/v/hd", "")
	hd.ImageMessage.Width, hd.ImageMessage.Height = proto.Uint32(4096), proto.Uint32(3072)
	hd.ImageMessage.FileLength = proto.Uint64(9_000_000)
	hd.MessageContextInfo = association(waProto.MessageAssociation_HD_IMAGE_DUAL_UPLOAD, parent)
	return &waProto.Message{AssociatedChildMessage: &waProto.FutureProofMessage{Message: hd}}
}

func TestFold_HDChildReplacesStoredParent(t *testing.T) {
	tc := newStoreClient(t)
	notes := tc.recordNotifications()
	tc.ingestMessage(liveEvent(ashaPN, ashaPN, "M1", imageMsg("/v/sd", "the beach"), false))
	tc.log.reset()

	tc.ingestMessage(liveEvent(ashaPN, ashaPN, "M2", hdChild("M1"), false))

	if n := tc.rowCount(t, ashaPN); n != 1 {
		t.Fatalf("rows = %d, want 1: the HD copy replaces the standard one", n)
	}
	m := tc.mustGet(t, ashaPN, "M1")
	img := contentOf(m).GetImageMessage()
	if !m.HD || img.GetDirectPath() != "/v/hd" || img.GetWidth() != 4096 {
		t.Errorf("parent not upgraded: hd=%v path=%q width=%d", m.HD, img.GetDirectPath(), img.GetWidth())
	}
	// The HD copy has no caption of its own: the parent's is kept, in the
	// text and in the content the view is built from.
	if m.Text != "the beach" || img.GetCaption() != "the beach" {
		t.Errorf("caption lost: text %q, proto %q", m.Text, img.GetCaption())
	}
	if m.MediaType != "image" {
		t.Errorf("media type = %q", m.MediaType)
	}
	if chat := tc.mustChat(t, ashaPN); chat.UnreadCount != 1 {
		t.Errorf("unread = %d, want 1: the HD copy is not a second message", chat.UnreadCount)
	}
	if !tc.log.has(MessageEvent{ChatJID: ashaPN.String(), ID: "M1"}) {
		t.Errorf("expected a change event for M1, got %+v", tc.log.all())
	}
	if sent := notes.wait(1); len(sent) != 1 {
		t.Errorf("notifications = %v, want 1", sent)
	}
	v, ok := tc.view(context.Background(), m)
	if !ok || v.Kind != KindImage || !v.HD || v.Media.Width != 4096 || v.Media.Size != 9_000_000 || v.Body != "the beach" {
		t.Errorf("view = %+v (media %+v)", v, v.Media)
	}
}

func TestFold_HDChildWithoutWrapper(t *testing.T) {
	tc := newStoreClient(t)
	tc.ingestMessage(liveEvent(ashaPN, ashaPN, "M1", imageMsg("/v/sd", ""), false))

	// The same association, on a plain image message.
	child := hdChild("M1").GetAssociatedChildMessage().GetMessage()
	tc.ingestMessage(liveEvent(ashaPN, ashaPN, "M2", child, false))

	if n := tc.rowCount(t, ashaPN); n != 1 {
		t.Fatalf("rows = %d, want 1", n)
	}
	if m := tc.mustGet(t, ashaPN, "M1"); !m.HD || contentOf(m).GetImageMessage().GetDirectPath() != "/v/hd" {
		t.Errorf("parent not upgraded: %+v", m)
	}
}

func TestFold_HDChildWithoutStoredParent(t *testing.T) {
	tc := newStoreClient(t)
	tc.ingestMessage(liveEvent(ashaPN, ashaPN, "M2", hdChild("GONE"), false))

	m := tc.mustGet(t, ashaPN, "M2")
	if !m.HD || m.MediaType != "image" || contentOf(m).GetImageMessage().GetDirectPath() != "/v/hd" {
		t.Errorf("orphan HD copy should be stored as its own image: %+v", m)
	}
	if contentOf(m).GetAssociatedChildMessage() != nil {
		t.Error("the stored content is still wrapped")
	}
	if chat := tc.mustChat(t, ashaPN); chat.UnreadCount != 1 || chat.LastMessagePreview != "📷 Photo" {
		t.Errorf("chat = %+v, want it counted and previewed as a photo", chat)
	}
	if !tc.log.has(MessageEvent{ChatJID: ashaPN.String(), ID: "M2", New: true}) {
		t.Errorf("expected a new-message event, got %+v", tc.log.all())
	}
}

func TestFold_HDChildOnlyFromParentSender(t *testing.T) {
	tc := newStoreClient(t)
	tc.ingestMessage(liveEvent(groupJID, ashaPN, "M1", imageMsg("/v/sd", ""), false))
	tc.ingestMessage(liveEvent(groupJID, bilalPN, "M2", hdChild("M1"), false))
	if m := tc.mustGet(t, groupJID, "M1"); m.HD || contentOf(m).GetImageMessage().GetDirectPath() != "/v/sd" {
		t.Errorf("someone else replaced the image: %+v", m)
	}
}

func TestFold_Album(t *testing.T) {
	tc := newStoreClient(t)
	album := &waProto.Message{AlbumMessage: &waProto.AlbumMessage{ExpectedImageCount: proto.Uint32(2)}}
	tc.ingestMessage(liveEvent(ashaPN, ashaPN, "A1", album, false))
	if _, ok, _ := tc.chats.Get(ashaPN.String()); ok {
		t.Error("the album announcement alone created a chat entry")
	}
	for _, id := range []string{"P1", "P2"} {
		photo := imageMsg("/v/"+id, "")
		photo.MessageContextInfo = association(waProto.MessageAssociation_MEDIA_ALBUM, "A1")
		tc.ingestMessage(liveEvent(ashaPN, ashaPN, id, photo, false))
	}
	// One of them wrapped as an associated child, as some clients send.
	wrapped := imageMsg("/v/P3", "")
	wrapped.MessageContextInfo = association(waProto.MessageAssociation_MEDIA_ALBUM, "A1")
	tc.ingestMessage(liveEvent(ashaPN, ashaPN, "P3",
		&waProto.Message{AssociatedChildMessage: &waProto.FutureProofMessage{Message: wrapped}}, false))

	if n := tc.rowCount(t, ashaPN); n != 3 {
		t.Fatalf("rows = %d, want the 3 photos and no album row", n)
	}
	if _, ok, _ := tc.msgs.Get(ashaPN.String(), "A1"); ok {
		t.Error("the album announcement was stored as a row")
	}
	if m := tc.mustGet(t, ashaPN, "P3"); m.HD || m.MediaType != "image" || contentOf(m).GetImageMessage().GetDirectPath() != "/v/P3" {
		t.Errorf("wrapped album photo = %+v", m)
	}
	if chat := tc.mustChat(t, ashaPN); chat.UnreadCount != 3 {
		t.Errorf("unread = %d, want 3", chat.UnreadCount)
	}
}

// optionHash is how WhatsApp names a poll option in a vote.
func optionHash(name string) []byte {
	sum := sha256.Sum256([]byte(name))
	return sum[:]
}

func pollVote(pollID string) *waProto.Message {
	return &waProto.Message{PollUpdateMessage: &waProto.PollUpdateMessage{
		PollCreationMessageKey: key(pollID),
		Vote:                   &waProto.PollEncValue{EncPayload: []byte("ciphertext"), EncIV: []byte("iv")},
		SenderTimestampMS:      proto.Int64(time.Now().UnixMilli()),
	}}
}

func TestFold_PollVotesAndTally(t *testing.T) {
	tc := newStoreClient(t)
	dec := &fakeDecrypter{votes: map[string]*waProto.PollVoteMessage{
		"V1": {SelectedOptions: [][]byte{optionHash("Pizza")}},
		"V2": {SelectedOptions: [][]byte{optionHash("Pizza"), optionHash("Biryani")}},
		"V3": {SelectedOptions: [][]byte{optionHash("Biryani")}},
		"V4": {SelectedOptions: [][]byte{optionHash("Momos")}},
		"V5": {}, // unticked everything
	}}
	tc.decryptOverride = dec

	poll := &waProto.Message{PollCreationMessageV3: &waProto.PollCreationMessage{
		Name: proto.String("Lunch?"),
		Options: []*waProto.PollCreationMessage_Option{
			{OptionName: proto.String("Pizza")},
			{OptionName: proto.String("Biryani")},
			{OptionName: proto.String("Momos")},
		},
		SelectableOptionsCount: proto.Uint32(0),
	}}
	tc.ingestMessage(liveEvent(groupJID, meJID, "P1", poll, true))

	tc.ingestMessage(liveEvent(groupJID, ashaLID, "V1", pollVote("P1"), false))
	tc.ingestMessage(liveEvent(groupJID, bilalPN, "V2", pollVote("P1"), false))
	tc.ingestMessage(liveEvent(groupJID, meJID, "V3", pollVote("P1"), true))
	// A vote that can't be decrypted is dropped.
	tc.ingestMessage(liveEvent(groupJID, bilalPN, "V-undecryptable", pollVote("P1"), false))

	tally := func() map[string]PollOption {
		t.Helper()
		v, ok, err := tc.Message(context.Background(), groupJID.String(), "P1")
		if err != nil || !ok || v.Kind != KindPoll || v.Poll == nil {
			t.Fatalf("poll view: %+v ok=%v err=%v", v, ok, err)
		}
		if v.Poll.Question != "Lunch?" || !v.Poll.MultiSelect || len(v.Poll.Options) != 3 {
			t.Fatalf("poll = %+v", v.Poll)
		}
		out := map[string]PollOption{}
		for _, o := range v.Poll.Options {
			out[o.Name] = o
		}
		return out
	}
	got := tally()
	if got["Pizza"].Votes != 2 || got["Biryani"].Votes != 2 || got["Momos"].Votes != 0 {
		t.Errorf("tally = %+v, want Pizza 2, Biryani 2, Momos 0", got)
	}
	if !got["Biryani"].Mine || got["Pizza"].Mine {
		t.Errorf("mine = %+v, want only Biryani", got)
	}

	// Asha changes her vote (this time seen under her phone number), and
	// Bilal unticks everything.
	tc.ingestMessage(liveEvent(groupJID, ashaPN, "V4", pollVote("P1"), false))
	tc.ingestMessage(liveEvent(groupJID, bilalPN, "V5", pollVote("P1"), false))
	got = tally()
	if got["Pizza"].Votes != 0 || got["Biryani"].Votes != 1 || got["Momos"].Votes != 1 {
		t.Errorf("tally after changes = %+v, want Pizza 0, Biryani 1, Momos 1", got)
	}

	if n := tc.rowCount(t, groupJID); n != 1 {
		t.Errorf("rows = %d, want only the poll", n)
	}
	// Stored as lower-case hex, as msgstore.PollVote documents.
	for _, v := range tc.mustGet(t, groupJID, "P1").PollVotes {
		for _, h := range v.Options {
			if raw, err := hex.DecodeString(h); err != nil || len(raw) != sha256.Size || h != strings.ToLower(h) {
				t.Errorf("vote option %q is not lower-case hex SHA-256", h)
			}
		}
	}
}

func TestFold_SecretEncryptedEdit(t *testing.T) {
	tc := newStoreClient(t)
	tc.decryptOverride = &fakeDecrypter{secrets: map[string]*waProto.Message{
		"S1": edit("M1", text("decrypted edit")),
		// Some clients put the new content straight into the envelope.
		"S2": text("second edit"),
	}}
	secret := func() *waProto.Message {
		return &waProto.Message{SecretEncryptedMessage: &waProto.SecretEncryptedMessage{
			TargetMessageKey: key("M1"),
			EncPayload:       []byte("x"),
			SecretEncType:    waProto.SecretEncryptedMessage_MESSAGE_EDIT.Enum(),
		}}
	}
	tc.ingestMessage(liveEvent(ashaPN, ashaPN, "M1", text("before"), false))

	tc.ingestMessage(liveEvent(ashaPN, ashaPN, "S1", secret(), false))
	if m := tc.mustGet(t, ashaPN, "M1"); m.Text != "decrypted edit" || !m.Edited {
		t.Errorf("after encrypted edit: %+v", m)
	}
	tc.ingestMessage(liveEvent(ashaPN, ashaPN, "S2", secret(), false))
	if m := tc.mustGet(t, ashaPN, "M1"); m.Text != "second edit" {
		t.Errorf("after second encrypted edit: %+v", m)
	}
	// One that can't be decrypted leaves no trace.
	tc.ingestMessage(liveEvent(ashaPN, ashaPN, "S3", secret(), false))
	if n := tc.rowCount(t, ashaPN); n != 1 {
		t.Errorf("rows = %d, want 1", n)
	}
	if chat := tc.mustChat(t, ashaPN); chat.UnreadCount != 1 {
		t.Errorf("unread = %d, want 1", chat.UnreadCount)
	}
}

func TestFold_EncryptedReaction(t *testing.T) {
	tc := newStoreClient(t)
	tc.decryptOverride = &fakeDecrypter{reactions: map[string]*waProto.ReactionMessage{
		"R1": {Text: proto.String("🎉")},
	}}
	tc.ingestMessage(liveEvent(groupJID, ashaPN, "M1", text("news"), false))
	enc := func() *waProto.Message {
		return &waProto.Message{EncReactionMessage: &waProto.EncReactionMessage{TargetMessageKey: key("M1"), EncPayload: []byte("x")}}
	}
	tc.ingestMessage(liveEvent(groupJID, bilalPN, "R1", enc(), false))
	tc.ingestMessage(liveEvent(groupJID, bilalPN, "R-undecryptable", enc(), false))

	m := tc.mustGet(t, groupJID, "M1")
	if len(m.Reactions) != 1 || m.Reactions[0].Emoji != "🎉" || m.Reactions[0].SenderJID != bilalPN.String() {
		t.Errorf("reactions = %+v", m.Reactions)
	}
	if n := tc.rowCount(t, groupJID); n != 1 {
		t.Errorf("rows = %d, want 1", n)
	}
}

func TestFold_NoiseIsNeverStored(t *testing.T) {
	protocol := func(tp waProto.ProtocolMessage_Type) *waProto.Message {
		return &waProto.Message{ProtocolMessage: &waProto.ProtocolMessage{Type: tp.Enum()}}
	}
	noise := map[string]*waProto.Message{
		"nil":                       nil,
		"empty":                     {},
		"only key":                  {SenderKeyDistributionMessage: &waProto.SenderKeyDistributionMessage{GroupID: proto.String("g")}, MessageContextInfo: &waProto.MessageContextInfo{}},
		"history sync notification": protocol(waProto.ProtocolMessage_HISTORY_SYNC_NOTIFICATION),
		"app state key share":       protocol(waProto.ProtocolMessage_APP_STATE_SYNC_KEY_SHARE),
		"app state key request":     protocol(waProto.ProtocolMessage_APP_STATE_SYNC_KEY_REQUEST),
		"peer data request":         protocol(waProto.ProtocolMessage_PEER_DATA_OPERATION_REQUEST_MESSAGE),
		"peer data response":        protocol(waProto.ProtocolMessage_PEER_DATA_OPERATION_REQUEST_RESPONSE_MESSAGE),
		"lid migration":             protocol(waProto.ProtocolMessage_LID_MIGRATION_MAPPING_SYNC),
		"security setting":          protocol(waProto.ProtocolMessage_INITIAL_SECURITY_NOTIFICATION_SETTING_SYNC),
		"revoke without key":        protocol(waProto.ProtocolMessage_REVOKE),
		"history notice":            {MessageHistoryNotice: &waProto.MessageHistoryNotice{}},
		"history bundle":            {MessageHistoryBundle: &waProto.MessageHistoryBundle{}},
		"enc comment":               {EncCommentMessage: &waProto.EncCommentMessage{}},
		"pin in chat":               {PinInChatMessage: &waProto.PinInChatMessage{Key: key("M1")}},
		"keep in chat":              {KeepInChatMessage: &waProto.KeepInChatMessage{Key: key("M1")}},
		"album":                     {AlbumMessage: &waProto.AlbumMessage{}},
		"enc event response":        {EncEventResponseMessage: &waProto.EncEventResponseMessage{}},
	}
	tc := newStoreClient(t)
	notes := tc.recordNotifications()
	i := 0
	for name, msg := range noise {
		i++
		tc.ingestMessage(liveEvent(ashaPN, ashaPN, "N"+string(rune('a'+i)), msg, false))
		if n := tc.rowCount(t, ashaPN); n != 0 {
			t.Fatalf("%s was stored as a message", name)
		}
	}
	if _, ok, _ := tc.chats.Get(ashaPN.String()); ok {
		t.Error("noise created a chat entry (unread count or preview)")
	}
	if events := tc.log.all(); len(events) != 0 {
		t.Errorf("noise caused events: %+v", events)
	}
	if sent := notes.wait(0); len(sent) != 0 {
		t.Errorf("noise caused notifications: %v", sent)
	}
}

func TestFold_UnknownTargetIsDropped(t *testing.T) {
	tc := newStoreClient(t)
	tc.decryptOverride = &fakeDecrypter{
		votes:   map[string]*waProto.PollVoteMessage{"V1": {SelectedOptions: [][]byte{optionHash("x")}}},
		secrets: map[string]*waProto.Message{"S1": edit("OLD", text("edit"))},
	}
	notes := tc.recordNotifications()
	orphans := map[string]*waProto.Message{
		"R1": reaction("OLD", "👍"),
		"E1": edit("OLD", text("edited")),
		"D1": revoke("OLD"),
		"V1": pollVote("OLD"),
		"S1": {SecretEncryptedMessage: &waProto.SecretEncryptedMessage{TargetMessageKey: key("OLD")}},
	}
	for id, msg := range orphans {
		tc.ingestMessage(liveEvent(ashaPN, ashaPN, id, msg, false))
	}
	if n := tc.rowCount(t, ashaPN); n != 0 {
		t.Errorf("rows = %d, want 0: events for messages that aren't stored are dropped", n)
	}
	if _, ok, _ := tc.chats.Get(ashaPN.String()); ok {
		t.Error("an orphan event created a chat entry")
	}
	if events := tc.log.all(); len(events) != 0 {
		t.Errorf("orphan events caused events: %+v", events)
	}
	if sent := notes.wait(0); len(sent) != 0 {
		t.Errorf("orphan events caused notifications: %v", sent)
	}
}

// An event must not reach into another conversation, even when it names
// a message ID that exists there.
func TestFold_EventsStayInTheirChat(t *testing.T) {
	tc := newStoreClient(t)
	tc.ingestMessage(liveEvent(ashaPN, ashaPN, "M1", text("private"), false))
	tc.ingestMessage(liveEvent(bilalPN, bilalPN, "X1", reaction("M1", "👀"), false))
	tc.ingestMessage(liveEvent(groupJID, ashaPN, "X2", revoke("M1"), false))
	if m := tc.mustGet(t, ashaPN, "M1"); m.Deleted || len(m.Reactions) != 0 {
		t.Errorf("a message was changed from another chat: %+v", m)
	}
}

func TestIngest_DuplicateIsCountedOnce(t *testing.T) {
	tc := newStoreClient(t)
	notes := tc.recordNotifications()
	evt := liveEvent(ashaPN, ashaPN, "M1", text("hello"), false)
	tc.ingestMessage(evt)
	tc.ingestMessage(evt)

	if n := tc.rowCount(t, ashaPN); n != 1 {
		t.Errorf("rows = %d, want 1", n)
	}
	if chat := tc.mustChat(t, ashaPN); chat.UnreadCount != 1 {
		t.Errorf("unread = %d, want 1", chat.UnreadCount)
	}
	fresh := 0
	for _, e := range tc.log.messageEvents() {
		if e.New {
			fresh++
		}
	}
	if fresh != 1 {
		t.Errorf("new-message events = %d, want 1", fresh)
	}
	if sent := notes.wait(1); len(sent) != 1 {
		t.Errorf("notifications = %v, want 1", sent)
	}
}

func TestIngest_OwnMessageFromAnotherDeviceMarksChatRead(t *testing.T) {
	tc := newStoreClient(t)
	notes := tc.recordNotifications()
	tc.ingestMessage(liveEvent(ashaPN, ashaPN, "M1", text("hello"), false))
	tc.ingestMessage(liveEvent(ashaPN, ashaPN, "M2", text("are you there?"), false))
	if chat := tc.mustChat(t, ashaPN); chat.UnreadCount != 2 {
		t.Fatalf("unread = %d, want 2", chat.UnreadCount)
	}

	// You answer on your phone.
	tc.ingestMessage(liveEvent(ashaPN, meJID, "M3", text("yes"), true))
	chat := tc.mustChat(t, ashaPN)
	if chat.UnreadCount != 0 || chat.LastMessagePreview != "yes" {
		t.Errorf("chat = %+v, want read, preview of your reply", chat)
	}
	m := tc.mustGet(t, ashaPN, "M3")
	if m.SenderJID != "me" || !m.FromMe || m.Status != msgstore.StatusSent {
		t.Errorf("own message = %+v", m)
	}
	if sent := notes.wait(2); len(sent) != 2 {
		t.Errorf("notifications = %v, want 2 (none for your own message)", sent)
	}
}

func TestIngest_NotificationRules(t *testing.T) {
	tc := newStoreClient(t)
	tc.contacts.set(ashaPN, types.ContactInfo{FullName: "Asha"})
	notes := tc.recordNotifications()

	photo := imageMsg("/v/1", "")
	tc.ingestMessage(liveEvent(ashaPN, ashaPN, "M1", photo, false))

	// An old message delivered on reconnect is not announced.
	stale := liveEvent(ashaPN, ashaPN, "M2", text("from this morning"), false)
	stale.Info.Timestamp = time.Now().Add(-10 * time.Minute)
	tc.ingestMessage(stale)

	// A chat muted on the phone stays quiet.
	tc.ingestMessage(liveEvent(bilalPN, bilalPN, "M3", text("first"), false))
	if err := tc.chats.SetMutedUntil(bilalPN.String(), -1); err != nil {
		t.Fatal(err)
	}
	tc.ingestMessage(liveEvent(bilalPN, bilalPN, "M4", text("muted"), false))

	sent := notes.wait(2)
	want := []string{"Asha: 📷 Photo", "+91 90000 00002: first"}
	if len(sent) != len(want) {
		t.Fatalf("notifications = %q, want %q", sent, want)
	}
	seen := map[string]bool{}
	for _, s := range sent {
		seen[s] = true
	}
	for _, w := range want {
		if !seen[w] {
			t.Errorf("notifications = %q, missing %q", sent, w)
		}
	}
}

func TestIngest_DisappearingTimer(t *testing.T) {
	tc := newStoreClient(t)
	withTimer := &waProto.Message{ExtendedTextMessage: &waProto.ExtendedTextMessage{
		Text:        proto.String("this will vanish"),
		ContextInfo: &waProto.ContextInfo{Expiration: proto.Uint32(604800)},
	}}
	tc.ingestMessage(liveEvent(ashaPN, ashaPN, "M1", withTimer, false))
	if chat := tc.mustChat(t, ashaPN); chat.Ephemeral != 604800 {
		t.Errorf("timer = %d, want it learned from the message", chat.Ephemeral)
	}

	off := &waProto.Message{ProtocolMessage: &waProto.ProtocolMessage{
		Type:                waProto.ProtocolMessage_EPHEMERAL_SETTING.Enum(),
		EphemeralExpiration: proto.Uint32(0),
	}}
	tc.log.reset()
	tc.ingestMessage(liveEvent(ashaPN, ashaPN, "T1", off, false))
	chat := tc.mustChat(t, ashaPN)
	if chat.Ephemeral != 0 {
		t.Errorf("timer = %d, want it switched off", chat.Ephemeral)
	}
	if chat.UnreadCount != 1 || tc.rowCount(t, ashaPN) != 1 {
		t.Errorf("the timer change was counted as a message: %+v", chat)
	}
	if !tc.log.has(ChatEvent{JID: ashaPN.String()}) {
		t.Errorf("expected a chat event, got %+v", tc.log.all())
	}
}

func TestIngest_ViewOncePlaceholder(t *testing.T) {
	tc := newStoreClient(t)
	info := liveEvent(ashaPN, ashaPN, "VO1", nil, false).Info
	tc.ingestUnavailable(&events.UndecryptableMessage{Info: info, IsUnavailable: true, UnavailableType: events.UnavailableTypeViewOnce})
	// Any other undecryptable message is retried by whatsmeow; nothing is stored.
	other := liveEvent(ashaPN, ashaPN, "U1", nil, false).Info
	tc.ingestUnavailable(&events.UndecryptableMessage{Info: other})

	if n := tc.rowCount(t, ashaPN); n != 1 {
		t.Fatalf("rows = %d, want the one placeholder", n)
	}
	v, ok := tc.view(context.Background(), tc.mustGet(t, ashaPN, "VO1"))
	if !ok || v.Kind != KindUnsupported || v.Body == "" {
		t.Errorf("placeholder view = %+v", v)
	}

	// Should the real message arrive after all, it takes the placeholder's place.
	tc.ingestMessage(liveEvent(ashaPN, ashaPN, "VO1", imageMsg("/v/once", ""), false))
	m := tc.mustGet(t, ashaPN, "VO1")
	if m.MediaType != "image" || contentOf(m).GetImageMessage() == nil {
		t.Errorf("placeholder was not replaced: %+v", m)
	}
	if chat := tc.mustChat(t, ashaPN); chat.UnreadCount != 1 {
		t.Errorf("unread = %d, want 1", chat.UnreadCount)
	}
}

// hdChildAsDelivered is the HD copy the way WhatsApp really sends it
// (checked against stored messages): the link to the standard-quality
// photo sits on the outer wrapper, and the photo inside carries none.
func hdChildAsDelivered(parent string) *waProto.Message {
	hd := imageMsg("/v/hd", "")
	hd.ImageMessage.Width, hd.ImageMessage.Height = proto.Uint32(4096), proto.Uint32(3072)
	hd.ImageMessage.FileLength = proto.Uint64(9_000_000)
	return &waProto.Message{
		AssociatedChildMessage: &waProto.FutureProofMessage{Message: hd},
		MessageContextInfo:     association(waProto.MessageAssociation_HD_IMAGE_DUAL_UPLOAD, parent),
	}
}

func TestFold_HDChildLinkedOnTheWrapper(t *testing.T) {
	tc := newStoreClient(t)
	tc.ingestMessage(liveEvent(ashaPN, ashaPN, "M1", imageMsg("/v/sd", "the beach"), false))
	tc.ingestMessage(liveEvent(ashaPN, ashaPN, "M2", hdChildAsDelivered("M1"), false))

	if n := tc.rowCount(t, ashaPN); n != 1 {
		t.Fatalf("rows = %d, want 1: the HD copy replaces the standard one", n)
	}
	m := tc.mustGet(t, ashaPN, "M1")
	img := contentOf(m).GetImageMessage()
	if !m.HD || img.GetDirectPath() != "/v/hd" || img.GetWidth() != 4096 {
		t.Fatalf("parent not upgraded: hd=%v path=%q width=%d", m.HD, img.GetDirectPath(), img.GetWidth())
	}
	if m.Text != "the beach" {
		t.Errorf("caption lost: %q", m.Text)
	}
}

// The same, for rows imported from the old JSON store.
func TestFoldLegacy_HDChildLinkedOnTheWrapper(t *testing.T) {
	tc := newStoreClient(t)
	rows := []msgstore.Message{
		{ID: "M1", ChatJID: ashaPN.String(), SenderJID: "me", FromMe: true, Timestamp: 1000, MediaType: "image", RawProto: encodeRawProto(imageMsg("/v/sd", "sunset"))},
		{ID: "M2", ChatJID: ashaPN.String(), SenderJID: "me", FromMe: true, Timestamp: 2000, RawProto: encodeRawProto(hdChildAsDelivered("M1"))},
	}
	if err := tc.msgs.AppendMany(rows); err != nil {
		t.Fatal(err)
	}
	if err := tc.msgs.SetMeta(legacyFoldMeta, "pending"); err != nil {
		t.Fatal(err)
	}
	if err := tc.foldLegacy(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := tc.rowCount(t, ashaPN); n != 1 {
		t.Fatalf("rows = %d, want 1", n)
	}
	m := tc.mustGet(t, ashaPN, "M1")
	if !m.HD || contentOf(m).GetImageMessage().GetDirectPath() != "/v/hd" {
		t.Fatalf("legacy parent not upgraded: hd=%v", m.HD)
	}
}
