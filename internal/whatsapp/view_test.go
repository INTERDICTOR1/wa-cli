package whatsapp

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	waProto "go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"

	"github.com/codebyoketch/wa-cli/internal/chatstore"
	"github.com/codebyoketch/wa-cli/internal/msgstore"
)

// stored builds a stored incoming message from Asha with the given content.
func stored(id string, msg *waProto.Message) msgstore.Message {
	mediaType, text := analyze(unwrapMessage(msg)).storedFields()
	return msgstore.Message{
		ID: id, ChatJID: ashaPN.String(), SenderJID: ashaLID.String(),
		Timestamp: time.Now().UnixMilli(), Text: text, MediaType: mediaType, RawProto: rawOf(msg),
	}
}

func TestView_Kinds(t *testing.T) {
	tc := newStoreClient(t)
	tc.contacts.set(ashaPN, types.ContactInfo{FullName: "Asha"})
	thumb := []byte{0xff, 0xd8, 0xff}

	cases := []struct {
		name  string
		msg   *waProto.Message
		kind  string
		body  string
		media *MediaInfo
	}{
		{"text", text("hi"), KindText, "hi", nil},
		{"extended text", &waProto.Message{ExtendedTextMessage: &waProto.ExtendedTextMessage{Text: proto.String("link")}}, KindText, "link", nil},
		{"image", &waProto.Message{ImageMessage: &waProto.ImageMessage{
			Caption: proto.String("look"), Mimetype: proto.String("image/jpeg"), FileLength: proto.Uint64(2048),
			Width: proto.Uint32(1600), Height: proto.Uint32(900), JPEGThumbnail: thumb,
		}}, KindImage, "look", &MediaInfo{Mime: "image/jpeg", Size: 2048, Width: 1600, Height: 900, HasThumb: true}},
		{"view-once image", &waProto.Message{ViewOnceMessageV2: &waProto.FutureProofMessage{Message: &waProto.Message{
			ImageMessage: &waProto.ImageMessage{Mimetype: proto.String("image/jpeg")},
		}}}, KindImage, "", &MediaInfo{Mime: "image/jpeg", ViewOnce: true}},
		{"video", &waProto.Message{VideoMessage: &waProto.VideoMessage{
			Mimetype: proto.String("video/mp4"), Seconds: proto.Uint32(42), FileLength: proto.Uint64(5 << 20),
			Width: proto.Uint32(720), Height: proto.Uint32(1280), JPEGThumbnail: thumb,
		}}, KindVideo, "", &MediaInfo{Mime: "video/mp4", Size: 5 << 20, Width: 720, Height: 1280, Seconds: 42, HasThumb: true}},
		{"gif", &waProto.Message{VideoMessage: &waProto.VideoMessage{
			Mimetype: proto.String("video/mp4"), GifPlayback: proto.Bool(true), Caption: proto.String("lol"),
		}}, KindGIF, "lol", &MediaInfo{Mime: "video/mp4"}},
		{"audio", &waProto.Message{AudioMessage: &waProto.AudioMessage{
			Mimetype: proto.String("audio/mpeg"), Seconds: proto.Uint32(200), FileLength: proto.Uint64(3000),
		}}, KindAudio, "", &MediaInfo{Mime: "audio/mpeg", Size: 3000, Seconds: 200}},
		{"voice", &waProto.Message{AudioMessage: &waProto.AudioMessage{
			Mimetype: proto.String("audio/ogg; codecs=opus"), Seconds: proto.Uint32(7), PTT: proto.Bool(true),
		}}, KindVoice, "", &MediaInfo{Mime: "audio/ogg; codecs=opus", Seconds: 7}},
		{"document", &waProto.Message{DocumentWithCaptionMessage: &waProto.FutureProofMessage{Message: &waProto.Message{
			DocumentMessage: &waProto.DocumentMessage{
				Mimetype: proto.String("application/pdf"), FileName: proto.String("invoice.pdf"),
				FileLength: proto.Uint64(90_000), PageCount: proto.Uint32(3), Caption: proto.String("as promised"),
			},
		}}}, KindDocument, "as promised", &MediaInfo{Mime: "application/pdf", FileName: "invoice.pdf", Size: 90_000, Pages: 3}},
		{"sticker", &waProto.Message{StickerMessage: &waProto.StickerMessage{
			Mimetype: proto.String("image/webp"), Width: proto.Uint32(512), Height: proto.Uint32(512), FileLength: proto.Uint64(20_000),
		}}, KindSticker, "", &MediaInfo{Mime: "image/webp", Size: 20_000, Width: 512, Height: 512}},
		{"location", &waProto.Message{LocationMessage: &waProto.LocationMessage{
			DegreesLatitude: proto.Float64(34.0837), DegreesLongitude: proto.Float64(74.7973),
			Name: proto.String("Lal Chowk"), Address: proto.String("Srinagar"),
		}}, KindLocation, "Lal Chowk\nSrinagar\nhttps://maps.google.com/?q=34.0837,74.7973", nil},
		{"live location", &waProto.Message{LiveLocationMessage: &waProto.LiveLocationMessage{
			DegreesLatitude: proto.Float64(-1.5), DegreesLongitude: proto.Float64(36.25),
		}}, KindLocation, "Live location\nhttps://maps.google.com/?q=-1.5,36.25", nil},
		{"contact", &waProto.Message{ContactMessage: &waProto.ContactMessage{
			DisplayName: proto.String("Bilal"),
			Vcard:       proto.String("BEGIN:VCARD\nVERSION:3.0\nFN:Bilal\nitem1.TEL;waid=919000000002:+91 90000 00002\nEND:VCARD"),
		}}, KindContact, "Bilal: +91 90000 00002", nil},
		{"contacts", &waProto.Message{ContactsArrayMessage: &waProto.ContactsArrayMessage{Contacts: []*waProto.ContactMessage{
			{DisplayName: proto.String("A"), Vcard: proto.String("TEL;type=CELL:+1 555")},
			{DisplayName: proto.String("B")},
		}}}, KindContact, "A: +1 555\nB", nil},
		{"template", &waProto.Message{TemplateMessage: &waProto.TemplateMessage{
			HydratedTemplate: &waProto.TemplateMessage_HydratedFourRowTemplate{
				HydratedContentText: proto.String("Your OTP is 4821"), HydratedFooterText: proto.String("Do not share it"),
			},
		}}, KindText, "Your OTP is 4821\nDo not share it", nil},
		{"buttons", &waProto.Message{ButtonsMessage: &waProto.ButtonsMessage{
			ContentText: proto.String("Confirm order?"),
			Buttons: []*waProto.ButtonsMessage_Button{
				{ButtonText: &waProto.ButtonsMessage_Button_ButtonText{DisplayText: proto.String("Yes")}},
				{ButtonText: &waProto.ButtonsMessage_Button_ButtonText{DisplayText: proto.String("No")}},
			},
		}}, KindText, "Confirm order?\n[Yes] [No]", nil},
		{"list", &waProto.Message{ListMessage: &waProto.ListMessage{
			Title: proto.String("Menu"), Description: proto.String("Pick one"),
			Sections: []*waProto.ListMessage_Section{{Rows: []*waProto.ListMessage_Row{
				{Title: proto.String("Tea"), Description: proto.String("hot")}, {Title: proto.String("Coffee")},
			}}},
		}}, KindText, "Menu\nPick one\n• Tea — hot\n• Coffee", nil},
		{"empty list", &waProto.Message{ListMessage: &waProto.ListMessage{}}, KindUnsupported, "[list]", nil},
		{"interactive", &waProto.Message{InteractiveMessage: &waProto.InteractiveMessage{
			Body: &waProto.InteractiveMessage_Body{Text: proto.String("Track your parcel")},
		}}, KindText, "Track your parcel", nil},
		{"button reply", &waProto.Message{TemplateButtonReplyMessage: &waProto.TemplateButtonReplyMessage{
			SelectedDisplayText: proto.String("Yes"),
		}}, KindText, "Yes", nil},
		{"payment", &waProto.Message{SendPaymentMessage: &waProto.SendPaymentMessage{NoteMessage: text("rent")}}, KindSystem, "Payment: rent", nil},
		{"payment request", &waProto.Message{RequestPaymentMessage: &waProto.RequestPaymentMessage{}}, KindSystem, "Payment request", nil},
		{"missed call", &waProto.Message{CallLogMesssage: &waProto.CallLogMessage{
			IsVideo: proto.Bool(true), CallOutcome: waProto.CallLogMessage_MISSED.Enum(),
		}}, KindSystem, "Missed video call", nil},
		{"call", &waProto.Message{CallLogMesssage: &waProto.CallLogMessage{DurationSecs: proto.Int64(125)}}, KindSystem, "Voice call (2:05)", nil},
		{"group invite", &waProto.Message{GroupInviteMessage: &waProto.GroupInviteMessage{GroupName: proto.String("Trek 2026")}}, KindUnsupported, "[group invite] Trek 2026", nil},
		{"event", &waProto.Message{EventMessage: &waProto.EventMessage{Name: proto.String("Wedding")}}, KindUnsupported, "[event] Wedding", nil},
		{"something new", &waProto.Message{NewsletterAdminInviteMessage: &waProto.NewsletterAdminInviteMessage{}}, KindUnsupported, "[newsletterAdminInvite]", nil},
	}
	for _, c := range cases {
		v, ok := tc.view(context.Background(), stored("X", c.msg))
		if !ok {
			t.Errorf("%s: hidden, want a view", c.name)
			continue
		}
		if v.Kind != c.kind || v.Body != c.body {
			t.Errorf("%s: kind %q body %q, want kind %q body %q", c.name, v.Kind, v.Body, c.kind, c.body)
		}
		switch {
		case c.media == nil && v.Media != nil:
			t.Errorf("%s: unexpected media %+v", c.name, v.Media)
		case c.media != nil && (v.Media == nil || *v.Media != *c.media):
			t.Errorf("%s: media %+v, want %+v", c.name, v.Media, c.media)
		}
		if v.Sender != "Asha" {
			t.Errorf("%s: sender %q, want the resolved name", c.name, v.Sender)
		}
	}
}

