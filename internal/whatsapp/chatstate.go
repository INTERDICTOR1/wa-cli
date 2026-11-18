package whatsapp

import (
	"context"
	"errors"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/appstate"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"

	waerrors "github.com/codebyoketch/wa-cli/internal/errors"
	"github.com/codebyoketch/wa-cli/internal/msgstore"
)

// errOffline is returned by actions that only make sense with a
// connection (they change state kept on WhatsApp's side).
var errOffline = waerrors.New("not connected to WhatsApp")

// isOfflineError reports whether err is just the connection not being
// there.
func isOfflineError(err error) bool {
	return isRetryableSendError(err) || errors.Is(err, whatsmeow.ErrIQTimedOut)
}

// --- read state ---

// maxReadReceipts caps how many messages one MarkChatRead acknowledges.
const maxReadReceipts = 50

// MarkChatRead zeroes the unread count and tells WhatsApp the chat's
// recent incoming messages were read. With Behavior.SendReadReceipts off
// (or read receipts disabled in the account's privacy settings) only
// your own devices are told, so the sender sees no blue ticks. Offline it
// just clears the count.
func (c *Client) MarkChatRead(ctx context.Context, chatJID string) error {
	chatJID = c.canonicalChatString(chatJID)
	unread, marked := 0, false
	if c.chats != nil {
		if chat, ok, err := c.chats.Get(chatJID); err == nil && ok {
			unread, marked = chat.UnreadCount, chat.MarkedUnread
		}
		c.markReadLocally(chatJID)
	}
	if c.msgs == nil || !c.connected() {
		return nil
	}

	recent, err := c.msgs.LastIncoming(chatJID, min(max(unread, 1), maxReadReceipts))
	if err != nil {
		return err
	}
	if len(recent) > 0 {
		newest := recent[0].ID
		c.sendMu.Lock()
		done := c.lastRead[chatJID] == newest
		c.sendMu.Unlock()
		// Nothing new since the last receipt sent from here.
		if !done || unread > 0 {
			if err := c.sendReadReceipts(ctx, chatJID, recent); err != nil {
				if isOfflineError(err) {
					return nil
				}
				return waerrors.Wrap(err, "sending read receipt")
			}
			c.sendMu.Lock()
			if c.lastRead == nil {
				c.lastRead = map[string]string{}
			}
			c.lastRead[chatJID] = newest
			c.sendMu.Unlock()
		}
	}
	if marked {
		// The "unread" flag was set by hand: clear it on the phone too.
		if err := c.sendMarkRead(ctx, chatJID, true); err != nil && !isOfflineError(err) {
			c.log.Warnf("clearing the unread mark of %s failed: %v", chatJID, err)
		}
	}
	return nil
}

// sendReadReceipts acknowledges msgs (newest first) as read: one receipt
// per sender, addressed the way each message arrived.
func (c *Client) sendReadReceipts(ctx context.Context, chatJID string, msgs []msgstore.Message) error {
	receipt := types.ReceiptTypeRead
	if !c.getBehavior().SendReadReceipts {
		// Tell only your own devices. (With receipts allowed, whatsmeow
		// still downgrades to this when the account's privacy setting has
		// read receipts off.)
		receipt = types.ReceiptTypeReadSelf
	}
	chat, err := types.ParseJID(chatJID)
	if err != nil {
		return err
	}
	group := chat.Server == types.GroupServer

	var senders []types.JID
	ids := map[types.JID][]types.MessageID{}
	for _, m := range msgs {
		sender, err := types.ParseJID(m.SenderJID)
		if err != nil || sender.User == "" {
			if group {
				continue
			}
			sender = chat
		}
		sender = sender.ToNonAD()
		if _, ok := ids[sender]; !ok {
			senders = append(senders, sender)
		}
		ids[sender] = append(ids[sender], m.ID)
	}
	now := time.Now()
	for _, sender := range senders {
		to, participant := sender, types.EmptyJID
		if group {
			to, participant = chat, sender
		}
		if err := c.WA.MarkRead(ctx, ids[sender], now, to, participant, receipt); err != nil {
			return err
		}
	}
	return nil
}

