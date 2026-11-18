package whatsapp

import (
	"context"
	"time"

	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"

	"github.com/codebyoketch/wa-cli/internal/chatstore"
	"github.com/codebyoketch/wa-cli/internal/msgstore"
)

// handleEvent is the whatsmeow event handler: it keeps the local stores
// current and tells the TUI what changed. It runs on whatsmeow's
// goroutines, so anything slow (network calls, notifications) is pushed
// to the background.
func (c *Client) handleEvent(evt interface{}) {
	switch v := evt.(type) {
	case *events.Message:
		c.ingestMessage(v)
	case *events.HistorySync:
		c.ingestHistorySync(v)
	case *events.UndecryptableMessage:
		c.ingestUnavailable(v)
	case *events.Receipt:
		c.handleReceipt(v)

	case *events.Presence:
		c.emit(PresenceEvent{
			JID:      c.canonicalChat(v.From).String(),
			Online:   !v.Unavailable,
			LastSeen: unixMilliOrZero(v.LastSeen),
		})
	case *events.ChatPresence:
		c.handleChatPresence(v)

	case *events.Connected:
		c.emit(ConnEvent{State: ConnConnected})
		go c.afterConnect()
	case *events.Disconnected:
		c.emit(ConnEvent{State: ConnDisconnected})
	case *events.KeepAliveTimeout:
		c.emit(ConnEvent{State: ConnDisconnected, Detail: "the connection is not responding"})
	case *events.KeepAliveRestored:
		c.emit(ConnEvent{State: ConnConnected})
	case *events.LoggedOut:
		c.emit(ConnEvent{State: ConnLoggedOut, Detail: v.PermanentDisconnectDescription()})
	case *events.StreamReplaced:
		c.emit(ConnEvent{State: ConnReplaced, Detail: "another session with this login took over"})
	case *events.ConnectFailure:
		c.emit(ConnEvent{State: ConnDisconnected, Detail: v.PermanentDisconnectDescription()})
	case *events.TemporaryBan:
		c.emit(ConnEvent{State: ConnDisconnected, Detail: v.String()})
	case *events.ClientOutdated:
		c.emit(ConnEvent{State: ConnDisconnected, Detail: "WhatsApp rejected this client as outdated: update wa-cli"})

	case *events.PushName, *events.BusinessName, *events.Contact:
		c.invalidateNames()
	case *events.AppStateSyncComplete:
		c.invalidateNames()
		// A full app-state sync fills whatsmeow's chat settings and
		// contact tables without emitting per-chat events.
		go func() {
			changed := c.seedChatSettings(context.Background())
			if c.mergeLIDChats() > 0 || changed {
				c.emit(ChatEvent{})
			}
		}()

	case *events.Pin:
		c.updateChat(v.JID, func(jid string, chat chatstore.Chat) (bool, error) {
			on := v.Action.GetPinned()
			return chat.Pinned != on, c.chats.SetPinned(jid, on)
		})
	case *events.Archive:
		c.updateChat(v.JID, func(jid string, chat chatstore.Chat) (bool, error) {
			on := v.Action.GetArchived()
			return chat.Archived != on, c.chats.SetArchived(jid, on)
		})
	case *events.Mute:
		c.updateChat(v.JID, func(jid string, chat chatstore.Chat) (bool, error) {
			var until int64
			if v.Action.GetMuted() {
				until = v.Action.GetMuteEndTimestamp()
				if until <= 0 {
					until = -1
				}
			}
			return chat.MutedUntil != until, c.chats.SetMutedUntil(jid, until)
		})
	case *events.MarkChatAsRead:
		c.updateChat(v.JID, func(jid string, chat chatstore.Chat) (bool, error) {
			if v.Action.GetRead() {
				return chat.UnreadCount != 0 || chat.MarkedUnread, c.chats.MarkRead(jid)
			}
			return !chat.MarkedUnread, c.chats.SetMarkedUnread(jid, true)
		})
	case *events.DeleteChat:
		c.handleDeleteChat(v.JID, true)
	case *events.ClearChat:
		c.handleDeleteChat(v.JID, false)
	case *events.DeleteForMe:
		if c.msgs != nil {
			jid := c.canonicalChat(v.ChatJID).String()
			if err := c.msgs.Delete(jid, v.MessageID); err != nil {
				c.log.Warnf("deleting message %s failed: %v", v.MessageID, err)
				return
			}
			c.emit(MessageEvent{ChatJID: jid, ID: v.MessageID})
			c.refreshPreview(jid)
		}

	case *events.GroupInfo:
		c.handleGroupInfo(v)
	case *events.JoinedGroup:
		c.rememberGroup(&v.GroupInfo)
		c.emit(ChatEvent{JID: v.JID.String()})
	}
}

func unixMilliOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

// updateChat applies a change announced by another device to a stored
// chat and emits a ChatEvent when something really changed. apply
// returns whether the stored value differed.
func (c *Client) updateChat(jid types.JID, apply func(jid string, chat chatstore.Chat) (changed bool, err error)) {
	if c.chats == nil {
		return
	}
	key := c.canonicalChat(jid).String()
	chat, ok, err := c.chats.Get(key)
	if err != nil || !ok {
		return
	}
	changed, err := apply(key, chat)
	if err != nil {
		c.log.Warnf("updating chat %s failed: %v", key, err)
		return
	}
	if changed {
		c.emit(ChatEvent{JID: key})
	}
}

// handleDeleteChat mirrors a chat being deleted (or only emptied) on
// another device.
func (c *Client) handleDeleteChat(jid types.JID, removeChat bool) {
	key := c.canonicalChat(jid).String()
	if c.msgs != nil {
		if err := c.msgs.DeleteChat(key); err != nil {
			c.log.Warnf("deleting messages of %s failed: %v", key, err)
		}
	}
	if c.chats != nil {
		var err error
		if removeChat {
			err = c.chats.Delete(key)
		} else if chat, ok, getErr := c.chats.Get(key); getErr == nil && ok {
			err = c.chats.SetLastMessage(key, chat.LastMessageAt, "")
			if err == nil {
				err = c.chats.MarkRead(key)
			}
		}
		if err != nil {
			c.log.Warnf("updating deleted chat %s failed: %v", key, err)
		}
	}
	// The chat is gone or empty: have the list reloaded.
	c.emit(ChatEvent{})
}

// receiptStatus maps a receipt for a message you sent to the delivery
// status it proves. In groups a "read" from one member doesn't mean
// everyone read it, so it only counts as delivered there.
func receiptStatus(typ types.ReceiptType, group bool) (msgstore.Status, bool) {
	switch typ {
	case types.ReceiptTypeDelivered:
		return msgstore.StatusDelivered, true
	case types.ReceiptTypeRead:
		if group {
			return msgstore.StatusDelivered, true
		}
		return msgstore.StatusRead, true
	case types.ReceiptTypePlayed:
		if group {
			return msgstore.StatusDelivered, true
		}
		return msgstore.StatusPlayed, true
	}
	return msgstore.StatusNone, false
}

func (c *Client) handleReceipt(evt *events.Receipt) {
	if isPseudoChat(evt.Chat.String()) {
		return
	}
	chat := c.canonicalChat(evt.Chat).String()

	// One of your own devices read the chat.
	if evt.Type == types.ReceiptTypeReadSelf || (evt.IsFromMe && evt.Type == types.ReceiptTypeRead) {
		c.markReadLocally(chat)
		return
	}
	if evt.IsFromMe || c.msgs == nil {
		return
	}
	status, ok := receiptStatus(evt.Type, evt.IsGroup || isGroupJID(chat))
	if !ok {
		return
	}
	changed, err := c.msgs.RaiseStatus(chat, evt.MessageIDs, status)
	if err != nil {
		c.log.Warnf("recording receipt in %s failed: %v", chat, err)
		return
	}
	for _, id := range changed {
		c.emit(MessageEvent{ChatJID: chat, ID: id})
	}
}

// markReadLocally clears a chat's unread state in the store and reports
// whether there was anything to clear.
func (c *Client) markReadLocally(chatJID string) bool {
	if c.chats == nil {
		return false
	}
	chat, ok, err := c.chats.Get(chatJID)
	if err != nil || !ok || (chat.UnreadCount == 0 && !chat.MarkedUnread) {
		return false
	}
	if err := c.chats.MarkRead(chatJID); err != nil {
		c.log.Warnf("marking %s read failed: %v", chatJID, err)
		return false
	}
	c.emit(ChatEvent{JID: chatJID})
	return true
}