func TestView_OwnDeletedAndBare(t *testing.T) {
	tc := newStoreClient(t)
	ctx := context.Background()

	own := stored("O1", text("mine"))
	own.FromMe, own.SenderJID, own.Status = true, "me", msgstore.StatusRead
	if v, _ := tc.view(ctx, own); v.Sender != "" || v.Status != msgstore.StatusRead {
		t.Errorf("own message: sender %q status %v", v.Sender, v.Status)
	}

	deleted := msgstore.Message{ID: "D1", ChatJID: ashaPN.String(), SenderJID: ashaPN.String(), Deleted: true}
	if v, ok := tc.view(ctx, deleted); !ok || v.Kind != KindDeleted || v.Media != nil || v.Poll != nil {
		t.Errorf("deleted message view = %+v ok=%v", v, ok)
	}

	// A row with no content recorded falls back to its text and media type.
	bare := msgstore.Message{ID: "B1", ChatJID: ashaPN.String(), FromMe: true, SenderJID: "me", Text: "caption", MediaType: "image"}
	if v, ok := tc.view(ctx, bare); !ok || v.Kind != KindImage || v.Body != "caption" || v.Media == nil {
		t.Errorf("bare row view = %+v ok=%v", v, ok)
	}

	// Rows that are not messages of their own are hidden.
	for name, msg := range map[string]*waProto.Message{
		"reaction": reaction("X", "👍"),
		"edit":     edit("X", text("y")),
		"revoke":   revoke("X"),
		"noise":    {SenderKeyDistributionMessage: &waProto.SenderKeyDistributionMessage{}},
		"album":    {AlbumMessage: &waProto.AlbumMessage{}},
	} {
		if _, ok := tc.view(ctx, stored("H", msg)); ok {
			t.Errorf("%s row is shown, want it hidden", name)
		}
	}
}