// lastMessageKey returns the time and key of a chat's newest stored
// message, which WhatsApp wants along with whole-chat actions.
func (c *Client) lastMessageKey(chatJID string, target types.JID) (time.Time, *waCommon.MessageKey) {
	if c.msgs == nil {
		return time.Time{}, nil
	}
	latest, err := c.msgs.Latest(chatJID, 1)
	if err != nil || len(latest) == 0 {
		return time.Time{}, nil
	}
	m := latest[0]
	key := &waCommon.MessageKey{
		RemoteJID: proto.String(target.String()),
		FromMe:    proto.Bool(m.FromMe),
		ID:        proto.String(m.ID),
	}
	if target.Server == types.GroupServer && !m.FromMe {
		key.Participant = proto.String(m.SenderJID)
	}
	return time.UnixMilli(m.Timestamp), key
}

// sendMarkRead syncs the manual read/unread mark of a chat to your other
// devices.
func (c *Client) sendMarkRead(ctx context.Context, chatJID string, read bool) error {
	chat, err := types.ParseJID(chatJID)
	if err != nil {
		return err
	}
	target := c.appStateJID(ctx, chat)
	ts, key := c.lastMessageKey(chatJID, target)
	return c.WA.SendAppState(ctx, appstate.BuildMarkChatAsRead(target, read, ts, key))
}

// MarkChatUnread sets the manual "unread" flag, here and (when
// connected) on your other devices.
func (c *Client) MarkChatUnread(ctx context.Context, chatJID string) error {
	if c.chats == nil {
		return errNoStore
	}
	chatJID = c.canonicalChatString(chatJID)
	if err := c.chats.SetMarkedUnread(chatJID, true); err != nil {
		return err
	}
	c.emit(ChatEvent{JID: chatJID})
	if !c.connected() {
		return nil
	}
	if err := c.sendMarkRead(ctx, chatJID, false); err != nil && !isOfflineError(err) {
		return waerrors.Wrap(err, "syncing the unread mark")
	}
	return nil
}

// --- pin, archive, mute ---

// sendChatSetting sends a chat setting to WhatsApp so every device
// agrees on it, then applies it locally. These need a connection: the
// setting lives on WhatsApp's side.
func (c *Client) sendChatSetting(ctx context.Context, chatJID string, build func(target types.JID) appstate.PatchInfo, apply func(jid string) error) error {
	if c.chats == nil {
		return errNoStore
	}
	chatJID = c.canonicalChatString(chatJID)
	chat, err := types.ParseJID(chatJID)
	if err != nil {
		return err
	}
	if !c.awaitConnection(ctx, 3*time.Second) {
		return errOffline
	}
	if err := c.WA.SendAppState(ctx, build(c.appStateJID(ctx, chat))); err != nil {
		return waerrors.Wrap(err, "updating chat on WhatsApp")
	}
	if err := apply(chatJID); err != nil {
		return err
	}
	c.emit(ChatEvent{JID: chatJID})
	return nil
}

// SetPinned pins or unpins a chat on all your devices.
func (c *Client) SetPinned(ctx context.Context, chatJID string, on bool) error {
	return c.sendChatSetting(ctx, chatJID,
		func(target types.JID) appstate.PatchInfo { return appstate.BuildPin(target, on) },
		func(jid string) error { return c.chats.SetPinned(jid, on) })
}

// SetArchived archives or unarchives a chat on all your devices.
// Archiving also unpins it, as WhatsApp does.
func (c *Client) SetArchived(ctx context.Context, chatJID string, on bool) error {
	return c.sendChatSetting(ctx, chatJID,
		func(target types.JID) appstate.PatchInfo {
			ts, key := c.lastMessageKey(c.canonicalChatString(chatJID), target)
			return appstate.BuildArchive(target, on, ts, key)
		},
		func(jid string) error {
			if on {
				if err := c.chats.SetPinned(jid, false); err != nil {
					return err
				}
			}
			return c.chats.SetArchived(jid, on)
		})
}

// SetMuted mutes (until unmuted) or unmutes a chat on all your devices.
func (c *Client) SetMuted(ctx context.Context, chatJID string, on bool) error {
	return c.sendChatSetting(ctx, chatJID,
		func(target types.JID) appstate.PatchInfo { return appstate.BuildMute(target, on, 0) },
		func(jid string) error {
			var until int64
			if on {
				until = -1
			}
			return c.chats.SetMutedUntil(jid, until)
		})
}

// SetDraft remembers unsent text for a chat.
func (c *Client) SetDraft(chatJID, text string) error {
	if c.chats == nil {
		return errNoStore
	}
	chatJID = c.canonicalChatString(chatJID)
	if err := c.chats.SetDraft(chatJID, text); err != nil {
		return err
	}
	c.emit(ChatEvent{JID: chatJID})
	return nil
}

