package whatsapp

import (
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"unicode"

	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"

	"github.com/codebyoketch/wa-cli/internal/msgstore"
)

// This file is the one place that knows what the many WhatsApp message
// types mean: what is a message of its own, what merely modifies another
// message, what is plumbing, and how each real message is described.

// msgClass says what to do with a piece of message content.
type msgClass int

const (
	// classRow is a real message: it gets a row in the message store.
	classRow msgClass = iota
	// classEvent applies to another message (reaction, edit, delete, poll
	// vote, HD upgrade) or to the chat (disappearing-messages timer). It
	// is folded into its target and never stored as a row.
	classEvent
	// classDrop is plumbing nobody should see.
	classDrop
)

// analysis is everything derived from one message's content.
type analysis struct {
	Class msgClass
	// Kind is the MessageView kind.
	Kind string
	// MediaType is what msgstore.Message.MediaType holds: "image",
	// "video", "audio", "voice note", "document", "sticker", or "".
	MediaType string
	// Text is the message text or caption, with mentions unresolved. For
	// kinds without either it is a readable description.
	Text string
	// Ctx is the content's reply/mention/forward info, if it has any.
	Ctx   *waProto.ContextInfo
	Media *MediaInfo
	Poll  *waProto.PollCreationMessage
	// Thumb is the small picture embedded in the message (JPEG, or PNG for
	// stickers).
	Thumb []byte
	// Download is the attachment, for kinds that have one.
	Download whatsmeow.DownloadableMessage
}

// decodeRawProto reconstructs a stored message's original content from
// its base64-encoded protobuf, returning nil if unavailable or decoding
// fails (callers should degrade gracefully rather than error out — a
// reply/forward without the original quote attached is still useful).
func decodeRawProto(raw string) *waProto.Message {
	if raw == "" {
		return nil
	}
	data, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return nil
	}
	msg := &waProto.Message{}
	if err := proto.Unmarshal(data, msg); err != nil {
		return nil
	}
	return msg
}

// encodeRawProto is the inverse of decodeRawProto; "" if msg is nil or
// can't be serialized.
func encodeRawProto(msg *waProto.Message) string {
	if msg == nil {
		return ""
	}
	b, err := proto.Marshal(msg)
	if err != nil {
		return ""
	}
	return base64.StdEncoding.EncodeToString(b)
}

// contentOf returns a stored message's content with wrappers removed, or
// nil when the row has none.
func contentOf(m msgstore.Message) *waProto.Message {
	return unwrapMessage(decodeRawProto(m.RawProto))
}

// wrappers lists the envelope fields that carry the real message inside.
// associatedChildMessage is deliberately absent: it is folded into its
// parent (see fold.go).
var wrappers = []func(*waProto.Message) *waProto.Message{
	func(m *waProto.Message) *waProto.Message { return m.GetDeviceSentMessage().GetMessage() },
	func(m *waProto.Message) *waProto.Message { return m.GetBotInvokeMessage().GetMessage() },
	func(m *waProto.Message) *waProto.Message { return m.GetEphemeralMessage().GetMessage() },
	func(m *waProto.Message) *waProto.Message { return m.GetDocumentWithCaptionMessage().GetMessage() },
	func(m *waProto.Message) *waProto.Message { return m.GetLottieStickerMessage().GetMessage() },
	func(m *waProto.Message) *waProto.Message { return m.GetEditedMessage().GetMessage() },
	func(m *waProto.Message) *waProto.Message { return m.GetGroupMentionedMessage().GetMessage() },
	func(m *waProto.Message) *waProto.Message { return m.GetPollCreationMessageV4().GetMessage() },
	func(m *waProto.Message) *waProto.Message { return m.GetSpoilerMessage().GetMessage() },
	func(m *waProto.Message) *waProto.Message { return m.GetStatusMentionMessage().GetMessage() },
	func(m *waProto.Message) *waProto.Message { return m.GetGroupStatusMentionMessage().GetMessage() },
	func(m *waProto.Message) *waProto.Message { return m.GetGroupStatusMessage().GetMessage() },
	func(m *waProto.Message) *waProto.Message { return m.GetGroupStatusMessageV2().GetMessage() },
	func(m *waProto.Message) *waProto.Message { return m.GetEventCoverImage().GetMessage() },
	func(m *waProto.Message) *waProto.Message { return m.GetBotForwardedMessage().GetMessage() },
	func(m *waProto.Message) *waProto.Message { return m.GetQuestionMessage().GetMessage() },
	func(m *waProto.Message) *waProto.Message { return m.GetQuestionReplyMessage().GetMessage() },
	func(m *waProto.Message) *waProto.Message { return m.GetLimitSharingMessage().GetMessage() },
}

