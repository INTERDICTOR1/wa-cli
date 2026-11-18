package whatsapp

import (
	"context"
	"math"
	"sort"
	"time"

	"go.mau.fi/whatsmeow/proto/waCommon"
	waProto "go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"

	"github.com/codebyoketch/wa-cli/internal/chatstore"
	"github.com/codebyoketch/wa-cli/internal/msgstore"
)

// chatFor returns the canonical chat a message belongs to. WhatsApp sends
// the other address of a 1:1 chat along with the message, which settles
// it even before the mapping reaches the LID store.
func (c *Client) chatFor(src *types.MessageSource) types.JID {
	chat := src.Chat.ToNonAD()
	if src.IsIncomingBroadcast() && !src.IsFromMe {
		// Sent to a broadcast list you're on: it belongs in the sender's chat.
		chat = src.Sender.ToNonAD()
	}
	if chat.Server == types.HiddenUserServer && !src.IsGroup {
		if src.IsFromMe && src.RecipientAlt.Server == types.DefaultUserServer {
			return src.RecipientAlt.ToNonAD()
		}
		if !src.IsFromMe && src.SenderAlt.Server == types.DefaultUserServer {
			return src.SenderAlt.ToNonAD()
		}
	}
	return c.canonicalChat(chat)
}

// incomingFromEvent turns a live message event into the form the fold
// code works on.
func (c *Client) incomingFromEvent(evt *events.Message, chat types.JID) incoming {
	msg := unwrapMessage(evt.Message)
	if evt.IsViewOnce && msg != nil {
		markViewOnce(msg)
	}
	in := incoming{
		ChatJID:   chat.String(),
		ID:        evt.Info.ID,
		SenderJID: evt.Info.Sender.ToNonAD().String(),
		FromMe:    evt.Info.IsFromMe,
		Timestamp: evt.Info.Timestamp.UnixMilli(),
		Msg:       msg,
		evt:       evt,
		rawChat:   evt.Info.Chat,
	}
	if evt.Info.Timestamp.IsZero() {
		in.Timestamp = time.Now().UnixMilli()
	}
	if in.FromMe {
		in.SenderJID = senderMe
		in.Status = msgstore.StatusSent
	}
	return in
}

// ingestMessage keeps the stores current as messages arrive (or are sent
// from another of your devices): a real message becomes a row, anything
// that is about another message is folded into it. Only a newly stored
// row counts as unread, moves the chat up the list or notifies.
func (c *Client) ingestMessage(evt *events.Message) {
	if evt == nil || isPseudoChat(evt.Info.Chat.String()) {
		return
	}
	chat := c.chatFor(&evt.Info.MessageSource)
	if isPseudoChat(chat.String()) {
		return
	}
	c.noteLIDChat(evt.Info.Chat, chat)
	in := c.incomingFromEvent(evt, chat)

	// A bot streams its answer as a series of messages that each replace
	// the previous one.
	replaces := ""
	if bot := evt.Info.MsgBotInfo; bot.EditType == types.EditTypeInner || bot.EditType == types.EditTypeLast {
		replaces = bot.EditTargetID
	}

	c.foldMu.Lock()
	res := c.ingest(context.Background(), in, replaces)
	c.foldMu.Unlock()

	c.announce(in, res, evt.Info.PushName)
}

// ingest stores or folds one message. In the result Store is set only
// when a row was really inserted (not for a duplicate).
func (c *Client) ingest(ctx context.Context, in incoming, replaces string) applied {
	var res applied
	retargeted := false
	switch classOf(in) {
	case classDrop:
		return res
	case classEvent:
		res = c.applyEvent(ctx, in)
	default:
		a := analyze(in.Msg)
		row := in.row(a)
		if replaces != "" && replaces != in.ID {
			if c.replaceContent(in, replaces, a, &res) {
				return res
			}
			row.ID, retargeted = replaces, true
		}
		res.Store = &row
	}
	if res.Store != nil && !c.storeRow(res.Store, &res) {
		// Already stored. A message that names the ID it wants to replace
		// was refused above; the placeholder rule is not a second way in.
		if !retargeted {
			c.replacePlaceholder(*res.Store, &res)
		}
		res.Store = nil
	}
	return res
}

