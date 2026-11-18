package whatsapp

import (
	"context"
	"reflect"
	"testing"

	waProto "go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"

	"github.com/codebyoketch/wa-cli/internal/chatstore"
	"github.com/codebyoketch/wa-cli/internal/msgstore"
)

// seedLegacy fills the store the way the old JSON store's import leaves
// it: every event-like message is a row of its own.
func seedLegacy(t *testing.T, tc *testClient) {
	t.Helper()
	asha, me := ashaPN.String(), "me"
	ts := int64(1_700_000_000_000)
	row := func(chat types.JID, id, sender string, msg *waProto.Message) msgstore.Message {
		ts += 1000
		mediaType, text := "", ""
		if msg != nil {
			// What the old code stored: text and caption only.
			mediaType, text = classifyMessage(msg)
			if msg.GetTemplateMessage() != nil || pollOf(msg) != nil {
				text = ""
			}
		}
		return msgstore.Message{
			ID: id, ChatJID: chat.String(), SenderJID: sender, FromMe: sender == me,
			Timestamp: ts, Text: text, MediaType: mediaType, RawProto: rawOf(msg),
		}
	}
	template := &waProto.Message{TemplateMessage: &waProto.TemplateMessage{
		HydratedTemplate: &waProto.TemplateMessage_HydratedFourRowTemplate{
			HydratedContentText: proto.String("Your OTP is 4821"),
		},
	}}
	poll := &waProto.Message{PollCreationMessage: &waProto.PollCreationMessage{
		Name:    proto.String("Tea?"),
		Options: []*waProto.PollCreationMessage_Option{{OptionName: proto.String("Yes")}, {OptionName: proto.String("No")}},
	}}
	rows := []msgstore.Message{
		row(ashaPN, "M1", asha, text("helo")),
		row(ashaPN, "R1", me, reaction("M1", "👍")),
		row(ashaPN, "E1", asha, edit("M1", text("hello"))),
		row(ashaPN, "M2", asha, text("oops")),
		row(ashaPN, "D1", asha, revoke("M2")),
		row(ashaPN, "N1", asha, &waProto.Message{SenderKeyDistributionMessage: &waProto.SenderKeyDistributionMessage{GroupID: proto.String("g")}}),
		row(ashaPN, "N2", me, &waProto.Message{ProtocolMessage: &waProto.ProtocolMessage{Type: waProto.ProtocolMessage_HISTORY_SYNC_NOTIFICATION.Enum()}}),
		row(ashaPN, "A1", asha, &waProto.Message{AlbumMessage: &waProto.AlbumMessage{ExpectedImageCount: proto.Uint32(1)}}),
		row(ashaPN, "IMG1", asha, imageMsg("/v/sd", "sunset")),
		row(ashaPN, "C1", asha, hdChild("IMG1")),
		row(ashaPN, "C2", asha, hdChild("TRIMMED-AWAY")),
		row(ashaPN, "P1", me, poll),
		row(ashaPN, "V1", asha, pollVote("P1")),
		row(ashaPN, "V2", asha, pollVote("P1")), // can't be decrypted
		row(ashaPN, "T1", asha, template),
		row(ashaPN, "R-orphan", asha, reaction("TRIMMED-AWAY", "👍")),
		{ID: "PLAIN", ChatJID: asha, SenderJID: me, FromMe: true, Timestamp: 1_700_000_900_000, Text: "sent by an old version"},

		// The same conversation, stored a second time under Asha's LID.
		row(ashaLID, "L1", ashaLID.String(), text("from the lid chat")),
		row(ashaLID, "L2", ashaLID.String(), reaction("M1", "❤️")),
		row(groupJID, "G1", ashaLID.String(), text("group message")),
		row(groupJID, "G2", bilalPN.String(), reaction("G1", "🔥")),
	}
	if _, err := tc.msgs.InsertMany(rows); err != nil {
		t.Fatalf("seeding rows: %v", err)
	}
	for _, chat := range []chatstore.Chat{
		{JID: asha, Name: "Asha", LastMessageAt: ts, UnreadCount: 1},
		{JID: ashaLID.String(), LastMessageAt: ts + 5000, LastMessagePreview: "from the lid chat", UnreadCount: 2},
		{JID: groupJID.String(), Name: "Family", IsGroup: true, LastMessageAt: ts},
	} {
		if err := tc.chats.Upsert(chat); err != nil {
			t.Fatalf("seeding chats: %v", err)
		}
	}
	if err := tc.msgs.SetMeta(legacyFoldMeta, "pending"); err != nil {
		t.Fatalf("SetMeta: %v", err)
	}
}