var viewOnceWrappers = []func(*waProto.Message) *waProto.Message{
	func(m *waProto.Message) *waProto.Message { return m.GetViewOnceMessage().GetMessage() },
	func(m *waProto.Message) *waProto.Message { return m.GetViewOnceMessageV2().GetMessage() },
	func(m *waProto.Message) *waProto.Message { return m.GetViewOnceMessageV2Extension().GetMessage() },
}

// unwrapMessage peels the envelopes WhatsApp puts around the real content
// (sent-from-another-device, disappearing, view-once, document with
// caption, ...). View-once is remembered on the media itself. msg is
// returned unchanged when it isn't wrapped; nil stays nil.
func unwrapMessage(msg *waProto.Message) *waProto.Message {
	for depth := 0; msg != nil && depth < 8; depth++ {
		inner, viewOnce := peel(msg)
		if inner == nil {
			break
		}
		if inner.MessageContextInfo == nil && msg.MessageContextInfo != nil {
			inner.MessageContextInfo = msg.MessageContextInfo
		}
		if viewOnce {
			markViewOnce(inner)
		}
		msg = inner
	}
	return msg
}

func peel(msg *waProto.Message) (inner *waProto.Message, viewOnce bool) {
	for _, get := range viewOnceWrappers {
		if inner := get(msg); inner != nil {
			return inner, true
		}
	}
	for _, get := range wrappers {
		if inner := get(msg); inner != nil {
			return inner, false
		}
	}
	return nil, false
}

// markViewOnce records on the media itself that it came in a view-once
// envelope, so the fact survives storing only the inner message.
func markViewOnce(msg *waProto.Message) {
	switch {
	case msg.GetImageMessage() != nil:
		msg.ImageMessage.ViewOnce = proto.Bool(true)
	case msg.GetVideoMessage() != nil:
		msg.VideoMessage.ViewOnce = proto.Bool(true)
	case msg.GetAudioMessage() != nil:
		msg.AudioMessage.ViewOnce = proto.Bool(true)
	}
}

// pollOf returns msg's poll, whichever of the poll fields carries it.
func pollOf(msg *waProto.Message) *waProto.PollCreationMessage {
	for _, p := range []*waProto.PollCreationMessage{
		msg.GetPollCreationMessage(),
		msg.GetPollCreationMessageV2(),
		msg.GetPollCreationMessageV3(),
		msg.GetPollCreationMessageV5(),
		msg.GetPollCreationMessageV6(),
	} {
		if p != nil {
			return p
		}
	}
	return nil
}