func TestView_QuoteForwardMentions(t *testing.T) {
	tc := newStoreClient(t)
	tc.contacts.set(ashaPN, types.ContactInfo{FullName: "Asha"})
	tc.contacts.set(bilalPN, types.ContactInfo{PushName: "Bilal B"})
	ctx := context.Background()

	reply := &waProto.Message{ExtendedTextMessage: &waProto.ExtendedTextMessage{
		Text: proto.String("@" + meLID.User + " and @" + bilalPN.User + " see this"),
		ContextInfo: &waProto.ContextInfo{
			StanzaID:    proto.String("Q1"),
			Participant: proto.String(bilalPN.String()),
			QuotedMessage: &waProto.Message{ImageMessage: &waProto.ImageMessage{
				Caption: proto.String("the\nreceipt"),
			}},
			MentionedJID:    []string{meLID.String(), bilalPN.String()},
			IsForwarded:     proto.Bool(true),
			ForwardingScore: proto.Uint32(2),
		},
	}}
	v, ok := tc.view(ctx, stored("M1", reply))
	if !ok {
		t.Fatal("reply is hidden")
	}
	if v.Body != "@You and @Bilal B see this" {
		t.Errorf("body = %q, want mentions as names", v.Body)
	}
	if !v.Mentioned || !v.Forwarded {
		t.Errorf("mentioned=%v forwarded=%v, want both", v.Mentioned, v.Forwarded)
	}
	if v.Quote == nil || v.Quote.ID != "Q1" || v.Quote.Sender != "Bilal B" || v.Quote.Text != "[image] the receipt" {
		t.Errorf("quote = %+v", v.Quote)
	}

	// Quoting one of your own messages (named by your LID here).
	own := &waProto.Message{ExtendedTextMessage: &waProto.ExtendedTextMessage{
		Text: proto.String("ok"),
		ContextInfo: &waProto.ContextInfo{
			StanzaID: proto.String("Q2"), Participant: proto.String(meLID.String()), QuotedMessage: text("shall we?"),
		},
	}}
	if v, _ := tc.view(ctx, stored("M2", own)); v.Quote == nil || v.Quote.Sender != "You" || v.Quote.Text != "shall we?" || v.Mentioned || v.Forwarded {
		t.Errorf("quote of own message = %+v (view %+v)", v.Quote, v)
	}

	// A reply that doesn't carry the quoted content: use the stored original.
	original := stored("Q3", &waProto.Message{AudioMessage: &waProto.AudioMessage{PTT: proto.Bool(true)}})
	if _, err := tc.msgs.Insert(original); err != nil {
		t.Fatal(err)
	}
	bare := &waProto.Message{ExtendedTextMessage: &waProto.ExtendedTextMessage{
		Text: proto.String("can't listen now"), ContextInfo: &waProto.ContextInfo{StanzaID: proto.String("Q3")},
	}}
	if v, _ := tc.view(ctx, stored("M3", bare)); v.Quote == nil || v.Quote.Sender != "Asha" || v.Quote.Text != "[voice message]" {
		t.Errorf("quote from the stored original = %+v", v.Quote)
	}

	// Not a reply.
	if v, _ := tc.view(ctx, stored("M4", text("plain"))); v.Quote != nil {
		t.Errorf("plain message has a quote: %+v", v.Quote)
	}
}