// replacePlaceholder fills in a row stored as a "can only be seen on
// your phone" placeholder when the real message turns up after all. Only
// the placeholder's own sender can fill it: a message ID is chosen by
// whoever sends it, so anyone in the chat can reuse one, and their words
// would otherwise be shown under the first sender's name.
func (c *Client) replacePlaceholder(row msgstore.Message, res *applied) {
	if c.msgs == nil || contentOf(row).GetPlaceholderMessage() != nil {
		return
	}
	cur, ok, err := c.msgs.Get(row.ChatJID, row.ID)
	if err != nil || !ok || cur.Deleted || contentOf(cur).GetPlaceholderMessage() == nil {
		return
	}
	if cur.FromMe != row.FromMe || (!row.FromMe && !c.sameUser(row.SenderJID, cur.SenderJID)) {
		return
	}
	cur.MediaType, cur.Text, cur.RawProto = row.MediaType, row.Text, row.RawProto
	if err := c.msgs.Put(cur); err != nil {
		c.log.Warnf("replacing placeholder %s failed: %v", row.ID, err)
		return
	}
	res.Changed = append(res.Changed, msgRef{row.ChatJID, row.ID})
	res.Preview = true
}

// replaceContent swaps the content of stored message targetID for in's,
// if the target is stored and in is from its sender.
func (c *Client) replaceContent(in incoming, targetID string, a analysis, res *applied) bool {
	t, ok := c.target(in.ChatJID, targetID)
	if !ok || t.Deleted || !c.sameSender(in, t) {
		return false
	}
	mediaType, text := a.storedFields()
	found, err := c.msgs.ReplaceContent(t.ChatJID, t.ID, mediaType, text, encodeRawProto(in.Msg), t.HD)
	if err != nil || !found {
		return false
	}
	res.Changed = append(res.Changed, msgRef{t.ChatJID, t.ID})
	res.Preview = true
	return true
}

// storeRow inserts row and reports whether it is new. Without a message
// store there is nothing to compare against, so every message counts as
// new (the chat list is still kept current).
func (c *Client) storeRow(row *msgstore.Message, res *applied) bool {
	if c.msgs == nil {
		return true
	}
	inserted, err := c.msgs.Insert(*row)
	if err != nil {
		c.log.Warnf("storing message %s in %s failed: %v", row.ID, row.ChatJID, err)
		return false
	}
	if !inserted && row.FromMe && row.Status >= msgstore.StatusSent {
		// Already stored (sent from here, or seen in a history sync):
		// arriving again proves at least that it was sent.
		if changed, err := c.msgs.RaiseStatus(row.ChatJID, []string{row.ID}, row.Status); err == nil {
			for _, id := range changed {
				res.Changed = append(res.Changed, msgRef{row.ChatJID, id})
			}
		}
	}
	return inserted
}

// announce updates the chat list for what ingesting a live message did
// and tells the TUI.
func (c *Client) announce(in incoming, res applied, pushName string) {
	seen := map[msgRef]bool{}
	for _, ref := range res.Changed {
		if seen[ref] {
			continue
		}
		seen[ref] = true
		c.emit(MessageEvent{ChatJID: ref.ChatJID, ID: ref.ID})
		if res.Preview {
			c.refreshPreviewIfLatest(ref)
		}
	}
	if res.ChatChanged {
		c.emit(ChatEvent{JID: in.ChatJID})
	}
	if res.Store == nil {
		return
	}

	row := *res.Store
	a := analyze(contentOf(row))
	ctx := context.Background()
	group := isGroupJID(row.ChatJID)

	var chat chatstore.Chat
	if c.chats != nil {
		chat, _, _ = c.chats.Get(row.ChatJID)
		update := chatstore.Chat{
			JID:                row.ChatJID,
			IsGroup:            group,
			LastMessageAt:      row.Timestamp,
			LastMessagePreview: c.previewFor(ctx, row, a),
		}
		// A 1:1 chat is named after the other person: their saved name,
		// else the push name on their own messages (never on yours, which
		// carry your own). A group's name comes from the group, not from
		// whoever spoke last.
		if !group && !row.FromMe {
			name := c.knownName(ctx, row.ChatJID)
			if name == "" {
				name = pushName
			}
			if name != chat.Name {
				update.Name = name
			}
		}
		if err := c.chats.Upsert(update); err != nil {
			c.log.Warnf("chatstore upsert failed for %s: %v", row.ChatJID, err)
		}
		if row.FromMe {
			// You answered from another device: you have read the chat.
			if chat.UnreadCount > 0 || chat.MarkedUnread {
				if err := c.chats.MarkRead(row.ChatJID); err != nil {
					c.log.Warnf("marking %s read failed: %v", row.ChatJID, err)
				}
			}
		} else if err := c.chats.IncrementUnread(row.ChatJID); err != nil {
			c.log.Warnf("chatstore unread increment failed for %s: %v", row.ChatJID, err)
		}
		if timer := int64(a.Ctx.GetExpiration()); timer > 0 && timer != chat.Ephemeral {
			if err := c.chats.SetEphemeral(row.ChatJID, timer); err != nil {
				c.log.Warnf("storing disappearing timer of %s failed: %v", row.ChatJID, err)
			}
		}
	}

	c.emit(MessageEvent{ChatJID: row.ChatJID, ID: row.ID, New: true})
	c.emit(ChatEvent{JID: row.ChatJID})

	c.evMu.RLock()
	callback := c.onIncoming
	c.evMu.RUnlock()
	if callback != nil {
		callback(row)
	}

	c.maybeNotify(ctx, in, row, a, chat, pushName)
}