// analyze classifies and describes msg, which must already be unwrapped
// (see unwrapMessage / contentOf).
func analyze(msg *waProto.Message) analysis {
	if msg == nil {
		return analysis{Class: classDrop}
	}

	// Things that modify another message, or the chat.
	switch {
	case msg.GetReactionMessage() != nil,
		msg.GetEncReactionMessage() != nil,
		msg.GetPollUpdateMessage() != nil,
		msg.GetSecretEncryptedMessage() != nil,
		msg.GetAssociatedChildMessage() != nil:
		return analysis{Class: classEvent}
	}
	if pm := msg.GetProtocolMessage(); pm != nil {
		switch pm.GetType() {
		case waProto.ProtocolMessage_REVOKE:
			// REVOKE is enum value 0, which is also what an unset type
			// reads as: require the key of the message being deleted.
			if pm.GetKey().GetID() != "" {
				return analysis{Class: classEvent}
			}
		case waProto.ProtocolMessage_MESSAGE_EDIT, waProto.ProtocolMessage_EPHEMERAL_SETTING:
			return analysis{Class: classEvent}
		}
		return analysis{Class: classDrop}
	}

	// Plumbing and announcements that aren't messages.
	switch {
	case msg.GetAlbumMessage() != nil, // "N photos": its children are ordinary messages
		msg.GetPinInChatMessage() != nil,
		msg.GetKeepInChatMessage() != nil,
		msg.GetEncCommentMessage() != nil,
		msg.GetCommentMessage() != nil,
		msg.GetMessageHistoryNotice() != nil,
		msg.GetMessageHistoryBundle() != nil,
		msg.GetEncEventResponseMessage() != nil,
		msg.GetStickerSyncRmrMessage() != nil,
		msg.GetPollAddOptionMessage() != nil,
		msg.GetGroupRootKeyShare() != nil,
		msg.GetRootSecretDistributeMessage() != nil,
		msg.GetStatusNotificationMessage() != nil,
		msg.GetChat() != nil:
		return analysis{Class: classDrop}
	}

	a := analysis{Class: classRow, Kind: KindText}
	switch {
	case msg.GetConversation() != "":
		a.Text = msg.GetConversation()

	case msg.GetExtendedTextMessage() != nil:
		m := msg.GetExtendedTextMessage()
		a.Text, a.Ctx = m.GetText(), m.GetContextInfo()

	case msg.GetImageMessage() != nil:
		m := msg.GetImageMessage()
		a.Kind, a.MediaType = KindImage, "image"
		a.Text, a.Ctx, a.Thumb, a.Download = m.GetCaption(), m.GetContextInfo(), m.GetJPEGThumbnail(), m
		a.Media = &MediaInfo{
			Mime: m.GetMimetype(), Size: m.GetFileLength(),
			Width: int(m.GetWidth()), Height: int(m.GetHeight()),
			ViewOnce: m.GetViewOnce(),
		}

	case msg.GetVideoMessage() != nil, msg.GetPtvMessage() != nil:
		m := msg.GetVideoMessage()
		if m == nil {
			m = msg.GetPtvMessage() // a round "video note"
		}
		a.Kind, a.MediaType = KindVideo, "video"
		if m.GetGifPlayback() {
			a.Kind = KindGIF
		}
		a.Text, a.Ctx, a.Thumb, a.Download = m.GetCaption(), m.GetContextInfo(), m.GetJPEGThumbnail(), m
		a.Media = &MediaInfo{
			Mime: m.GetMimetype(), Size: m.GetFileLength(),
			Width: int(m.GetWidth()), Height: int(m.GetHeight()),
			Seconds: int(m.GetSeconds()), ViewOnce: m.GetViewOnce(),
		}

	case msg.GetAudioMessage() != nil:
		m := msg.GetAudioMessage()
		a.Kind, a.MediaType = KindAudio, "audio"
		if m.GetPTT() {
			a.Kind, a.MediaType = KindVoice, "voice note"
		}
		a.Ctx, a.Download = m.GetContextInfo(), m
		a.Media = &MediaInfo{
			Mime: m.GetMimetype(), Size: m.GetFileLength(),
			Seconds: int(m.GetSeconds()), ViewOnce: m.GetViewOnce(),
		}

	case msg.GetDocumentMessage() != nil:
		m := msg.GetDocumentMessage()
		a.Kind, a.MediaType = KindDocument, "document"
		a.Text, a.Ctx, a.Thumb, a.Download = m.GetCaption(), m.GetContextInfo(), m.GetJPEGThumbnail(), m
		name := m.GetFileName()
		if name == "" {
			name = m.GetTitle()
		}
		a.Media = &MediaInfo{
			Mime: m.GetMimetype(), FileName: name, Size: m.GetFileLength(),
			Pages: int(m.GetPageCount()),
		}

	case msg.GetStickerMessage() != nil:
		m := msg.GetStickerMessage()
		a.Kind, a.MediaType = KindSticker, "sticker"
		a.Ctx, a.Thumb, a.Download = m.GetContextInfo(), m.GetPngThumbnail(), m
		a.Media = &MediaInfo{
			Mime: m.GetMimetype(), Size: m.GetFileLength(),
			Width: int(m.GetWidth()), Height: int(m.GetHeight()),
		}

	case msg.GetLocationMessage() != nil:
		m := msg.GetLocationMessage()
		a.Kind, a.Ctx, a.Thumb = KindLocation, m.GetContextInfo(), m.GetJPEGThumbnail()
		a.Text = locationText(m.GetDegreesLatitude(), m.GetDegreesLongitude(), m.GetName(), m.GetAddress(), m.GetComment())

	case msg.GetLiveLocationMessage() != nil:
		m := msg.GetLiveLocationMessage()
		a.Kind, a.Ctx, a.Thumb = KindLocation, m.GetContextInfo(), m.GetJPEGThumbnail()
		a.Text = locationText(m.GetDegreesLatitude(), m.GetDegreesLongitude(), "Live location", m.GetCaption())

	case msg.GetContactMessage() != nil:
		m := msg.GetContactMessage()
		a.Kind, a.Ctx = KindContact, m.GetContextInfo()
		a.Text = contactText(m)

	case msg.GetContactsArrayMessage() != nil:
		m := msg.GetContactsArrayMessage()
		a.Kind, a.Ctx = KindContact, m.GetContextInfo()
		var lines []string
		for _, ct := range m.GetContacts() {
			lines = append(lines, contactText(ct))
		}
		a.Text = strings.Join(lines, "\n")
		if a.Text == "" {
			a.Text = m.GetDisplayName()
		}

	case pollOf(msg) != nil:
		a.Poll = pollOf(msg)
		a.Kind, a.Ctx, a.Text = KindPoll, a.Poll.GetContextInfo(), a.Poll.GetName()

	case msg.GetTemplateMessage() != nil:
		m := msg.GetTemplateMessage()
		a.Ctx = m.GetContextInfo()
		a.text(templateText(m), "[template]")

	case msg.GetHighlyStructuredMessage() != nil:
		a.text(templateText(msg.GetHighlyStructuredMessage().GetHydratedHsm()), "[template]")

	case msg.GetButtonsMessage() != nil:
		m := msg.GetButtonsMessage()
		a.Ctx = m.GetContextInfo()
		var buttons []string
		for _, b := range m.GetButtons() {
			if label := b.GetButtonText().GetDisplayText(); label != "" {
				buttons = append(buttons, "["+label+"]")
			}
		}
		a.text(joinLines(m.GetText(), m.GetContentText(), m.GetFooterText(), strings.Join(buttons, " ")), "[buttons]")

	case msg.GetListMessage() != nil:
		m := msg.GetListMessage()
		a.Ctx = m.GetContextInfo()
		lines := []string{m.GetTitle(), m.GetDescription()}
		for _, sec := range m.GetSections() {
			for _, row := range sec.GetRows() {
				lines = append(lines, "• "+joinWith(" — ", row.GetTitle(), row.GetDescription()))
			}
		}
		lines = append(lines, m.GetFooterText())
		a.text(joinLines(lines...), "[list]")

	case msg.GetInteractiveMessage() != nil:
		m := msg.GetInteractiveMessage()
		a.Ctx = m.GetContextInfo()
		a.text(interactiveText(m), "[interactive message]")

	case msg.GetButtonsResponseMessage() != nil:
		m := msg.GetButtonsResponseMessage()
		a.Ctx = m.GetContextInfo()
		a.text(m.GetSelectedDisplayText(), "[button reply]")

	case msg.GetTemplateButtonReplyMessage() != nil:
		m := msg.GetTemplateButtonReplyMessage()
		a.Ctx = m.GetContextInfo()
		a.text(m.GetSelectedDisplayText(), "[button reply]")

	case msg.GetListResponseMessage() != nil:
		m := msg.GetListResponseMessage()
		a.Ctx = m.GetContextInfo()
		a.text(joinLines(m.GetTitle(), m.GetDescription()), "[list reply]")

	case msg.GetInteractiveResponseMessage() != nil:
		m := msg.GetInteractiveResponseMessage()
		a.Ctx = m.GetContextInfo()
		a.text(m.GetBody().GetText(), "[reply]")

	case msg.GetRichResponseMessage() != nil:
		m := msg.GetRichResponseMessage()
		a.Ctx = m.GetContextInfo()
		var parts []string
		for _, sub := range m.GetSubmessages() {
			parts = append(parts, sub.GetMessageText())
		}
		a.text(joinLines(parts...), "[AI response]")

	case msg.GetSendPaymentMessage() != nil:
		a.Kind = KindSystem
		a.Text = joinWith(": ", "Payment", noteText(msg.GetSendPaymentMessage().GetNoteMessage()))

	case msg.GetRequestPaymentMessage() != nil:
		a.Kind = KindSystem
		a.Text = joinWith(": ", "Payment request", noteText(msg.GetRequestPaymentMessage().GetNoteMessage()))

	case msg.GetDeclinePaymentRequestMessage() != nil:
		a.Kind, a.Text = KindSystem, "Payment request declined"

	case msg.GetCancelPaymentRequestMessage() != nil:
		a.Kind, a.Text = KindSystem, "Payment request cancelled"

	case msg.GetPaymentInviteMessage() != nil:
		a.Kind, a.Text = KindSystem, "Payment invite"

	case msg.GetCallLogMesssage() != nil:
		a.Kind, a.Text = KindSystem, callLogText(msg.GetCallLogMesssage())

	case msg.GetCall() != nil, msg.GetBcallMessage() != nil:
		a.Kind, a.Text = KindSystem, "Call"

	case msg.GetScheduledCallCreationMessage() != nil:
		m := msg.GetScheduledCallCreationMessage()
		a.Kind, a.Text = KindSystem, joinWith(": ", "Scheduled call", m.GetTitle())

	case msg.GetScheduledCallEditMessage() != nil:
		a.Kind, a.Text = KindSystem, "Scheduled call updated"

	case msg.GetGroupInviteMessage() != nil:
		m := msg.GetGroupInviteMessage()
		a.Kind, a.Ctx, a.Thumb = KindUnsupported, m.GetContextInfo(), m.GetJPEGThumbnail()
		a.Text = joinLines(joinWith(" ", "[group invite]", m.GetGroupName()), m.GetCaption())

	case msg.GetEventMessage() != nil:
		m := msg.GetEventMessage()
		a.Kind, a.Ctx = KindUnsupported, m.GetContextInfo()
		a.Text = joinLines(joinWith(" ", "[event]", m.GetName()), m.GetDescription(), m.GetJoinLink())

	case msg.GetProductMessage() != nil:
		m := msg.GetProductMessage()
		a.Kind, a.Ctx = KindUnsupported, m.GetContextInfo()
		a.Text = joinLines(joinWith(" ", "[product]", m.GetProduct().GetTitle()), m.GetProduct().GetDescription(), m.GetBody())

	case msg.GetOrderMessage() != nil:
		m := msg.GetOrderMessage()
		a.Kind, a.Ctx = KindUnsupported, m.GetContextInfo()
		a.Text = joinLines(joinWith(" ", "[order]", m.GetOrderTitle()), m.GetMessage())

	case msg.GetPollResultSnapshotMessage() != nil, msg.GetPollResultSnapshotMessageV3() != nil:
		m := msg.GetPollResultSnapshotMessage()
		if m == nil {
			m = msg.GetPollResultSnapshotMessageV3()
		}
		a.Kind, a.Ctx = KindUnsupported, m.GetContextInfo()
		lines := []string{joinWith(" ", "[poll results]", m.GetName())}
		for _, v := range m.GetPollVotes() {
			lines = append(lines, fmt.Sprintf("• %s: %d", v.GetOptionName(), v.GetOptionVoteCount()))
		}
		a.Text = joinLines(lines...)

	case msg.GetStickerPackMessage() != nil:
		m := msg.GetStickerPackMessage()
		a.Kind, a.Ctx = KindUnsupported, m.GetContextInfo()
		a.Text = joinLines(joinWith(" ", "[sticker pack]", m.GetName()), m.GetCaption())

	case msg.GetPlaceholderMessage() != nil:
		a.Kind, a.Text = KindUnsupported, "[this message can only be seen on your phone]"

	default:
		name := firstFieldName(msg)
		if name == "" || name == "conversation" { // nothing, or an empty text
			return analysis{Class: classDrop}
		}
		a.Kind, a.Text = KindUnsupported, "["+name+"]"
	}
	if a.Media != nil {
		a.Media.HasThumb = len(a.Thumb) > 0
	}
	return a
}