func TestView_Reactions(t *testing.T) {
	tc := newStoreClient(t)
	tc.contacts.set(ashaPN, types.ContactInfo{FullName: "Asha"})
	tc.contacts.set(bilalPN, types.ContactInfo{FullName: "Bilal"})
	m := stored("M1", text("party?"))
	m.Reactions = []msgstore.Reaction{
		{SenderJID: bilalPN.String(), Emoji: "❤️", Timestamp: 10},
		{SenderJID: "me", Emoji: "👍", Timestamp: 20},
		// Asha, recorded once under each of her addresses: the newer counts.
		{SenderJID: ashaLID.String(), Emoji: "😮", Timestamp: 30},
		{SenderJID: ashaPN.String(), Emoji: "👍", Timestamp: 40},
		{SenderJID: strangerLID.String(), Emoji: "👍", Timestamp: 50},
	}
	v, _ := tc.view(context.Background(), m)
	if len(v.Reactions) != 2 {
		t.Fatalf("reactions = %+v, want two emoji", v.Reactions)
	}
	top, second := v.Reactions[0], v.Reactions[1]
	if top.Emoji != "👍" || top.Count != 3 || !top.Mine {
		t.Errorf("most used = %+v, want 👍 x3 including mine", top)
	}
	if got := strings.Join(top.Names, ","); got != "You,Asha,"+strangerLID.User {
		t.Errorf("names = %q", got)
	}
	if second.Emoji != "❤️" || second.Count != 1 || second.Mine || second.Names[0] != "Bilal" {
		t.Errorf("second = %+v", second)
	}
}