func (c *Client) handleChatPresence(evt *events.ChatPresence) {
	if evt.IsFromMe {
		return
	}
	state := TypingNone
	if evt.State == types.ChatPresenceComposing {
		state = TypingText
		if evt.Media == types.ChatPresenceMediaAudio {
			state = TypingAudio
		}
	}
	sender := evt.Sender.ToNonAD().String()
	c.emit(TypingEvent{
		ChatJID:    c.canonicalChat(evt.Chat).String(),
		SenderJID:  sender,
		SenderName: c.ResolveName(context.Background(), sender),
		State:      state,
	})
}

// afterConnect runs once a connection is authenticated: it announces
// presence if the user allows it and loads the group list for names.
func (c *Client) afterConnect() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	c.evMu.RLock()
	announce := c.behavior.SendPresence && c.wantOnline
	c.evMu.RUnlock()
	if announce {
		_ = c.SetOnline(ctx, true)
	}
	c.loadGroups(ctx, false)
	// Group lists carry many LID -> phone-number mappings.
	if c.mergeLIDChats() > 0 {
		c.emit(ChatEvent{})
	}
}

// seedChatSettings copies the phone's pin/archive/mute state, which
// whatsmeow keeps from app-state syncs, onto the stored chats. Local
// only. Reports whether any chat changed.
func (c *Client) seedChatSettings(ctx context.Context) (changed bool) {
	if c.chats == nil || c.WA == nil || c.WA.Store == nil || c.WA.Store.ChatSettings == nil {
		return false
	}
	settings := c.WA.Store.ChatSettings
	list, err := c.chats.List()
	if err != nil {
		return false
	}
	now := time.Now()
	for _, chat := range list {
		if ctx.Err() != nil {
			return changed
		}
		jid, err := types.ParseJID(chat.JID)
		if err != nil {
			continue
		}
		// The phone may index the chat under either of the person's
		// addresses.
		found := false
		var pinned, archived bool
		var until int64
		for _, key := range []types.JID{jid, c.lidForPN(jid)} {
			if key.IsEmpty() {
				continue
			}
			s, err := settings.GetChatSettings(ctx, key)
			if err != nil || !s.Found {
				continue
			}
			found = true
			pinned = pinned || s.Pinned
			archived = archived || s.Archived
			switch {
			case s.MutedUntil.Equal(store.MutedForever):
				until = -1
			case s.MutedUntil.After(now) && until != -1:
				until = max(until, s.MutedUntil.UnixMilli())
			}
		}
		if !found {
			continue
		}
		if chat.Pinned != pinned {
			changed = c.chats.SetPinned(chat.JID, pinned) == nil || changed
		}
		if chat.Archived != archived {
			changed = c.chats.SetArchived(chat.JID, archived) == nil || changed
		}
		if chat.MutedUntil != until && !(until == 0 && chat.MutedUntil > 0 && chat.MutedUntil < now.UnixMilli()) {
			changed = c.chats.SetMutedUntil(chat.JID, until) == nil || changed
		}
	}
	return changed
}

// appStateJID picks the address the phone indexes a 1:1 chat's settings
// under: whichever form whatsmeow has already seen settings for, else
// the @lid form on accounts migrated to LIDs, else the phone number.
func (c *Client) appStateJID(ctx context.Context, chat types.JID) types.JID {
	if chat.Server != types.DefaultUserServer || c.WA == nil || c.WA.Store == nil {
		return chat
	}
	lid := c.lidForPN(chat)
	if lid.IsEmpty() {
		return chat
	}
	if settings := c.WA.Store.ChatSettings; settings != nil {
		if s, err := settings.GetChatSettings(ctx, chat); err == nil && s.Found {
			return chat
		}
		if s, err := settings.GetChatSettings(ctx, lid); err == nil && s.Found {
			return lid
		}
	}
	if c.WA.Store.LIDMigrationTimestamp > 0 {
		return lid
	}
	return chat
}