// text sets a KindText body, or an unsupported placeholder when the
// message turned out to carry no text.
func (a *analysis) text(text, placeholder string) {
	if strings.TrimSpace(text) == "" {
		a.Kind, a.Text = KindUnsupported, placeholder
		return
	}
	a.Kind, a.Text = KindText, text
}

// joinLines joins the non-blank parts with newlines.
func joinLines(parts ...string) string { return joinWith("\n", parts...) }

// joinWith joins the non-blank parts with sep.
func joinWith(sep string, parts ...string) string {
	kept := parts[:0:0]
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, sep)
}

// locationText renders a location as its labels followed by a map link.
func locationText(lat, lng float64, labels ...string) string {
	link := "https://maps.google.com/?q=" +
		strconv.FormatFloat(lat, 'f', -1, 64) + "," + strconv.FormatFloat(lng, 'f', -1, 64)
	return joinLines(append(labels, link)...)
}

// contactText renders a shared contact as "Name: +phone".
func contactText(m *waProto.ContactMessage) string {
	return joinWith(": ", m.GetDisplayName(), vcardPhone(m.GetVcard()))
}

// vcardPhone returns the first phone number in a vCard, or "".
func vcardPhone(vcard string) string {
	for _, line := range strings.Split(vcard, "\n") {
		line = strings.TrimSpace(line)
		// "TEL;type=CELL;waid=919541988522:+91 95419 88522", possibly
		// prefixed with a group ("item1.TEL;...").
		head, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		head = strings.ToUpper(head)
		if dot := strings.LastIndex(head, "."); dot >= 0 && !strings.Contains(head[:dot], ";") {
			head = head[dot+1:]
		}
		if head == "TEL" || strings.HasPrefix(head, "TEL;") {
			if value = strings.TrimSpace(value); value != "" {
				return value
			}
		}
	}
	return ""
}