func TestView_SinglePollAndUnknownHashes(t *testing.T) {
	tc := newStoreClient(t)
	poll := stored("P1", &waProto.Message{PollCreationMessageV5: &waProto.PollCreationMessage{
		Name:                   proto.String("Which day?"),
		Options:                []*waProto.PollCreationMessage_Option{{OptionName: proto.String("Sat")}, {OptionName: proto.String("Sun")}},
		SelectableOptionsCount: proto.Uint32(1),
	}})
	hexHash := func(s string) string { return fmt.Sprintf("%x", optionHash(s)) }
	poll.PollVotes = []msgstore.PollVote{
		{VoterJID: "me", Options: []string{hexHash("Sun")}, Timestamp: 1},
		{VoterJID: bilalPN.String(), Options: []string{strings.ToUpper(hexHash("Sun")), hexHash("Sun"), "deadbeef"}, Timestamp: 2},
	}
	v, ok := tc.view(context.Background(), poll)
	if !ok || v.Kind != KindPoll || v.Poll == nil {
		t.Fatalf("view = %+v", v)
	}
	if v.Poll.MultiSelect {
		t.Error("a one-choice poll is shown as multi-select")
	}
	if v.Poll.Options[0].Votes != 0 || v.Poll.Options[1].Votes != 2 || !v.Poll.Options[1].Mine {
		t.Errorf("options = %+v, want Sun 2 (mine), each voter counted once", v.Poll.Options)
	}

	// The wrapped poll variant is found too.
	v4 := stored("P2", &waProto.Message{PollCreationMessageV4: &waProto.FutureProofMessage{Message: &waProto.Message{
		PollCreationMessage: &waProto.PollCreationMessage{Name: proto.String("Wrapped?")},
	}}})
	if v, ok := tc.view(context.Background(), v4); !ok || v.Kind != KindPoll || v.Poll.Question != "Wrapped?" {
		t.Errorf("wrapped poll view = %+v", v)
	}
}