// previewFor is the chat-list line for a stored message: its preview
// text with mentions as names, prefixed with the sender in groups.
func (c *Client) previewFor(ctx context.Context, m msgstore.Message, a analysis) string {
	if m.Deleted {
		return "🚫 This message was deleted"
	}
	if a.Class != classRow {
		// No content recorded: fall back to the stored text.
		return oneLine(withMediaTag(m.MediaType, m.Text), previewMaxRunes)
	}
	a.Text = resolveMentions(a.Text, a.Ctx.GetMentionedJID(), func(jid string) string {
		return c.ResolveName(ctx, jid)
	})
	text := a.previewText()
	if isGroupJID(m.ChatJID) && a.Kind != KindSystem {
		who := "You"
		if !m.FromMe {
			who = c.ResolveName(ctx, m.SenderJID)
		}
		text = oneLine(who+": "+text, previewMaxRunes)
	}
	return text
}

// refreshPreviewIfLatest rewrites a chat's list line when ref is its
// newest message and has just changed (edited, deleted, upgraded to HD).
func (c *Client) refreshPreviewIfLatest(ref msgRef) {
	if c.msgs == nil || c.chats == nil {
		return
	}
	latest, err := c.msgs.Latest(ref.ChatJID, 1)
	if err != nil || len(latest) != 1 || latest[0].ID != ref.ID {
		return
	}
	c.setPreview(latest[0])
}

// refreshPreview rewrites a chat's list line from its newest stored
// message (after a message was removed).
func (c *Client) refreshPreview(chatJID string) {
	if c.msgs == nil || c.chats == nil {
		return
	}
	latest, err := c.msgs.Latest(chatJID, 1)
	if err != nil {
		return
	}
	if len(latest) == 0 {
		if chat, ok, err := c.chats.Get(chatJID); err == nil && ok && chat.LastMessagePreview != "" {
			if c.chats.SetLastMessage(chatJID, chat.LastMessageAt, "") == nil {
				c.emit(ChatEvent{JID: chatJID})
			}
		}
		return
	}
	c.setPreview(latest[0])
}

func (c *Client) setPreview(m msgstore.Message) {
	preview := c.previewFor(context.Background(), m, analyze(contentOf(m)))
	chat, ok, err := c.chats.Get(m.ChatJID)
	if err != nil || !ok || (chat.LastMessagePreview == preview && chat.LastMessageAt == m.Timestamp) {
		return
	}
	if err := c.chats.SetLastMessage(m.ChatJID, m.Timestamp, preview); err != nil {
		c.log.Warnf("updating preview of %s failed: %v", m.ChatJID, err)
		return
	}
	c.emit(ChatEvent{JID: m.ChatJID})
}

// chatMuted reports whether notifications for chat are off: muted with
// `wa chat mute`, or muted on the phone (forever, or until a time still
// ahead).
func chatMuted(chat chatstore.Chat, now time.Time) bool {
	return chat.Muted || chat.MutedUntil == -1 || chat.MutedUntil > now.UnixMilli()
}

