package whatsapp

import (
	"testing"

	"go.mau.fi/whatsmeow/proto/waCommon"
	waProto "go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"

	"github.com/codebyoketch/wa-cli/internal/msgstore"
)

func TestFormatPhone(t *testing.T) {
	cases := map[string]string{
		"919541988522": "+91 95419 88522",
		"14155550123":  "+14155550123",
		"9195419":      "+9195419",
	}
	for in, want := range cases {
		if got := formatPhone(in); got != want {
			t.Errorf("formatPhone(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPickName_FieldPriorityAcrossContacts(t *testing.T) {
	pnContact := types.ContactInfo{PushName: "pn push"}
	lidContact := types.ContactInfo{FullName: "Lid Full"}
	if got := pickName(pnContact, lidContact); got != "Lid Full" {
		t.Errorf("got %q, want address-book name to beat push name", got)
	}
	if got := pickName(types.ContactInfo{PushName: "-"}, types.ContactInfo{BusinessName: "Biz"}); got != "Biz" {
		t.Errorf(`got %q, want "-" push name skipped`, got)
	}
	if got := pickName(); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestDisplayText(t *testing.T) {
	resolve := func(jid string) string {
		if jid == "177403959709918@lid" {
			return "Asha"
		}
		return "?"
	}
	ptype := func(tp waProto.ProtocolMessage_Type) *waProto.Message {
		return &waProto.Message{ProtocolMessage: &waProto.ProtocolMessage{Type: tp.Enum()}}
	}

	cases := []struct {
		name   string
		msg    *waProto.Message
		want   string
		hidden bool
	}{
		{"group key distribution", &waProto.Message{
			SenderKeyDistributionMessage: &waProto.SenderKeyDistributionMessage{GroupID: proto.String("g")},
			MessageContextInfo:           &waProto.MessageContextInfo{},
		}, "", true},
		{"history sync notice", ptype(waProto.ProtocolMessage_HISTORY_SYNC_NOTIFICATION), "", true},
		{"empty", &waProto.Message{}, "", true},
		{"text", &waProto.Message{Conversation: proto.String("hi")}, "hi", false},
		{"mention", &waProto.Message{ExtendedTextMessage: &waProto.ExtendedTextMessage{
			Text:        proto.String("@177403959709918 kal bank?"),
			ContextInfo: &waProto.ContextInfo{MentionedJID: []string{"177403959709918@lid"}},
		}}, "@Asha kal bank?", false},
		// Reactions, deletes and edits belong to another message: they are
		// folded into it and never shown as lines of their own.
		{"reaction", &waProto.Message{ReactionMessage: &waProto.ReactionMessage{Text: proto.String("👍")}}, "", true},
		{"reaction removed", &waProto.Message{ReactionMessage: &waProto.ReactionMessage{}}, "", true},
		{"deleted", &waProto.Message{ProtocolMessage: &waProto.ProtocolMessage{
			Type: waProto.ProtocolMessage_REVOKE.Enum(),
			Key:  &waCommon.MessageKey{ID: proto.String("X")},
		}}, "", true},
		{"edited", &waProto.Message{ProtocolMessage: &waProto.ProtocolMessage{
			Type:          waProto.ProtocolMessage_MESSAGE_EDIT.Enum(),
			EditedMessage: &waProto.Message{Conversation: proto.String("fixed")},
		}}, "", true},
		{"album announcement", &waProto.Message{AlbumMessage: &waProto.AlbumMessage{ExpectedImageCount: proto.Uint32(3)}}, "", true},
		{"image", &waProto.Message{ImageMessage: &waProto.ImageMessage{Caption: proto.String("look")}}, "[image] look", false},
		{"voice note", &waProto.Message{AudioMessage: &waProto.AudioMessage{PTT: proto.Bool(true)}}, "[voice note]", false},
		{"location", &waProto.Message{LocationMessage: &waProto.LocationMessage{
			DegreesLatitude: proto.Float64(34.0837), DegreesLongitude: proto.Float64(74.7973), Name: proto.String("Lal Chowk"),
		}}, "Lal Chowk\nhttps://maps.google.com/?q=34.0837,74.7973", false},
		{"poll", &waProto.Message{PollCreationMessageV3: &waProto.PollCreationMessage{Name: proto.String("Lunch?")}}, "[poll] Lunch?", false},
		{"unknown type", &waProto.Message{NewsletterAdminInviteMessage: &waProto.NewsletterAdminInviteMessage{}}, "[newsletterAdminInvite]", false},
	}
	for _, tc := range cases {
		got, hidden := displayText(tc.msg, msgstore.Message{}, resolve)
		if got != tc.want || hidden != tc.hidden {
			t.Errorf("%s: got (%q, %v), want (%q, %v)", tc.name, got, hidden, tc.want, tc.hidden)
		}
	}

	if got, hidden := displayText(nil, msgstore.Message{Text: "sent", MediaType: "image"}, resolve); got != "[image] sent" || hidden {
		t.Errorf("no raw proto: got (%q, %v)", got, hidden)
	}
	if got, hidden := displayText(nil, msgstore.Message{Deleted: true}, resolve); got != "[deleted]" || hidden {
		t.Errorf("deleted row: got (%q, %v)", got, hidden)
	}
	edited := &waProto.Message{Conversation: proto.String("fixed")}
	if got, hidden := displayText(edited, msgstore.Message{Edited: true}, resolve); got != "fixed (edited)" || hidden {
		t.Errorf("edited row: got (%q, %v)", got, hidden)
	}
}