// templateText returns the readable text of a business template message.
func templateText(m *waProto.TemplateMessage) string {
	if m == nil {
		return ""
	}
	h := m.GetHydratedTemplate()
	if h == nil {
		h = m.GetHydratedFourRowTemplate()
	}
	if h == nil {
		return interactiveText(m.GetInteractiveMessageTemplate())
	}
	var header string
	switch {
	case h.GetImageMessage() != nil:
		header = "[image]"
	case h.GetVideoMessage() != nil:
		header = "[video]"
	case h.GetDocumentMessage() != nil:
		header = "[document]"
	case h.GetLocationMessage() != nil:
		header = "[location]"
	}
	return joinLines(header, h.GetHydratedTitleText(), h.GetHydratedContentText(), h.GetHydratedFooterText())
}

func interactiveText(m *waProto.InteractiveMessage) string {
	if m == nil {
		return ""
	}
	return joinLines(m.GetHeader().GetTitle(), m.GetHeader().GetSubtitle(), m.GetBody().GetText(), m.GetFooter().GetText())
}

// noteText is the text of the note attached to a payment.
func noteText(note *waProto.Message) string {
	a := analyze(unwrapMessage(note))
	if a.Class != classRow || a.Kind != KindText {
		return ""
	}
	return a.Text
}