// --- older history ---

// olderBatch is how many messages one LoadOlder asks for (what WhatsApp's
// own clients request at a time).
const olderBatch = 50

// LoadOlder asks the phone for messages older than the oldest stored
// one. They arrive later as an on-demand history sync and are announced
// with MessageEvents. requested is false when there is nothing to anchor
// the request on. Needs the phone to be online.
func (c *Client) LoadOlder(ctx context.Context, chatJID string) (requested bool, err error) {
	if c.msgs == nil {
		return false, errNoStore
	}
	chatJID = c.canonicalChatString(chatJID)
	oldest, ok, err := c.msgs.Oldest(chatJID)
	if err != nil || !ok {
		return false, err
	}
	if !c.connected() {
		return false, errOffline
	}
	if err := c.RequestOlderMessages(ctx, oldest, olderBatch); err != nil {
		return false, waerrors.Wrap(err, "asking the phone for older messages")
	}
	return true, nil
}

// RequestOlderMessages asks your phone for up to count messages sent
// before oldest (the oldest one stored for its chat). The phone answers
// later with an on-demand history sync, which ingestHistorySync stores.
// Needs the phone to be online.
func (c *Client) RequestOlderMessages(ctx context.Context, oldest msgstore.Message, count int) error {
	chat, err := types.ParseJID(oldest.ChatJID)
	if err != nil {
		return err
	}
	// The phone keeps a 1:1 chat under one of the person's two addresses
	// and only answers for that one, so ask under both.
	targets := []types.JID{chat}
	if lid := c.lidForPN(chat); !lid.IsEmpty() {
		targets = []types.JID{lid, chat}
	}
	var firstErr error
	sent := false
	for _, target := range targets {
		req := c.WA.BuildHistorySyncRequest(&types.MessageInfo{
			MessageSource: types.MessageSource{Chat: target, IsFromMe: oldest.FromMe},
			ID:            oldest.ID,
			Timestamp:     time.UnixMilli(oldest.Timestamp),
		}, count)
		if _, err := c.WA.SendPeerMessage(ctx, req); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		sent = true
	}
	if !sent {
		return firstErr
	}
	return nil
}

// --- presence ---

// presenceAllowed reports whether the user lets this client show them
// online/typing (which is also what lets it see others').
func (c *Client) presenceAllowed() bool {
	return c.getBehavior().SendPresence && c.connected()
}

// quiet turns the errors presence calls shouldn't bother anyone with
// into nil: no connection, or no push name to announce yet.
func quiet(err error) error {
	if err == nil || isOfflineError(err) || errors.Is(err, whatsmeow.ErrNoPushName) {
		return nil
	}
	return err
}

// SetOnline shows you as online or offline. The choice is remembered and
// re-announced after a reconnect. A no-op unless Behavior.SendPresence
// is on and there is a connection.
func (c *Client) SetOnline(ctx context.Context, online bool) error {
	c.evMu.Lock()
	c.wantOnline = online
	c.evMu.Unlock()
	if !c.presenceAllowed() {
		return nil
	}
	state := types.PresenceUnavailable
	if online {
		state = types.PresenceAvailable
	}
	return quiet(c.WA.SendPresence(ctx, state))
}

// SetTyping shows or clears "typing…" in a chat. A no-op unless
// Behavior.SendPresence is on and there is a connection.
func (c *Client) SetTyping(ctx context.Context, chatJID string, typing bool) error {
	if !c.presenceAllowed() {
		return nil
	}
	chat, err := c.sendTarget(chatJID)
	if err != nil {
		return nil
	}
	state := types.ChatPresencePaused
	if typing {
		state = types.ChatPresenceComposing
	}
	return quiet(c.WA.SendChatPresence(ctx, chat, state, types.ChatPresenceMediaText))
}

// WatchPresence asks for online/last-seen updates for a 1:1 chat; they
// arrive as PresenceEvents. A no-op for groups, and unless
// Behavior.SendPresence is on and there is a connection (WhatsApp only
// shows others' presence to those who share their own).
func (c *Client) WatchPresence(ctx context.Context, chatJID string) error {
	if !c.presenceAllowed() {
		return nil
	}
	chat, err := c.sendTarget(chatJID)
	if err != nil || (chat.Server != types.DefaultUserServer && chat.Server != types.HiddenUserServer) {
		return nil
	}
	return quiet(c.WA.SubscribePresence(ctx, chat))
}