func (tc *testClient) snapshot(t *testing.T) map[string][]msgstore.Message {
	t.Helper()
	out := map[string][]msgstore.Message{}
	err := tc.msgs.Each(func(m msgstore.Message) error {
		out[m.ChatJID] = append(out[m.ChatJID], m)
		return nil
	})
	if err != nil {
		t.Fatalf("Each: %v", err)
	}
	return out
}

func TestLegacyFold(t *testing.T) {
	tc := newStoreClient(t)
	tc.decryptOverride = &fakeDecrypter{votes: map[string]*waProto.PollVoteMessage{
		"V1": {SelectedOptions: [][]byte{optionHash("Yes")}},
	}}
	seedLegacy(t, tc)
	ctx := context.Background()

	tc.Maintain(ctx)

	if state, _ := tc.msgs.Meta(legacyFoldMeta); state != "done" {
		t.Fatalf("legacy_fold = %q, want done", state)
	}

	// The LID copy of the chat is gone, its messages moved over.
	if _, ok, _ := tc.chats.Get(ashaLID.String()); ok {
		t.Error("the @lid chat still exists after the merge")
	}
	if n := tc.rowCount(t, ashaLID); n != 0 {
		t.Errorf("%d messages are still stored under the @lid chat", n)
	}
	chat := tc.mustChat(t, ashaPN)
	if chat.Name != "Asha" || chat.UnreadCount != 3 || chat.LastMessagePreview != "from the lid chat" {
		t.Errorf("merged chat = %+v", chat)
	}

	var ids []string
	for _, m := range tc.snapshot(t)[ashaPN.String()] {
		ids = append(ids, m.ID)
	}
	want := []string{"M1", "M2", "IMG1", "C2", "P1", "T1", "L1", "PLAIN"}
	if !reflect.DeepEqual(ids, want) {
		t.Fatalf("rows left in the chat = %v, want %v", ids, want)
	}

	m1 := tc.mustGet(t, ashaPN, "M1")
	if m1.Text != "hello" || !m1.Edited {
		t.Errorf("M1 = %+v, want the edit applied", m1)
	}
	reactions := map[string]string{}
	for _, r := range m1.Reactions {
		reactions[r.SenderJID] = r.Emoji
	}
	if len(reactions) != 2 || reactions["me"] != "👍" || reactions[ashaPN.String()] != "❤️" {
		t.Errorf("M1 reactions = %v, want mine and the one from the @lid chat", reactions)
	}
	if m2 := tc.mustGet(t, ashaPN, "M2"); !m2.Deleted || m2.Text != "" {
		t.Errorf("M2 = %+v, want it deleted", m2)
	}
	img := tc.mustGet(t, ashaPN, "IMG1")
	if !img.HD || contentOf(img).GetImageMessage().GetDirectPath() != "/v/hd" || img.Text != "sunset" {
		t.Errorf("IMG1 = %+v, want the HD copy with the caption kept", img)
	}
	// The HD copy whose parent was trimmed away becomes a normal image.
	orphan := tc.mustGet(t, ashaPN, "C2")
	if !orphan.HD || orphan.MediaType != "image" || contentOf(orphan).GetImageMessage().GetDirectPath() != "/v/hd" {
		t.Errorf("C2 = %+v, want an unwrapped HD image", orphan)
	}
	p1 := tc.mustGet(t, ashaPN, "P1")
	if len(p1.PollVotes) != 1 || p1.PollVotes[0].VoterJID != ashaPN.String() || p1.Text != "Tea?" {
		t.Errorf("P1 = %+v, want one vote and the question as searchable text", p1)
	}
	// Rows that stay get the text the new code stores (searchable).
	if t1 := tc.mustGet(t, ashaPN, "T1"); t1.Text != "Your OTP is 4821" {
		t.Errorf("T1 text = %q, want the template's text", t1.Text)
	}
	if plain := tc.mustGet(t, ashaPN, "PLAIN"); plain.Text != "sent by an old version" {
		t.Errorf("a row without content was changed: %+v", plain)
	}
	g1 := tc.mustGet(t, groupJID, "G1")
	if len(g1.Reactions) != 1 || g1.Reactions[0].SenderJID != bilalPN.String() || tc.rowCount(t, groupJID) != 1 {
		t.Errorf("group: G1 = %+v", g1)
	}

	// Running it again changes nothing.
	before := tc.snapshot(t)
	tc.Maintain(ctx)
	if err := tc.foldLegacy(ctx); err != nil {
		t.Fatalf("second foldLegacy: %v", err)
	}
	if after := tc.snapshot(t); !reflect.DeepEqual(before, after) {
		t.Errorf("a second pass changed the store:\nbefore %+v\nafter  %+v", before, after)
	}
}