func callLogText(m *waProto.CallLogMessage) string {
	what := "voice call"
	if m.GetIsVideo() {
		what = "video call"
	}
	switch m.GetCallOutcome() {
	case waProto.CallLogMessage_MISSED, waProto.CallLogMessage_SILENCED_BY_DND, waProto.CallLogMessage_SILENCED_UNKNOWN_CALLER:
		return "Missed " + what
	case waProto.CallLogMessage_REJECTED:
		return "Declined " + what
	case waProto.CallLogMessage_FAILED:
		return "Failed " + what
	}
	text := strings.ToUpper(what[:1]) + what[1:]
	if secs := m.GetDurationSecs(); secs > 0 {
		text += fmt.Sprintf(" (%d:%02d)", secs/60, secs%60)
	}
	return text
}

// previewMaxRunes is how much of a message the chat list shows.
const previewMaxRunes = 80

// oneLine collapses all whitespace runs (newlines included) into single
// spaces and cuts the result to max runes, adding "…" when cut. It cuts
// between runes, never inside a multi-byte character.
func oneLine(text string, max int) string {
	var b strings.Builder
	n := 0
	space := false
	for _, r := range text {
		if unicode.IsSpace(r) {
			space = b.Len() > 0
			continue
		}
		if space {
			if n++; n > max {
				break
			}
			b.WriteByte(' ')
			space = false
		}
		if n++; n > max {
			break
		}
		b.WriteRune(r)
	}
	if n > max {
		return strings.TrimRight(b.String(), " ") + "…"
	}
	return b.String()
}