// maybeNotify shows a desktop notification for a newly stored incoming
// message, if the user asked for them and nothing mutes it.
func (c *Client) maybeNotify(ctx context.Context, in incoming, row msgstore.Message, a analysis, chat chatstore.Chat, pushName string) {
	c.evMu.RLock()
	enabled, groups, showPreview := c.notifyEnabled, c.notifyGroups, c.notifyShowPreview
	c.evMu.RUnlock()
	if !enabled || row.FromMe {
		return
	}
	// Messages older than a couple of minutes are an offline backlog being
	// delivered on reconnect, not new — don't pop one toast per message.
	now := time.Now()
	if now.Sub(time.UnixMilli(row.Timestamp)) >= 2*time.Minute {
		return
	}
	group := isGroupJID(row.ChatJID)
	if (group && !groups) || chatMuted(chat, now) {
		return
	}
	// Chats muted on the phone, as whatsmeow recorded it from app state
	// (under the address the phone uses for the chat).
	if in.evt != nil && c.WA != nil && c.WA.Store != nil && c.WA.Store.ChatSettings != nil {
		if settings, err := c.WA.Store.ChatSettings.GetChatSettings(ctx, in.evt.Info.Chat); err == nil && settings.MutedUntil.After(now) {
			return
		}
	}

	title := c.knownName(ctx, row.SenderJID)
	if title == "" {
		title = pushName
	}
	if title == "" {
		title = c.ResolveName(ctx, row.SenderJID)
	}
	if group && chat.Name != "" {
		title += " · " + chat.Name
	}
	body := "New message"
	if showPreview {
		a.Text = resolveMentions(a.Text, a.Ctx.GetMentionedJID(), func(jid string) string {
			return c.ResolveName(ctx, jid)
		})
		if text := a.previewText(); text != "" {
			body = text
		}
	}
	c.sendNotification(title, body)
}

// ingestUnavailable records a view-once message, which WhatsApp doesn't
// deliver to linked devices, as a placeholder so the chat doesn't
// silently miss a message.
func (c *Client) ingestUnavailable(evt *events.UndecryptableMessage) {
	if evt.UnavailableType != events.UnavailableTypeViewOnce || evt.Info.IsFromMe {
		return
	}
	c.ingestMessage(&events.Message{
		Info:    evt.Info,
		Message: &waProto.Message{PlaceholderMessage: &waProto.PlaceholderMessage{}},
	})
}

// --- history sync ---

// historyStatus maps the delivery status recorded in a history sync to
// the store's, for a message you sent.
func historyStatus(wmi *waWeb.WebMessageInfo) msgstore.Status {
	if wmi.Status == nil {
		return msgstore.StatusSent
	}
	switch wmi.GetStatus() {
	case waWeb.WebMessageInfo_ERROR:
		return msgstore.StatusFailed
	case waWeb.WebMessageInfo_PENDING:
		return msgstore.StatusPending
	case waWeb.WebMessageInfo_SERVER_ACK:
		return msgstore.StatusSent
	case waWeb.WebMessageInfo_DELIVERY_ACK:
		return msgstore.StatusDelivered
	case waWeb.WebMessageInfo_READ:
		return msgstore.StatusRead
	case waWeb.WebMessageInfo_PLAYED:
		return msgstore.StatusPlayed
	}
	return msgstore.StatusSent
}

// muteUntilMillis converts a history sync's mute end time to the chat
// store's form: unix millis, -1 for "always", 0 for not muted. The field
// holds seconds; a value too large for that is taken as milliseconds.
func muteUntilMillis(end uint64, now time.Time) int64 {
	switch {
	case end == 0:
		return 0
	case end > math.MaxInt64:
		return -1
	}
	ms := int64(end)
	if ms < 1e11 { // seconds (1e11 s is the year 5138)
		ms *= 1000
	}
	if ms <= now.UnixMilli() {
		return 0
	}
	if ms > now.AddDate(50, 0, 0).UnixMilli() {
		return -1
	}
	return ms
}

// historyChat collects what one history sync says about one chat; two
// conversations of the same person (phone number and @lid) land in one.
type historyChat struct {
	jid    string
	group  bool
	real   bool // has a name or messages: not a bare stub
	name   string
	lastAt int64
	newest *msgstore.Message

	// The phone's settings for the chat, each only when it sent one.
	hasUnread, hasPinned, hasArchive, hasMarked, hasMute, hasTimer bool
	unread                                                         int
	pinned, archive, marked                                        bool
	mute, timer                                                    int64
}