func TestPreviewText(t *testing.T) {
	long := strings.Repeat("ab ", 60)
	cases := []struct {
		name string
		msg  *waProto.Message
		want string
	}{
		{"text", text("see you\n\n  at 5"), "see you at 5"},
		{"photo", imageMsg("/p", ""), "📷 Photo"},
		{"photo with caption", imageMsg("/p", "the lake"), "📷 the lake"},
		{"video", &waProto.Message{VideoMessage: &waProto.VideoMessage{}}, "🎥 Video"},
		{"gif", &waProto.Message{VideoMessage: &waProto.VideoMessage{GifPlayback: proto.Bool(true)}}, "🎬 GIF"},
		{"voice", &waProto.Message{AudioMessage: &waProto.AudioMessage{PTT: proto.Bool(true), Seconds: proto.Uint32(65)}}, "🎤 Voice message (1:05)"},
		{"audio", &waProto.Message{AudioMessage: &waProto.AudioMessage{}}, "🎵 Audio"},
		{"document", &waProto.Message{DocumentMessage: &waProto.DocumentMessage{FileName: proto.String("file.pdf")}}, "📄 file.pdf"},
		{"document with caption", &waProto.Message{DocumentMessage: &waProto.DocumentMessage{FileName: proto.String("file.pdf"), Caption: proto.String("read it")}}, "📄 file.pdf · read it"},
		{"sticker", &waProto.Message{StickerMessage: &waProto.StickerMessage{}}, "💟 Sticker"},
		{"location", &waProto.Message{LocationMessage: &waProto.LocationMessage{Name: proto.String("Home")}}, "📍 Home"},
		{"unnamed location", &waProto.Message{LocationMessage: &waProto.LocationMessage{}}, "📍 Location"},
		{"contact", &waProto.Message{ContactMessage: &waProto.ContactMessage{DisplayName: proto.String("Bilal"), Vcard: proto.String("TEL:+1")}}, "👤 Bilal"},
		{"poll", &waProto.Message{PollCreationMessage: &waProto.PollCreationMessage{Name: proto.String("Lunch?")}}, "📊 Lunch?"},
		{"long text", text(long), strings.TrimRight(long[:80], " ") + "…"},
	}
	for _, c := range cases {
		if got := analyze(unwrapMessage(c.msg)).previewText(); got != c.want {
			t.Errorf("%s: preview %q, want %q", c.name, got, c.want)
		}
	}
}

func TestOneLine_CutsBetweenCharacters(t *testing.T) {
	// 3-byte letters and 4-byte emoji: a byte cut at 80 would split one.
	text := strings.Repeat("नमस्ते 🙂 ", 40)
	got := oneLine(text, 80)
	if !utf8.ValidString(got) {
		t.Fatalf("not valid UTF-8: %q", got)
	}
	if n := utf8.RuneCountInString(got); n > 81 || !strings.HasSuffix(got, "…") {
		t.Errorf("got %d runes (%q), want at most 80 and an ellipsis", n, got)
	}
	if !strings.HasPrefix(text, strings.TrimSuffix(got, "…")) {
		t.Errorf("the cut changed the text: %q", got)
	}
	if got := oneLine("short", 80); got != "short" {
		t.Errorf("short text changed: %q", got)
	}
	if got := oneLine(strings.Repeat("é", 80), 80); got != strings.Repeat("é", 80) {
		t.Errorf("text of exactly the limit was cut: %q", got)
	}
}

func TestPreviewFor_GroupSenderAndMentions(t *testing.T) {
	tc := newStoreClient(t)
	tc.contacts.set(ashaPN, types.ContactInfo{FullName: "Asha"})
	ctx := context.Background()

	msg := &waProto.Message{ExtendedTextMessage: &waProto.ExtendedTextMessage{
		Text:        proto.String("@" + ashaLID.User + " come"),
		ContextInfo: &waProto.ContextInfo{MentionedJID: []string{ashaLID.String()}},
	}}
	row := stored("M1", msg)
	row.ChatJID = groupJID.String()
	if got := tc.previewFor(ctx, row, analyze(contentOf(row))); got != "Asha: @Asha come" {
		t.Errorf("group preview = %q", got)
	}
	row.FromMe, row.SenderJID = true, "me"
	if got := tc.previewFor(ctx, row, analyze(contentOf(row))); got != "You: @Asha come" {
		t.Errorf("own group preview = %q", got)
	}
	row.ChatJID = ashaPN.String()
	if got := tc.previewFor(ctx, row, analyze(contentOf(row))); got != "@Asha come" {
		t.Errorf("1:1 preview = %q", got)
	}
	row.Deleted = true
	if got := tc.previewFor(ctx, row, analysis{Class: classDrop}); got != "🚫 This message was deleted" {
		t.Errorf("deleted preview = %q", got)
	}
}

