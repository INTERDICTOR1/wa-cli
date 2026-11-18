package whatsapp

import (
	"context"
	"errors"
	"strings"
	"time"

	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/socket"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"

	"github.com/codebyoketch/wa-cli/internal/chatstore"
	waerrors "github.com/codebyoketch/wa-cli/internal/errors"
	"github.com/codebyoketch/wa-cli/internal/msgstore"
)

// Every outgoing message follows the same path: its ID is generated up
// front, the row is stored as pending and announced, then it is sent
// under that ID and the row's status records the outcome. Because the ID
// never changes, repeating a send (a retry across a reconnect, or the
// user's Retry) can't deliver the message twice.

// connectWait is how long a send waits for the connection to come up
// before giving up (commands send right after Connect, which only starts
// the handshake).
const connectWait = 10 * time.Second

// sendBackoffs are the pauses before each attempt of a send.
var sendBackoffs = []time.Duration{0, 300 * time.Millisecond, 800 * time.Millisecond, 2 * time.Second}

// isRetryableSendError reports whether err means the request was lost to
// the connection not being up, as opposed to WhatsApp refusing it.
func isRetryableSendError(err error) bool {
	var disconnected *whatsmeow.DisconnectedError
	return errors.Is(err, whatsmeow.ErrNotConnected) ||
		errors.Is(err, socket.ErrSocketClosed) ||
		errors.As(err, &disconnected)
}