// deferredEvent is an event-like history item, applied after the rows of
// the sync are stored.
type deferredEvent struct {
	ts    int64
	apply func(res *applied)
}

// ingestHistorySync stores a history sync: the initial ones after
// pairing, and the on-demand ones LoadOlder asks for. Rows go in with a
// single insert; reactions, edits, deletes and votes are then folded in
// timestamp order. Chat settings come from the phone's conversation list.
func (c *Client) ingestHistorySync(evt *events.HistorySync) {
	data := evt.Data
	if data == nil || c.WA == nil {
		return
	}
	ctx := context.Background()
	onDemand := data.GetSyncType() == waHistorySync.HistorySync_ON_DEMAND
	now := time.Now()

	// The sync brings its own LID -> phone-number mappings; whatsmeow
	// stores them in the background, possibly after this handler runs.
	overlay := map[string]types.JID{}
	for _, mapping := range data.GetPhoneNumberToLidMappings() {
		lid, err1 := types.ParseJID(mapping.GetLidJID())
		pn, err2 := types.ParseJID(mapping.GetPnJID())
		if err1 != nil || err2 != nil || lid.Server != types.HiddenUserServer {
			continue
		}
		if pn.Server == types.LegacyUserServer {
			pn.Server = types.DefaultUserServer
		}
		if pn.Server == types.DefaultUserServer {
			overlay[lid.User] = pn.ToNonAD()
		}
	}
	canonical := func(raw types.JID, pnHint string) types.JID {
		raw = raw.ToNonAD()
		if raw.Server != types.HiddenUserServer {
			return raw
		}
		if pn, ok := overlay[raw.User]; ok {
			return pn
		}
		if pn := c.pnForLID(raw); !pn.IsEmpty() {
			return pn
		}
		if hint, err := types.ParseJID(pnHint); err == nil && pnHint != "" && hint.Server == types.DefaultUserServer {
			return hint.ToNonAD()
		}
		return raw
	}

	var rows []msgstore.Message
	var deferred []deferredEvent
	chats := map[string]*historyChat{}
	var order []string

	for _, conv := range data.GetConversations() {
		rawID := conv.GetID()
		if rawID == "" || isPseudoChat(rawID) {
			continue
		}
		raw, err := types.ParseJID(rawID)
		if err != nil {
			continue
		}
		chat := canonical(raw, conv.GetPnJID())
		jid := chat.String()
		hc := chats[jid]
		if hc == nil {
			hc = &historyChat{jid: jid, group: chat.Server == types.GroupServer}
			chats[jid] = hc
			order = append(order, jid)
		}
		msgs := conv.GetMessages()
		if conv.GetName() != "" || len(msgs) > 0 {
			hc.real = true
		}
		if hc.group && conv.GetName() != "" {
			hc.name = conv.GetName()
		}
		hc.lastAt = max(hc.lastAt, int64(max(conv.GetLastMsgTimestamp(), conv.GetConversationTimestamp()))*1000)
		if !onDemand {
			// Only what the phone actually sent: later chunks of a sync
			// repeat a conversation with its messages but not its settings.
			if conv.UnreadCount != nil {
				hc.hasUnread, hc.unread = true, max(hc.unread, int(conv.GetUnreadCount()))
			}
			if conv.Pinned != nil {
				hc.hasPinned, hc.pinned = true, hc.pinned || conv.GetPinned() > 0
			}
			if conv.Archived != nil {
				hc.hasArchive, hc.archive = true, hc.archive || conv.GetArchived()
			}
			if conv.MarkedAsUnread != nil {
				hc.hasMarked, hc.marked = true, hc.marked || conv.GetMarkedAsUnread()
			}
			if conv.EphemeralExpiration != nil {
				hc.hasTimer, hc.timer = true, max(hc.timer, int64(conv.GetEphemeralExpiration()))
			}
			if conv.MuteEndTime != nil {
				hc.hasMute = true
				if mute := muteUntilMillis(conv.GetMuteEndTime(), now); mute == -1 || (hc.mute != -1 && mute > hc.mute) {
					hc.mute = mute
				}
			}
		}

		c.storeHistoryPushNames(raw, msgs)
		for _, hm := range msgs {
			wmi := hm.GetMessage()
			if wmi.GetMessage() == nil {
				continue // a system notice without content
			}
			parsed, err := c.WA.ParseWebMessage(raw, wmi)
			if err != nil {
				continue
			}
			in := c.incomingFromEvent(parsed, chat)
			in.evt = nil // parsed, not live: the addressing is rebuilt on demand
			in.History = true
			if in.FromMe {
				in.Status = historyStatus(wmi)
			}

			// ParseWebMessage turns an edit into the edited content under
			// the original message's ID; turn it back into an edit so it
			// is applied (or stored, when it is the only copy).
			if origID := wmi.GetKey().GetID(); origID != "" && origID != in.ID {
				edit := in
				targetID := in.ID
				edit.ID = origID
				edited := in.Msg
				deferred = append(deferred, deferredEvent{in.Timestamp, func(res *applied) {
					c.applyEdit(edit, targetID, edited, res)
				}})
			} else {
				switch classOf(in) {
				case classRow:
					row := in.row(analyze(in.Msg))
					rows = append(rows, row)
					if hc.newest == nil || row.Timestamp > hc.newest.Timestamp {
						newest := row
						hc.newest = &newest
					}
				case classEvent:
					event := in
					deferred = append(deferred, deferredEvent{in.Timestamp, func(res *applied) {
						c.applyEventDepth(ctx, event, res, 0)
					}})
				}
			}

			// Reactions and poll votes also come attached to their message.
			targetID := in.ID
			for _, r := range wmi.GetReactions() {
				actor, ok := c.keyActor(in, r.GetKey(), raw)
				if !ok {
					continue
				}
				actor.Timestamp = r.GetSenderTimestampMS()
				emoji := r.GetText()
				deferred = append(deferred, deferredEvent{actor.Timestamp, func(res *applied) {
					c.applyReaction(actor, targetID, emoji, actor.Timestamp, res)
				}})
			}
			for _, pu := range wmi.GetPollUpdates() {
				actor, ok := c.keyActor(in, pu.GetPollUpdateMessageKey(), raw)
				if !ok {
					continue
				}
				actor.Timestamp = pu.GetSenderTimestampMS()
				selected := pu.GetVote().GetSelectedOptions()
				deferred = append(deferred, deferredEvent{actor.Timestamp, func(res *applied) {
					c.applyPollVote(actor, targetID, selected, actor.Timestamp, res)
				}})
			}
		}
	}

	var inserted []msgstore.Message
	if c.msgs != nil && len(rows) > 0 {
		var err error
		if inserted, err = c.msgs.InsertMany(rows); err != nil {
			c.log.Warnf("msgstore history insert failed: %v", err)
		}
	}

	var changed []msgRef
	if c.msgs != nil {
		sort.SliceStable(deferred, func(i, j int) bool { return deferred[i].ts < deferred[j].ts })
		for _, d := range deferred {
			var res applied
			c.foldMu.Lock()
			d.apply(&res)
			if res.Store != nil && c.storeRow(res.Store, &res) {
				inserted = append(inserted, *res.Store)
			}
			c.foldMu.Unlock()
			changed = append(changed, res.Changed...)
		}
	}

	c.applyHistoryChats(ctx, chats, order, overlay)

	// Names learned from the sync (push names, mappings) change titles.
	c.invalidateNames()
	if onDemand {
		// A backfill the user asked for: tell the open chat what arrived.
		touched := map[string]bool{}
		for _, m := range inserted {
			c.emit(MessageEvent{ChatJID: m.ChatJID, ID: m.ID, New: true})
			touched[m.ChatJID] = true
		}
		for _, ref := range changed {
			c.emit(MessageEvent{ChatJID: ref.ChatJID, ID: ref.ID})
		}
		for jid := range touched {
			c.emit(ChatEvent{JID: jid})
		}
		return
	}
	c.emit(ChatEvent{})
}