func TestChats_TitlesAndOrder(t *testing.T) {
	tc := newStoreClient(t)
	tc.contacts.set(ashaPN, types.ContactInfo{FullName: "Asha Koul", PushName: "ash"})
	tc.contacts.set(strangerLID, types.ContactInfo{PushName: "Stranger"})
	ctx := context.Background()

	for _, chat := range []chatstore.Chat{
		{JID: ashaPN.String(), Name: "ash", LastMessageAt: 300},
		{JID: bilalPN.String(), LastMessageAt: 500},
		{JID: groupJID.String(), Name: "Family", IsGroup: true, LastMessageAt: 100},
		{JID: "120363000000000002@g.us", IsGroup: true, LastMessageAt: 50},
		{JID: strangerLID.String(), LastMessageAt: 400},
		{JID: "200000000000777@lid", LastMessageAt: 20},
	} {
		if err := tc.chats.Upsert(chat); err != nil {
			t.Fatal(err)
		}
	}
	if err := tc.chats.SetPinned(groupJID.String(), true); err != nil {
		t.Fatal(err)
	}
	if err := tc.chats.SetArchived(ashaPN.String(), true); err != nil {
		t.Fatal(err)
	}

	views, err := tc.Chats(ctx)
	if err != nil {
		t.Fatalf("Chats: %v", err)
	}
	var got []string
	for _, v := range views {
		if v.Title == "" {
			t.Errorf("chat %s has an empty title", v.JID)
		}
		got = append(got, v.Title)
	}
	want := []string{
		"Family",             // pinned first
		"+91 90000 00002",    // then newest first; no name known: the phone number
		"Stranger",           // push name of an @lid contact
		"Asha Koul",          // saved name beats the stored push name
		"120363000000000002", // a group whose subject isn't known yet
		"200000000000777",    // an @lid nobody knows anything about
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("titles in order = %q\nwant %q", got, want)
	}
	if !views[3].Archived {
		t.Error("archived chats are returned, flagged")
	}

	one, ok, err := tc.Chat(ctx, ashaLID.String())
	if err != nil || !ok || one.JID != ashaPN.String() || one.Title != "Asha Koul" {
		t.Errorf("Chat by the @lid address = %+v ok=%v err=%v", one, ok, err)
	}
	if _, ok, _ := tc.Chat(ctx, "nobody@s.whatsapp.net"); ok {
		t.Error("an unknown chat was found")
	}
}

func TestMessages_PagingAndSearch(t *testing.T) {
	tc := newStoreClient(t)
	ctx := context.Background()
	chat := ashaPN.String()

	var rows []msgstore.Message
	for i := 0; i < 120; i++ {
		m := stored(fmt.Sprintf("M%03d", i), text(fmt.Sprintf("message %d", i)))
		m.Timestamp = int64(1000 + i)
		rows = append(rows, m)
		// Leftover rows that aren't messages, scattered through the chat.
		if i%10 == 0 {
			hidden := stored(fmt.Sprintf("H%03d", i), reaction("NOPE", "👍"))
			hidden.Timestamp = int64(1000 + i)
			rows = append(rows, hidden)
		}
	}
	if _, err := tc.msgs.InsertMany(rows); err != nil {
		t.Fatal(err)
	}

	page, err := tc.Messages(ctx, chat, nil, 50)
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	if len(page) != 50 || page[0].ID != "M070" || page[49].ID != "M119" {
		t.Fatalf("newest page: %d messages from %s to %s, want 50 from M070 to M119", len(page), page[0].ID, page[len(page)-1].ID)
	}
	older, err := tc.Messages(ctx, chat, &page[0].Message, 50)
	if err != nil {
		t.Fatalf("Messages(before): %v", err)
	}
	if len(older) != 50 || older[0].ID != "M020" || older[49].ID != "M069" {
		t.Fatalf("older page: %d messages from %s to %s, want 50 from M020 to M069", len(older), older[0].ID, older[len(older)-1].ID)
	}
	oldest, err := tc.Messages(ctx, chat, &older[0].Message, 50)
	if err != nil || len(oldest) != 20 || oldest[0].ID != "M000" || oldest[19].ID != "M019" {
		t.Fatalf("oldest page: %d messages, err %v", len(oldest), err)
	}
	if none, _ := tc.Messages(ctx, chat, &oldest[0].Message, 50); len(none) != 0 {
		t.Errorf("paging past the start returned %d messages", len(none))
	}
	// Asking by the @lid address finds the same chat.
	if viaLID, _ := tc.Messages(ctx, ashaLID.String(), nil, 5); len(viaLID) != 5 || viaLID[4].ID != "M119" {
		t.Errorf("Messages by @lid address = %d messages", len(viaLID))
	}

	found, err := tc.Search(ctx, chat, "MESSAGE 11", 50)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(found) != 11 || found[0].ID != "M119" || found[10].ID != "M011" {
		t.Errorf("search found %d messages, want the 11 containing it, newest first", len(found))
	}
	if all, _ := tc.Search(ctx, "", "message 5", 50); len(all) != 11 {
		t.Errorf("search across chats found %d, want 11", len(all))
	}

	if v, ok, err := tc.Message(ctx, chat, "M042"); err != nil || !ok || v.Body != "message 42" {
		t.Errorf("Message = %+v ok=%v err=%v", v, ok, err)
	}
	if _, ok, _ := tc.Message(ctx, chat, "GONE"); ok {
		t.Error("a message that isn't stored was found")
	}
	if _, ok, _ := tc.Message(ctx, chat, "H000"); ok {
		t.Error("a hidden row was returned as a message")
	}
}