// extractPreview trims plain conversational text for display.
func extractPreview(msg interface{ GetConversation() string }) string {
	if msg == nil {
		return ""
	}
	text := msg.GetConversation()
	if runes := []rune(text); len(runes) > previewMaxRunes {
		text = string(runes[:previewMaxRunes]) + "…"
	}
	return text
}

// previewText is the chat-list line for a message: its text, or a symbol
// and label for media ("📷 Photo", "🎤 Voice message", "📄 file.pdf"),
// with the caption in place of the label when there is one.
func (a analysis) previewText() string {
	label := func(symbol, fallback string) string {
		if strings.TrimSpace(a.Text) != "" {
			return symbol + " " + a.Text
		}
		return symbol + " " + fallback
	}
	var text string
	switch a.Kind {
	case KindImage:
		text = label("📷", "Photo")
	case KindVideo:
		text = label("🎥", "Video")
	case KindGIF:
		text = label("🎬", "GIF")
	case KindAudio:
		text = "🎵 Audio"
	case KindVoice:
		text = "🎤 Voice message"
		if a.Media != nil && a.Media.Seconds > 0 {
			text += fmt.Sprintf(" (%d:%02d)", a.Media.Seconds/60, a.Media.Seconds%60)
		}
	case KindDocument:
		name := "Document"
		if a.Media != nil && a.Media.FileName != "" {
			name = a.Media.FileName
		}
		text = joinWith(" · ", "📄 "+name, a.Text)
	case KindSticker:
		text = "💟 Sticker"
	case KindLocation:
		first, _, _ := strings.Cut(a.Text, "\n")
		if strings.HasPrefix(first, "https://") {
			first = "Location"
		}
		text = "📍 " + first
	case KindContact:
		first, _, _ := strings.Cut(a.Text, "\n")
		name, _, _ := strings.Cut(first, ": ")
		text = "👤 " + name
	case KindPoll:
		text = "📊 " + a.Text
	default:
		text = a.Text
	}
	return oneLine(text, previewMaxRunes)
}

// quoteText is the one-line snippet shown for a quoted message: its text,
// with media as a "[image]"-style tag before the caption.
func (a analysis) quoteText() string {
	var tag string
	switch a.Kind {
	case KindImage:
		tag = "[image]"
	case KindVideo:
		tag = "[video]"
	case KindGIF:
		tag = "[GIF]"
	case KindAudio:
		tag = "[audio]"
	case KindVoice:
		tag = "[voice message]"
	case KindDocument:
		tag = "[document]"
		if a.Media != nil && a.Media.FileName != "" {
			tag = "[" + a.Media.FileName + "]"
		}
	case KindSticker:
		tag = "[sticker]"
	case KindLocation:
		tag = "[location]"
	case KindContact:
		tag = "[contact]"
	case KindPoll:
		tag = "[poll]"
	}
	text := a.Text
	if a.Kind == KindLocation {
		// Keep the place name, not the map link.
		first, _, _ := strings.Cut(text, "\n")
		if strings.HasPrefix(first, "https://") {
			first = ""
		}
		text = first
	}
	return oneLine(joinWith(" ", tag, text), 120)
}

// storedFields returns what the message store keeps beside the raw
// content: the media type and the searchable text.
func (a analysis) storedFields() (mediaType, text string) {
	return a.MediaType, a.Text
}

// classifyMessage identifies a message's type (empty string for plain
// text) and extracts its text content — the conversation/extended-text
// body for text messages, or the caption (if any) for media messages.
func classifyMessage(msg *waProto.Message) (mediaType, text string) {
	a := analyze(unwrapMessage(msg))
	if a.Class != classRow {
		return "", ""
	}
	if a.Kind == KindUnsupported {
		return "", ""
	}
	return a.storedFields()
}