// keyActor builds the "who did it" for a reaction or poll vote that a
// history sync attached to message in, from the event's own key.
func (c *Client) keyActor(in incoming, key *waCommon.MessageKey, rawChat types.JID) (incoming, bool) {
	actor := incoming{ChatJID: in.ChatJID, History: true, rawChat: rawChat}
	switch {
	case key.GetFromMe():
		actor.FromMe, actor.SenderJID = true, senderMe
	case key.GetParticipant() != "":
		actor.SenderJID = key.GetParticipant()
	case rawChat.Server != types.GroupServer:
		// In a 1:1 chat, whoever isn't you is the other person.
		actor.SenderJID = rawChat.ToNonAD().String()
	default:
		return actor, false
	}
	return actor, true
}

// applyHistoryChats writes what a history sync said about each chat.
func (c *Client) applyHistoryChats(ctx context.Context, chats map[string]*historyChat, order []string, overlay map[string]types.JID) {
	if c.chats == nil {
		return
	}
	existing := map[string]chatstore.Chat{}
	if list, err := c.chats.List(); err == nil {
		for _, ch := range list {
			existing[ch.JID] = ch
		}
	}

	// Chats stored under an @lid whose phone number this sync revealed.
	for jid := range existing {
		if !isLIDJID(jid) {
			continue
		}
		if lid, err := types.ParseJID(jid); err == nil {
			if pn, ok := overlay[lid.User]; ok && c.mergeLIDChat(jid, pn.String()) {
				if merged, ok, err := c.chats.Get(pn.String()); err == nil && ok {
					existing[pn.String()] = merged
				}
				delete(existing, jid)
			}
		}
	}

	for _, jid := range order {
		hc := chats[jid]
		old, known := existing[jid]
		if !hc.real && !known {
			// A bare conversation stub (no name, no messages): not a chat
			// worth listing.
			continue
		}
		update := chatstore.Chat{JID: jid, IsGroup: hc.group, LastMessageAt: hc.lastAt}
		if hc.group {
			update.Name = hc.name
		} else if name := c.knownName(ctx, jid); name != "" && name != old.Name {
			update.Name = name
		}
		if hc.newest != nil {
			update.LastMessageAt = max(update.LastMessageAt, hc.newest.Timestamp)
			update.LastMessagePreview = c.previewFor(ctx, *hc.newest, analyze(contentOf(*hc.newest)))
		}
		if err := c.chats.Upsert(update); err != nil {
			c.log.Warnf("chatstore upsert failed for %s: %v", jid, err)
			continue
		}
		warn := func(err error) {
			if err != nil {
				c.log.Warnf("storing chat settings of %s failed: %v", jid, err)
			}
		}
		if hc.hasUnread && old.UnreadCount != hc.unread {
			warn(c.chats.SetUnread(jid, hc.unread))
		}
		if hc.hasPinned && old.Pinned != hc.pinned {
			warn(c.chats.SetPinned(jid, hc.pinned))
		}
		if hc.hasArchive && old.Archived != hc.archive {
			warn(c.chats.SetArchived(jid, hc.archive))
		}
		if hc.hasMarked && old.MarkedUnread != hc.marked {
			warn(c.chats.SetMarkedUnread(jid, hc.marked))
		}
		if hc.hasMute && old.MutedUntil != hc.mute {
			warn(c.chats.SetMutedUntil(jid, hc.mute))
		}
		if hc.hasTimer && old.Ephemeral != hc.timer {
			warn(c.chats.SetEphemeral(jid, hc.timer))
		}
	}
}

// storeHistoryPushNames saves the sender push name carried on each
// history-sync message into whatsmeow's contact store. whatsmeow only does
// this itself for live messages and the separate push-name sync, so
// without it most people you haven't saved stay nameless.
func (c *Client) storeHistoryPushNames(chat types.JID, msgs []*waHistorySync.HistorySyncMsg) {
	if c.WA == nil || c.WA.Store == nil || c.WA.Store.Contacts == nil {
		return
	}
	ctx := context.Background()
	for _, hm := range msgs {
		wmi := hm.GetMessage()
		push := wmi.GetPushName()
		if push == "" || push == "-" || wmi.GetKey().GetFromMe() {
			continue
		}
		jid := chat
		if p := wmi.GetParticipant(); p != "" {
			jid, _ = types.ParseJID(p)
		} else if p := wmi.GetKey().GetParticipant(); p != "" {
			jid, _ = types.ParseJID(p)
		}
		if jid.IsEmpty() || jid.Server == types.GroupServer {
			continue
		}
		if _, _, err := c.WA.Store.Contacts.PutPushName(ctx, jid.ToNonAD(), push); err != nil {
			c.log.Warnf("storing history push name for %s failed: %v", jid, err)
		}
	}
}