// An interrupted pass leaves the flag pending; doing it all again from
// the half-folded state gives the same result as one clean pass.
func TestLegacyFold_Interrupted(t *testing.T) {
	clean := newStoreClient(t)
	seedLegacy(t, clean)
	clean.Maintain(context.Background())

	tc := newStoreClient(t)
	seedLegacy(t, tc)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	tc.mergeLIDChats()
	if err := tc.foldLegacy(cancelled); err == nil {
		t.Fatal("a cancelled pass reported success")
	}
	if state, _ := tc.msgs.Meta(legacyFoldMeta); state != "pending" {
		t.Fatalf("legacy_fold = %q after an interrupted pass, want pending", state)
	}
	// Simulate having stopped half way: some events already applied and
	// one of their rows already removed.
	if _, err := tc.msgs.ApplyEdit(ashaPN.String(), "M1", "hello", rawOf(text("hello"))); err != nil {
		t.Fatal(err)
	}
	if err := tc.msgs.Delete(ashaPN.String(), "N1"); err != nil {
		t.Fatal(err)
	}

	tc.Maintain(context.Background())
	if state, _ := tc.msgs.Meta(legacyFoldMeta); state != "done" {
		t.Fatalf("legacy_fold = %q, want done", state)
	}
	got, want := tc.snapshot(t), clean.snapshot(t)
	// Seq numbers differ between two stores; compare everything else.
	for _, rows := range []map[string][]msgstore.Message{got, want} {
		for _, list := range rows {
			for i := range list {
				list[i].Seq = 0
				for j := range list[i].Reactions {
					list[i].Reactions[j].Timestamp = 0
				}
			}
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("resumed pass differs from a clean one:\ngot  %+v\nwant %+v", got, want)
	}
}

func TestLegacyFold_OnlyWhenPending(t *testing.T) {
	tc := newStoreClient(t)
	seedLegacy(t, tc)
	if err := tc.msgs.SetMeta(legacyFoldMeta, ""); err != nil {
		t.Fatal(err)
	}
	if err := tc.foldLegacy(context.Background()); err != nil {
		t.Fatalf("foldLegacy: %v", err)
	}
	// Untouched: the reaction row is still there.
	if _, ok, _ := tc.msgs.Get(ashaPN.String(), "R1"); !ok {
		t.Error("rows were folded although no legacy pass was pending")
	}
}