func TestResolveName_CachedPerSender(t *testing.T) {
	tc := newStoreClient(t)
	tc.contacts.set(ashaPN, types.ContactInfo{FullName: "Asha"})
	tc.contacts.set(bilalPN, types.ContactInfo{PushName: "Bilal"})
	ctx := context.Background()

	var rows []msgstore.Message
	senders := []types.JID{ashaLID, bilalPN, strangerLID}
	for i := 0; i < 200; i++ {
		m := stored(fmt.Sprintf("M%03d", i), text("hi"))
		m.ChatJID, m.SenderJID, m.Timestamp = groupJID.String(), senders[i%3].String(), int64(i)
		rows = append(rows, m)
	}
	if _, err := tc.msgs.InsertMany(rows); err != nil {
		t.Fatal(err)
	}

	page, err := tc.Messages(ctx, groupJID.String(), nil, 200)
	if err != nil || len(page) != 200 {
		t.Fatalf("Messages: %d, %v", len(page), err)
	}
	if page[0].Sender != "Asha" || page[1].Sender != "Bilal" || page[2].Sender != strangerLID.User {
		t.Errorf("senders = %q, %q, %q", page[0].Sender, page[1].Sender, page[2].Sender)
	}
	first := tc.contacts.count()
	// A handful of store reads per distinct sender, not per message.
	if first == 0 || first > 3*len(senders) {
		t.Errorf("a 200-message page cost %d contact lookups for %d senders", first, len(senders))
	}
	if _, err := tc.Messages(ctx, groupJID.String(), nil, 200); err != nil {
		t.Fatal(err)
	}
	if again := tc.contacts.count(); again != first {
		t.Errorf("drawing the page again cost %d more lookups, want none", again-first)
	}

	// A changed name is picked up, and announced.
	tc.contacts.set(bilalPN, types.ContactInfo{FullName: "Bilal Ahmad", PushName: "Bilal"})
	tc.log.reset()
	tc.invalidateNames()
	tc.invalidateNames()
	tc.invalidateNames()
	if got := tc.ResolveName(ctx, bilalPN.String()); got != "Bilal Ahmad" {
		t.Errorf("after the change: %q", got)
	}
	time.Sleep(namesEventInterval + 300*time.Millisecond)
	names := 0
	for _, e := range tc.log.all() {
		if _, ok := e.(NamesEvent); ok {
			names++
		}
	}
	if names < 1 || names > 2 {
		t.Errorf("3 changes in a burst produced %d NamesEvents, want 1 now and at most 1 later", names)
	}
}