// retry runs fn up to len(backoffs) times, pausing backoffs[i] before
// attempt i. It repeats only while fn fails with an error retryable
// accepts and ready (called before each attempt, if non-nil) reports the
// connection is there; any other outcome is final.
func retry(ctx context.Context, backoffs []time.Duration, retryable func(error) bool, ready func() bool, fn func() error) error {
	var err error
	for _, wait := range backoffs {
		if wait > 0 {
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		up := ready == nil || ready()
		if err = fn(); err == nil || !retryable(err) || !up {
			return err
		}
	}
	return err
}

// withRetry runs a request that may be issued right after connecting or
// across a reconnect. fn must be safe to repeat.
func (c *Client) withRetry(ctx context.Context, fn func() error) error {
	return retry(ctx, sendBackoffs, isRetryableSendError, func() bool {
		return c.awaitConnection(ctx, connectWait)
	}, fn)
}

// awaitConnection waits up to wait for an authenticated connection and
// reports whether there is one.
func (c *Client) awaitConnection(ctx context.Context, wait time.Duration) bool {
	if c.WA == nil {
		return false
	}
	if c.connected() {
		return true
	}
	done := make(chan bool, 1)
	go func() { done <- c.WA.WaitForConnection(wait) }()
	select {
	case ok := <-done:
		return ok
	case <-ctx.Done():
		return false
	}
}

// sendTarget parses the chat a message goes to and returns it in
// canonical form, which is both the store key and the address to send to.
func (c *Client) sendTarget(chatJID string) (types.JID, error) {
	if !strings.Contains(chatJID, "@") {
		return types.EmptyJID, waerrors.New("not a chat: " + chatJID)
	}
	jid, err := types.ParseJID(chatJID)
	if err != nil || jid.User == "" {
		return types.EmptyJID, waerrors.New("not a chat: " + chatJID)
	}
	if isPseudoChat(jid.String()) {
		return types.EmptyJID, waerrors.New("can't send to " + chatJID)
	}
	return c.canonicalChat(jid), nil
}

func (c *Client) newMessageID() string {
	return c.WA.GenerateMessageID()
}

// chatTimer returns a chat's disappearing-messages timer in seconds.
func (c *Client) chatTimer(chatJID string) int64 {
	if c.chats == nil {
		return 0
	}
	chat, ok, err := c.chats.Get(chatJID)
	if err != nil || !ok {
		return 0
	}
	return chat.Ephemeral
}

// quoteParticipant is the sender to name when quoting m: its real sender
// JID, which for your own messages (stored as "me") is your own.
func (c *Client) quoteParticipant(m msgstore.Message) string {
	if m.FromMe || m.SenderJID == senderMe {
		// Groups addressed by LID know you by your LID.
		if isGroupJID(m.ChatJID) && c.groupUsesLID(m.ChatJID) {
			if lid := c.ownLID(); !lid.IsEmpty() {
				return lid.String()
			}
		}
		return c.ownJID().String()
	}
	if jid, err := types.ParseJID(m.SenderJID); err == nil && jid.User != "" {
		return jid.ToNonAD().String()
	}
	return m.ChatJID
}

// groupUsesLID reports whether a group's members are addressed by LID,
// as far as the cached member list shows.
func (c *Client) groupUsesLID(groupJID string) bool {
	members, _ := c.cachedMembers(groupJID)
	for _, m := range members {
		if isLIDJID(m.JID) {
			return true
		}
	}
	return false
}

// quotedContent returns the content to embed when quoting m, without its
// own quote (quotes don't nest) and without its secret.
func quotedContent(m msgstore.Message) *waProto.Message {
	msg := contentOf(m)
	if msg == nil {
		return nil
	}
	msg.MessageContextInfo = nil
	if ci := analyze(msg).Ctx; ci != nil {
		ci.QuotedMessage = nil
	}
	return msg
}

// outgoingContext builds the context info of a message sent to chat:
// who it mentions, what it replies to, and the chat's disappearing
// timer. Nil when it needs none of these.
func (c *Client) outgoingContext(chatJID string, replyTo *msgstore.Message, mentions []string) *waProto.ContextInfo {
	ci := &waProto.ContextInfo{}
	used := false
	if len(mentions) > 0 {
		ci.MentionedJID = mentions
		used = true
	}
	if replyTo != nil && replyTo.ID != "" {
		ci.StanzaID = proto.String(replyTo.ID)
		ci.Participant = proto.String(c.quoteParticipant(*replyTo))
		// Without the original content the reply is still sent, just
		// without the visible quote.
		if quoted := quotedContent(*replyTo); quoted != nil {
			ci.QuotedMessage = quoted
		}
		used = true
	}
	if timer := c.chatTimer(chatJID); timer > 0 {
		ci.Expiration = proto.Uint32(uint32(timer))
		used = true
	}
	if !used {
		return nil
	}
	return ci
}

// textMessage builds a text message: a plain conversation when it has no
// context, otherwise an extended text message carrying it.
func textMessage(text string, ci *waProto.ContextInfo) *waProto.Message {
	if ci == nil {
		return &waProto.Message{Conversation: proto.String(text)}
	}
	return &waProto.Message{ExtendedTextMessage: &waProto.ExtendedTextMessage{
		Text:        proto.String(text),
		ContextInfo: ci,
	}}
}

// TextMessage builds a text message. When it @mentions anyone it's an
// ExtendedTextMessage listing them in ContextInfo.MentionedJID; each must
// appear in text as "@" followed by its JID's user part.
func TextMessage(text string, mentioned []string) *waProto.Message {
	if len(mentioned) == 0 {
		return textMessage(text, nil)
	}
	return textMessage(text, &waProto.ContextInfo{MentionedJID: mentioned})
}

// outgoingRow builds the pending store row of a message about to be sent.
func outgoingRow(chatJID, id string, msg *waProto.Message) msgstore.Message {
	mediaType, text := analyze(unwrapMessage(msg)).storedFields()
	return msgstore.Message{
		ID:        id,
		ChatJID:   chatJID,
		SenderJID: senderMe,
		Timestamp: time.Now().UnixMilli(),
		Text:      text,
		FromMe:    true,
		MediaType: mediaType,
		RawProto:  encodeRawProto(msg),
		Status:    msgstore.StatusPending,
	}
}

// recordOutgoing stores a message that is about to be sent and announces
// it, so it shows up (as pending) before the network round trip. Sending
// to a chat also means you have read it.
func (c *Client) recordOutgoing(row msgstore.Message) {
	if c.msgs != nil {
		if _, err := c.msgs.Insert(row); err != nil {
			c.log.Warnf("storing outgoing message %s failed: %v", row.ID, err)
		}
	}
	c.emit(MessageEvent{ChatJID: row.ChatJID, ID: row.ID, New: true})

	if c.chats == nil {
		return
	}
	err := c.chats.Upsert(chatstore.Chat{
		JID:                row.ChatJID,
		IsGroup:            isGroupJID(row.ChatJID),
		LastMessageAt:      row.Timestamp,
		LastMessagePreview: c.previewFor(context.Background(), row, analyze(contentOf(row))),
	})
	if err != nil {
		c.log.Warnf("chatstore upsert failed for %s: %v", row.ChatJID, err)
	}
	if err := c.chats.MarkRead(row.ChatJID); err != nil {
		c.log.Warnf("marking %s read failed: %v", row.ChatJID, err)
	}
	c.emit(ChatEvent{JID: row.ChatJID})
}

// transmit sends msg under id and records the outcome on its stored row.
func (c *Client) transmit(ctx context.Context, chat types.JID, id string, msg *waProto.Message) error {
	err := c.withRetry(ctx, func() error {
		_, err := c.WA.SendMessage(ctx, chat, msg, whatsmeow.SendRequestExtra{ID: id})
		return err
	})
	c.finishSend(chat.String(), id, err)
	return err
}

// finishSend marks a pending row sent or failed and announces the change.
func (c *Client) finishSend(chatJID, id string, sendErr error) {
	if c.msgs != nil {
		var err error
		if sendErr != nil {
			err = c.msgs.SetStatus(chatJID, []string{id}, msgstore.StatusFailed)
		} else {
			// Raise rather than set: a delivery receipt may already have
			// moved it further along.
			_, err = c.msgs.RaiseStatus(chatJID, []string{id}, msgstore.StatusSent)
		}
		if err != nil {
			c.log.Warnf("recording send status of %s failed: %v", id, err)
		}
	}
	if sendErr == nil {
		c.sendMu.Lock()
		delete(c.pendingFiles, id)
		c.sendMu.Unlock()
	}
	c.emit(MessageEvent{ChatJID: chatJID, ID: id})
}

// deliver records msg as pending in chat, sends it and returns its ID.
func (c *Client) deliver(ctx context.Context, chat types.JID, msg *waProto.Message) (string, error) {
	id := c.newMessageID()
	c.recordOutgoing(outgoingRow(chat.String(), id, msg))
	return id, c.transmit(ctx, chat, id, msg)
}

// Send sends a text or file message. The message is stored as pending
// and announced with a MessageEvent before the network round trip; its
// final status (sent or failed) arrives as another MessageEvent, and the
// send error, if any, is returned.
func (c *Client) Send(ctx context.Context, chatJID string, out Outgoing) error {
	chat, err := c.sendTarget(chatJID)
	if err != nil {
		return err
	}
	if out.FilePath != "" {
		return c.sendFile(ctx, chat, c.newMessageID(), out)
	}
	if strings.TrimSpace(out.Text) == "" {
		return waerrors.New("nothing to send")
	}
	msg := textMessage(out.Text, c.outgoingContext(chat.String(), out.ReplyTo, out.Mentions))
	if _, err := c.deliver(ctx, chat, msg); err != nil {
		return waerrors.Wrap(err, "sending message")
	}
	return nil
}

// Retry re-sends a message whose status is StatusFailed, under the same
// ID. A file whose upload never finished is uploaded again if it was
// attached during this run.
func (c *Client) Retry(ctx context.Context, m msgstore.Message) error {
	if c.msgs == nil {
		return errNoStore
	}
	cur, ok, err := c.msgs.Get(m.ChatJID, m.ID)
	if err != nil {
		return err
	}
	if !ok {
		return waerrors.New("that message is no longer stored")
	}
	if !cur.FromMe || cur.Status != msgstore.StatusFailed {
		return waerrors.New("only a message that failed to send can be retried")
	}
	chat, err := c.sendTarget(cur.ChatJID)
	if err != nil {
		return err
	}

	c.sendMu.Lock()
	file, isFile := c.pendingFiles[cur.ID]
	c.sendMu.Unlock()
	msg := decodeRawProto(cur.RawProto)
	uploaded := msg != nil && (analyze(unwrapMessage(msg)).Download == nil || hasMediaRef(unwrapMessage(msg)))
	if !uploaded && !isFile {
		return waerrors.New("the file was never uploaded: attach it again")
	}

	if err := c.msgs.SetStatus(cur.ChatJID, []string{cur.ID}, msgstore.StatusPending); err != nil {
		return err
	}
	c.emit(MessageEvent{ChatJID: cur.ChatJID, ID: cur.ID})
	if !uploaded {
		return c.uploadAndSend(ctx, chat, cur.ID, file)
	}
	if err := c.transmit(ctx, chat, cur.ID, msg); err != nil {
		return waerrors.Wrap(err, "sending message")
	}
	return nil
}

// senderOf returns the JID to name as the sender of stored message m in
// a message key: empty for your own messages (whatsmeow then marks the
// key as from you).
func senderOf(m msgstore.Message) types.JID {
	if m.FromMe || m.SenderJID == senderMe {
		return types.EmptyJID
	}
	jid, err := types.ParseJID(m.SenderJID)
	if err != nil {
		return types.EmptyJID
	}
	return jid.ToNonAD()
}

// sendControl sends a message that isn't a chat message of its own (a
// reaction, an edit, a delete).
func (c *Client) sendControl(ctx context.Context, chat types.JID, msg *waProto.Message) error {
	id := c.newMessageID()
	return c.withRetry(ctx, func() error {
		_, err := c.WA.SendMessage(ctx, chat, msg, whatsmeow.SendRequestExtra{ID: id})
		return err
	})
}

// React sets your reaction on msg; emoji "" removes it.
func (c *Client) React(ctx context.Context, m msgstore.Message, emoji string) error {
	chat, err := c.sendTarget(m.ChatJID)
	if err != nil {
		return err
	}
	if m.Deleted {
		return waerrors.New("can't react to a deleted message")
	}
	if err := c.sendControl(ctx, chat, c.WA.BuildReaction(chat, senderOf(m), m.ID, emoji)); err != nil {
		return waerrors.Wrap(err, "sending reaction")
	}
	if c.msgs != nil {
		reaction := msgstore.Reaction{SenderJID: senderMe, Emoji: emoji, Timestamp: time.Now().UnixMilli()}
		if _, err := c.msgs.SetReaction(m.ChatJID, m.ID, reaction); err != nil {
			return err
		}
	}
	c.emit(MessageEvent{ChatJID: m.ChatJID, ID: m.ID})
	return nil
}

// editedContent returns what an edit of old to text looks like: wire is
// the content to send in the edit, stored is the message's full content
// afterwards. Both are nil when old can't be edited (only text and
// captions can).
func editedContent(old *waProto.Message, text string) (wire, stored *waProto.Message) {
	switch {
	case old.GetConversation() != "":
		msg := &waProto.Message{Conversation: proto.String(text)}
		return msg, msg
	case old.GetExtendedTextMessage() != nil:
		ext := proto.Clone(old.GetExtendedTextMessage()).(*waProto.ExtendedTextMessage)
		ext.Text = proto.String(text)
		if ext.ContextInfo != nil {
			// The mentions pointed into the old text.
			ext.ContextInfo.MentionedJID = nil
		}
		msg := &waProto.Message{ExtendedTextMessage: ext}
		return msg, msg
	}
	if caption, _ := captionOf(old); caption != nil {
		// A caption edit carries just the caption: the media can't change.
		stored = proto.Clone(old).(*waProto.Message)
		storedCaption, _ := captionOf(stored)
		*storedCaption = proto.String(text)
		switch {
		case old.GetImageMessage() != nil:
			wire = &waProto.Message{ImageMessage: &waProto.ImageMessage{Caption: proto.String(text)}}
		case old.GetVideoMessage() != nil:
			wire = &waProto.Message{VideoMessage: &waProto.VideoMessage{Caption: proto.String(text)}}
		default:
			wire = &waProto.Message{DocumentMessage: &waProto.DocumentMessage{Caption: proto.String(text)}}
		}
		return wire, stored
	}
	return nil, nil
}

// Edit replaces the text (or caption) of a message you sent. WhatsApp
// accepts edits for a limited time after sending.
func (c *Client) Edit(ctx context.Context, m msgstore.Message, text string) error {
	if !m.FromMe {
		return waerrors.New("only your own messages can be edited")
	}
	if strings.TrimSpace(text) == "" {
		return waerrors.New("the new text is empty")
	}
	if time.Since(time.UnixMilli(m.Timestamp)) > whatsmeow.EditWindow {
		return waerrors.New("this message is too old to edit")
	}
	chat, err := c.sendTarget(m.ChatJID)
	if err != nil {
		return err
	}
	if c.msgs != nil {
		// Edit the content as stored now, not a possibly stale copy.
		if cur, ok, err := c.msgs.Get(m.ChatJID, m.ID); err == nil && ok {
			m = cur
		}
	}
	old := contentOf(m)
	if old == nil || m.Deleted {
		return waerrors.New("this message can't be edited")
	}
	wire, stored := editedContent(old, text)
	if wire == nil {
		return waerrors.New("only text messages and captions can be edited")
	}
	if err := c.sendControl(ctx, chat, c.WA.BuildEdit(chat, m.ID, wire)); err != nil {
		return waerrors.Wrap(err, "sending edit")
	}
	if c.msgs != nil {
		if _, err := c.msgs.ApplyEdit(m.ChatJID, m.ID, text, encodeRawProto(stored)); err != nil {
			return err
		}
	}
	c.emit(MessageEvent{ChatJID: m.ChatJID, ID: m.ID})
	c.refreshPreviewIfLatest(msgRef{m.ChatJID, m.ID})
	return nil
}

// Revoke deletes a message for everyone: one you sent, or, in a group
// you administer, someone else's.
func (c *Client) Revoke(ctx context.Context, m msgstore.Message) error {
	chat, err := c.sendTarget(m.ChatJID)
	if err != nil {
		return err
	}
	if !m.FromMe && !isGroupJID(m.ChatJID) {
		return waerrors.New("only your own messages can be deleted for everyone")
	}
	if m.FromMe && (m.Status == msgstore.StatusFailed || m.Status == msgstore.StatusPending) {
		// Never left this device: there is nothing to delete elsewhere.
		return c.DeleteLocal(m)
	}
	if err := c.sendControl(ctx, chat, c.WA.BuildRevoke(chat, senderOf(m), m.ID)); err != nil {
		return waerrors.Wrap(err, "deleting message")
	}
	if c.msgs != nil {
		if _, err := c.msgs.MarkDeleted(m.ChatJID, m.ID); err != nil {
			return err
		}
	}
	c.emit(MessageEvent{ChatJID: m.ChatJID, ID: m.ID})
	c.refreshPreviewIfLatest(msgRef{m.ChatJID, m.ID})
	return nil
}

// DeleteLocal removes a message from this device only.
func (c *Client) DeleteLocal(m msgstore.Message) error {
	if c.msgs == nil {
		return errNoStore
	}
	if err := c.msgs.Delete(m.ChatJID, m.ID); err != nil {
		return err
	}
	c.sendMu.Lock()
	delete(c.pendingFiles, m.ID)
	c.sendMu.Unlock()
	c.emit(MessageEvent{ChatJID: m.ChatJID, ID: m.ID})
	c.refreshPreview(m.ChatJID)
	return nil
}

// Forward sends msg's content to another chat, marked as forwarded. Media
// reuses the original upload rather than being uploaded again. The copy
// is stored in the target chat like any sent message.
func (c *Client) Forward(ctx context.Context, toChatJID string, m msgstore.Message) error {
	chat, err := c.sendTarget(toChatJID)
	if err != nil {
		return err
	}
	_, err = c.forward(ctx, chat, m)
	return err
}

func (c *Client) forward(ctx context.Context, chat types.JID, m msgstore.Message) (string, error) {
	original := contentOf(m)
	if original == nil || m.Deleted {
		return "", waerrors.New("original message content isn't available to forward (too old, or not captured)")
	}
	msg, err := markForwarded(original)
	if err != nil {
		return "", err
	}
	if timer := c.chatTimer(chat.String()); timer > 0 {
		analyze(msg).Ctx.Expiration = proto.Uint32(uint32(timer))
	}
	id, err := c.deliver(ctx, chat, msg)
	if err != nil {
		return "", waerrors.Wrap(err, "forwarding message")
	}
	return id, nil
}

// markForwarded returns a message equivalent to original but with
// IsForwarded/ForwardingScore set on its ContextInfo, and without the
// quote of a reply (a forward isn't a reply to anything in its new
// chat). Plain Conversation text is normalized into ExtendedTextMessage
// first, since Conversation (a bare string field) has nowhere to attach
// ContextInfo. Supports text, the media types, locations and contacts;
// anything else is rejected. original is modified.
func markForwarded(original *waProto.Message) (*waProto.Message, error) {
	bump := func(ci *waProto.ContextInfo) *waProto.ContextInfo {
		if ci == nil {
			ci = &waProto.ContextInfo{}
		}
		ci.IsForwarded = proto.Bool(true)
		ci.ForwardingScore = proto.Uint32(ci.GetForwardingScore() + 1)
		ci.StanzaID, ci.Participant, ci.QuotedMessage, ci.RemoteJID = nil, nil, nil, nil
		ci.Expiration, ci.EphemeralSettingTimestamp = nil, nil
		return ci
	}

	switch {
	case original.GetConversation() != "":
		return &waProto.Message{
			ExtendedTextMessage: &waProto.ExtendedTextMessage{
				Text:        proto.String(original.GetConversation()),
				ContextInfo: bump(nil),
			},
		}, nil
	case original.GetExtendedTextMessage() != nil:
		m := original.GetExtendedTextMessage()
		m.ContextInfo = bump(m.ContextInfo)
		return &waProto.Message{ExtendedTextMessage: m}, nil
	case original.GetImageMessage() != nil:
		m := original.GetImageMessage()
		if m.GetViewOnce() {
			break
		}
		m.ContextInfo = bump(m.ContextInfo)
		return &waProto.Message{ImageMessage: m}, nil
	case original.GetVideoMessage() != nil:
		m := original.GetVideoMessage()
		if m.GetViewOnce() {
			break
		}
		m.ContextInfo = bump(m.ContextInfo)
		return &waProto.Message{VideoMessage: m}, nil
	case original.GetAudioMessage() != nil:
		m := original.GetAudioMessage()
		if m.GetViewOnce() {
			break
		}
		m.ContextInfo = bump(m.ContextInfo)
		return &waProto.Message{AudioMessage: m}, nil
	case original.GetDocumentMessage() != nil:
		m := original.GetDocumentMessage()
		m.ContextInfo = bump(m.ContextInfo)
		return &waProto.Message{DocumentMessage: m}, nil
	case original.GetStickerMessage() != nil:
		m := original.GetStickerMessage()
		m.ContextInfo = bump(m.ContextInfo)
		return &waProto.Message{StickerMessage: m}, nil
	case original.GetLocationMessage() != nil:
		m := original.GetLocationMessage()
		m.ContextInfo = bump(m.ContextInfo)
		return &waProto.Message{LocationMessage: m}, nil
	case original.GetContactMessage() != nil:
		m := original.GetContactMessage()
		m.ContextInfo = bump(m.ContextInfo)
		return &waProto.Message{ContactMessage: m}, nil
	case original.GetContactsArrayMessage() != nil:
		m := original.GetContactsArrayMessage()
		m.ContextInfo = bump(m.ContextInfo)
		return &waProto.Message{ContactsArrayMessage: m}, nil
	}
	return nil, waerrors.New("forwarding this message type isn't supported yet")
}

// --- the CLI's sending calls ---

// SendText sends a plain text message to jid and returns the sent
// message's ID (for later reply/forward reference). Callers should call
// Connect() first.
func (c *Client) SendText(ctx context.Context, jid types.JID, text string) (string, error) {
	chat := c.canonicalChat(jid)
	return c.SendMessage(ctx, chat, textMessage(text, c.outgoingContext(chat.String(), nil, nil)))
}

// SendMessage sends msg to jid, recording it in the local stores, and
// returns the sent message's ID.
func (c *Client) SendMessage(ctx context.Context, jid types.JID, msg *waProto.Message) (string, error) {
	id, err := c.deliver(ctx, c.canonicalChat(jid), msg)
	if err != nil {
		return "", waerrors.Wrap(err, "sending message")
	}
	return id, nil
}

// SendReply sends text to jid as a quoted reply to quoted. If quoted's
// original content couldn't be reconstructed (e.g. an old or unsupported
// message type), the reply is still sent, just without the visible quote.
func (c *Client) SendReply(ctx context.Context, jid types.JID, text string, quoted msgstore.Message) (string, error) {
	chat := c.canonicalChat(jid)
	id, err := c.deliver(ctx, chat, textMessage(text, c.outgoingContext(chat.String(), &quoted, nil)))
	if err != nil {
		return "", waerrors.Wrap(err, "sending reply")
	}
	return id, nil
}

// ForwardMessage re-sends quoted's content to toJID, marked as forwarded,
// and returns the new message's ID. For media messages, this reuses the
// original upload (URL/MediaKey/hashes) rather than re-uploading.
func (c *Client) ForwardMessage(ctx context.Context, toJID types.JID, quoted msgstore.Message) (string, error) {
	return c.forward(ctx, c.canonicalChat(toJID), quoted)
}
