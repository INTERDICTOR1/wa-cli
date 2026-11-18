package whatsapp

import (
	"context"
	"encoding/hex"
	"strings"
	"time"

	waProto "go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	"github.com/codebyoketch/wa-cli/internal/msgstore"
)

// WhatsApp delivers reactions, edits, deletes, poll votes and the HD copy
// of a photo as messages of their own. "Folding" applies each of those to
// the message it is about, so a chat holds exactly one row per real
// message. The same code serves live messages, history syncs and the
// one-time pass over rows imported from the old JSON store.

// incoming is one message as the ingest code sees it, wherever it came
// from.
type incoming struct {
	ChatJID   string // canonical chat JID
	ID        string
	SenderJID string // "me" for your own, otherwise the sender's JID
	FromMe    bool
	Timestamp int64 // unix millis
	// Msg is the content with wrappers removed.
	Msg    *waProto.Message
	Status msgstore.Status
	// History marks messages that are not arriving live: history syncs
	// and stored rows.
	History bool

	// evt is the live event, when there is one. Decrypting a poll vote or
	// an encrypted edit needs the chat and sender exactly as WhatsApp
	// addressed them; without evt they are rebuilt from rawChat and
	// SenderJID.
	evt     *events.Message
	rawChat types.JID
}

// msgRef names a stored message.
type msgRef struct{ ChatJID, ID string }

// applied is what ingesting one message did.
type applied struct {
	// Store is a real message to store under its own ID; the caller
	// inserts it (or, for a legacy row, rewrites the row in place).
	Store *msgstore.Message
	// Changed lists existing messages that were changed in place.
	Changed []msgRef
	// Preview is set when a change may have altered what the chat list
	// shows for the chat's last message (an edit, a delete, an HD copy).
	Preview bool
	// ChatChanged is set when the chat itself changed (its
	// disappearing-messages timer).
	ChatChanged bool
}

// secretDecrypter decrypts messages locked with another message's secret
// key. *whatsmeow.Client satisfies it; tests use a stub.
type secretDecrypter interface {
	DecryptReaction(ctx context.Context, reaction *events.Message) (*waProto.ReactionMessage, error)
	DecryptPollVote(ctx context.Context, vote *events.Message) (*waProto.PollVoteMessage, error)
	DecryptSecretEncryptedMessage(ctx context.Context, evt *events.Message) (*waProto.Message, error)
}

func (c *Client) decrypter() secretDecrypter {
	if c.decryptOverride != nil {
		return c.decryptOverride
	}
	if c.WA == nil {
		return nil
	}
	return c.WA
}

// row builds the store row for a real message.
func (in incoming) row(a analysis) msgstore.Message {
	mediaType, text := a.storedFields()
	return msgstore.Message{
		ID:        in.ID,
		ChatJID:   in.ChatJID,
		SenderJID: in.SenderJID,
		Timestamp: in.Timestamp,
		Text:      text,
		FromMe:    in.FromMe,
		MediaType: mediaType,
		RawProto:  encodeRawProto(in.Msg),
		Status:    in.Status,
	}
}

// dualUpload returns the association of a message that is the second
// copy of another message's media (the HD version of a photo or video,
// the HEVC encoding of a video, the clip of a motion photo), or nil.
func dualUpload(msg *waProto.Message, ownID string) *waProto.MessageAssociation {
	assoc := msg.GetMessageContextInfo().GetMessageAssociation()
	if assoc == nil {
		return nil
	}
	switch assoc.GetAssociationType() {
	case waProto.MessageAssociation_HD_IMAGE_DUAL_UPLOAD,
		waProto.MessageAssociation_HD_VIDEO_DUAL_UPLOAD,
		waProto.MessageAssociation_HEVC_VIDEO_DUAL_UPLOAD,
		waProto.MessageAssociation_MOTION_PHOTO:
	default:
		return nil
	}
	if parent := assoc.GetParentMessageKey().GetID(); parent == "" || parent == ownID {
		return nil
	}
	return assoc
}

// classOf says how to ingest in: as a row, as an event applied to
// another message, or not at all.
func classOf(in incoming) msgClass {
	if in.Msg == nil {
		return classDrop
	}
	if dualUpload(in.Msg, in.ID) != nil {
		return classEvent
	}
	return analyze(in.Msg).Class
}

// applyEvent folds an event-like message into the message (or chat) it is
// about. A target that isn't stored means the event is silently dropped.
func (c *Client) applyEvent(ctx context.Context, in incoming) applied {
	var res applied
	c.applyEventDepth(ctx, in, &res, 0)
	return res
}

func (c *Client) applyEventDepth(ctx context.Context, in incoming, res *applied, depth int) {
	msg := in.Msg
	if msg == nil || depth > 3 {
		return
	}
	// An HD copy arrives wrapped: the link to the standard-quality message
	// is on the wrapper (that is how WhatsApp really sends it) or on the
	// media inside. Open the wrapper first, or the wrapper itself would be
	// mistaken for the media.
	if wrapper := msg.GetAssociatedChildMessage(); wrapper != nil {
		child := unwrapMessage(wrapper.GetMessage())
		if child == nil {
			return
		}
		assoc := child.GetMessageContextInfo().GetMessageAssociation()
		if assoc == nil {
			assoc = msg.GetMessageContextInfo().GetMessageAssociation()
		}
		c.applyChild(ctx, in, child, assoc, res, depth)
		return
	}
	if assoc := dualUpload(msg, in.ID); assoc != nil {
		c.applyChild(ctx, in, msg, assoc, res, depth)
		return
	}
	switch {
	case msg.GetReactionMessage() != nil:
		r := msg.GetReactionMessage()
		c.applyReaction(in, r.GetKey().GetID(), r.GetText(), r.GetSenderTimestampMS(), res)

	case msg.GetEncReactionMessage() != nil:
		var r *waProto.ReactionMessage
		c.tryDecrypt(in, func(d secretDecrypter, evt *events.Message) (err error) {
			r, err = d.DecryptReaction(ctx, evt)
			return err
		})
		if r != nil {
			target := msg.GetEncReactionMessage().GetTargetMessageKey().GetID()
			c.applyReaction(in, target, r.GetText(), r.GetSenderTimestampMS(), res)
		}

	case msg.GetPollUpdateMessage() != nil:
		pu := msg.GetPollUpdateMessage()
		var vote *waProto.PollVoteMessage
		c.tryDecrypt(in, func(d secretDecrypter, evt *events.Message) (err error) {
			vote, err = d.DecryptPollVote(ctx, evt)
			return err
		})
		if vote != nil {
			c.applyPollVote(in, pu.GetPollCreationMessageKey().GetID(), vote.GetSelectedOptions(), pu.GetSenderTimestampMS(), res)
		}

	case msg.GetSecretEncryptedMessage() != nil:
		enc := msg.GetSecretEncryptedMessage()
		var dec *waProto.Message
		c.tryDecrypt(in, func(d secretDecrypter, evt *events.Message) (err error) {
			dec, err = d.DecryptSecretEncryptedMessage(ctx, evt)
			return err
		})
		dec = unwrapMessage(dec)
		if dec == nil {
			return
		}
		target := enc.GetTargetMessageKey().GetID()
		if pm := dec.GetProtocolMessage(); pm != nil && pm.GetType() == waProto.ProtocolMessage_MESSAGE_EDIT {
			if id := pm.GetKey().GetID(); id != "" {
				target = id
			}
			c.applyEdit(in, target, unwrapMessage(pm.GetEditedMessage()), res)
			return
		}
		if analyze(dec).Class == classEvent {
			inner := in
			inner.Msg = dec
			c.applyEventDepth(ctx, inner, res, depth+1)
			return
		}
		// Otherwise the plaintext is the target's new content.
		c.applyEdit(in, target, dec, res)

	case msg.GetProtocolMessage() != nil:
		pm := msg.GetProtocolMessage()
		switch pm.GetType() {
		case waProto.ProtocolMessage_REVOKE:
			c.applyRevoke(in, pm.GetKey().GetID(), res)
		case waProto.ProtocolMessage_MESSAGE_EDIT:
			c.applyEdit(in, pm.GetKey().GetID(), unwrapMessage(pm.GetEditedMessage()), res)
		case waProto.ProtocolMessage_EPHEMERAL_SETTING:
			if c.setEphemeral(in.ChatJID, int64(pm.GetEphemeralExpiration())) {
				res.ChatChanged = true
			}
		}
	}
}

// tryDecrypt runs fn with each way the event may have been addressed
// until one decrypts. Live events have exactly one; a stored row no
// longer says which of your (or the sender's) two addresses was used.
func (c *Client) tryDecrypt(in incoming, fn func(secretDecrypter, *events.Message) error) {
	d := c.decrypter()
	if d == nil {
		return
	}
	for _, evt := range c.decryptEvents(in) {
		if fn(d, evt) == nil {
			return
		}
	}
}

func (c *Client) decryptEvents(in incoming) []*events.Message {
	if in.evt != nil {
		if in.evt.Message == in.Msg {
			return []*events.Message{in.evt}
		}
		// The content was unwrapped further than whatsmeow did.
		evt := *in.evt
		evt.Message = in.Msg
		return []*events.Message{&evt}
	}
	chat := in.rawChat
	if chat.IsEmpty() {
		parsed, err := types.ParseJID(in.ChatJID)
		if err != nil {
			return nil
		}
		chat = parsed
	}
	var senders []types.JID
	if in.FromMe {
		senders = []types.JID{c.ownLID(), c.ownJID()}
	} else if sender, err := types.ParseJID(in.SenderJID); err == nil {
		sender = sender.ToNonAD()
		senders = []types.JID{sender, c.lidForPN(sender), c.pnForLID(sender)}
	}
	var out []*events.Message
	for _, sender := range senders {
		if sender.IsEmpty() {
			continue
		}
		out = append(out, &events.Message{
			Info: types.MessageInfo{
				MessageSource: types.MessageSource{
					Chat:     chat,
					Sender:   sender,
					IsFromMe: in.FromMe,
					IsGroup:  chat.Server == types.GroupServer,
				},
				ID: in.ID,
			},
			Message: in.Msg,
		})
	}
	return out
}

// target finds the stored message an event refers to: in the event's own
// chat or, when the same 1:1 conversation is also stored under the
// person's other address, there. It never reaches into an unrelated
// chat: an event can only touch messages of its own conversation.
func (c *Client) target(chatJID, id string) (msgstore.Message, bool) {
	if c.msgs == nil || id == "" {
		return msgstore.Message{}, false
	}
	if m, ok, err := c.msgs.Get(chatJID, id); err == nil && ok {
		return m, true
	}
	if isGroupJID(chatJID) {
		return msgstore.Message{}, false
	}
	m, ok, err := c.msgs.Find(id)
	if err != nil || !ok || isGroupJID(m.ChatJID) {
		return msgstore.Message{}, false
	}
	if c.canonicalChatString(m.ChatJID) != c.canonicalChatString(chatJID) {
		return msgstore.Message{}, false
	}
	return m, true
}

// actor is who an event is attributed to in a message's reactions and
// poll votes: "me", or the person's canonical JID.
func (c *Client) actor(in incoming) string {
	if in.FromMe {
		return senderMe
	}
	return c.canonicalUser(in.SenderJID)
}

// sameSender reports whether in was sent by the sender of stored message t.
func (c *Client) sameSender(in incoming, t msgstore.Message) bool {
	if in.FromMe || t.FromMe {
		return in.FromMe && t.FromMe
	}
	return c.sameUser(in.SenderJID, t.SenderJID)
}

func (c *Client) applyReaction(in incoming, targetID, emoji string, tsMS int64, res *applied) {
	t, ok := c.target(in.ChatJID, targetID)
	if !ok || t.Deleted {
		return
	}
	if tsMS <= 0 {
		tsMS = in.Timestamp
	}
	who := c.actor(in)
	if in.History {
		// A history sync can deliver a reaction that was since replaced.
		for _, r := range t.Reactions {
			if r.SenderJID == who && r.Timestamp > tsMS {
				return
			}
		}
	}
	found, err := c.msgs.SetReaction(t.ChatJID, t.ID, msgstore.Reaction{SenderJID: who, Emoji: emoji, Timestamp: tsMS})
	if err != nil {
		c.log.Warnf("storing reaction on %s failed: %v", t.ID, err)
		return
	}
	if found {
		res.Changed = append(res.Changed, msgRef{t.ChatJID, t.ID})
	}
}

func (c *Client) applyPollVote(in incoming, targetID string, selected [][]byte, tsMS int64, res *applied) {
	t, ok := c.target(in.ChatJID, targetID)
	if !ok || t.Deleted {
		return
	}
	if tsMS <= 0 {
		tsMS = in.Timestamp
	}
	who := c.actor(in)
	if in.History {
		for _, v := range t.PollVotes {
			if v.VoterJID == who && v.Timestamp > tsMS {
				return
			}
		}
	}
	options := make([]string, 0, len(selected))
	for _, hash := range selected {
		options = append(options, hex.EncodeToString(hash))
	}
	found, err := c.msgs.SetPollVote(t.ChatJID, t.ID, msgstore.PollVote{VoterJID: who, Options: options, Timestamp: tsMS})
	if err != nil {
		c.log.Warnf("storing poll vote on %s failed: %v", t.ID, err)
		return
	}
	if found {
		res.Changed = append(res.Changed, msgRef{t.ChatJID, t.ID})
	}
}

func (c *Client) applyRevoke(in incoming, targetID string, res *applied) {
	c.revoke(in, targetID, res, true)
}

// revoke applies a "delete for everyone". Anyone may delete their own
// message; someone else's only a group admin may - and WhatsApp's servers
// can't check that (they don't see what a message says), so it is checked
// here. With mayDefer, a deletion whose sender can't be checked yet waits
// for the group's member list instead of being dropped.
func (c *Client) revoke(in incoming, targetID string, res *applied, mayDefer bool) {
	t, ok := c.target(in.ChatJID, targetID)
	if !ok || t.Deleted {
		return
	}
	if !c.sameSender(in, t) {
		if !isGroupJID(t.ChatJID) {
			return
		}
		switch c.adminOf(t.ChatJID, in) {
		case adminNo:
			return
		case adminUnknown:
			// Deleting can't be undone, so an unchecked claim deletes
			// nothing.
			if mayDefer {
				c.deferRevoke(t.ChatJID, in, targetID)
			}
			return
		}
	}
	found, err := c.msgs.MarkDeleted(t.ChatJID, t.ID)
	if err != nil {
		c.log.Warnf("marking %s deleted failed: %v", t.ID, err)
		return
	}
	if found {
		res.Changed = append(res.Changed, msgRef{t.ChatJID, t.ID})
		res.Preview = true
	}
}

// adminAnswer is whether someone is an admin of a group.
type adminAnswer int

const (
	adminUnknown adminAnswer = iota // the group's member list isn't loaded
	adminNo
	adminYes
)

// adminOf reports whether the sender of in is an admin of the group.
func (c *Client) adminOf(groupJID string, in incoming) adminAnswer {
	if in.FromMe {
		// From one of your own devices, which checked your rights before
		// sending. (The member list holds the other members only.)
		return adminYes
	}
	members, loaded := c.cachedMembers(groupJID)
	if !loaded {
		return adminUnknown
	}
	for _, m := range members {
		if c.sameUser(m.JID, in.SenderJID) && m.Admin {
			return adminYes
		}
	}
	return adminNo
}

// pendingRevoke is a deletion of someone else's message that waits for
// the group's member list, to check that its sender is an admin.
type pendingRevoke struct {
	in       incoming
	targetID string
}

// maxPendingRevokes bounds how many deletions wait per group. Moderation
// is a handful of them; more than this is a flood, and is dropped.
const maxPendingRevokes = 32

// revokeCheckTimeout is how long fetching a member list may take.
const revokeCheckTimeout = 30 * time.Second

// deferRevoke holds a deletion until the group's member list has been
// fetched (one fetch per group, however many are waiting), then applies
// it if its sender turns out to be an admin. Deletions that aren't
// arriving live have nothing current to be checked against and are
// dropped: the message stays.
func (c *Client) deferRevoke(groupJID string, in incoming, targetID string) {
	if in.History {
		return
	}
	c.groupMu.Lock()
	queue, fetching := c.pendingRevokes[groupJID]
	if len(queue) < maxPendingRevokes {
		if c.pendingRevokes == nil {
			c.pendingRevokes = map[string][]pendingRevoke{}
		}
		c.pendingRevokes[groupJID] = append(queue, pendingRevoke{in, targetID})
	}
	c.groupMu.Unlock()
	if fetching {
		return
	}
	c.background.Add(1)
	go func() {
		defer c.background.Done()
		c.settleRevokes(groupJID)
	}()
}

// settleRevokes fetches a group's member list and decides the deletions
// waiting for it. If the list can't be fetched they are dropped.
func (c *Client) settleRevokes(groupJID string) {
	ctx, cancel := context.WithTimeout(context.Background(), revokeCheckTimeout)
	defer cancel()
	err := c.fetchMembers(ctx, groupJID)

	c.groupMu.Lock()
	queue := c.pendingRevokes[groupJID]
	delete(c.pendingRevokes, groupJID)
	c.groupMu.Unlock()
	if err != nil {
		c.log.Warnf("left %d message(s) in %s undeleted: couldn't check who may delete them: %v", len(queue), groupJID, err)
		return
	}
	for _, p := range queue {
		var res applied
		c.foldMu.Lock()
		c.revoke(p.in, p.targetID, &res, false)
		c.foldMu.Unlock()
		c.announce(p.in, res, "")
	}
}

func (c *Client) applyEdit(in incoming, targetID string, edited *waProto.Message, res *applied) {
	if edited == nil || targetID == "" {
		return
	}
	t, ok := c.target(in.ChatJID, targetID)
	if !ok {
		if !in.History {
			return
		}
		// In a history sync an edit can be the only copy of the message.
		a := analyze(edited)
		if a.Class != classRow {
			return
		}
		row := in
		row.ID, row.Msg = targetID, edited
		stored := row.row(a)
		stored.Edited = true
		res.Store = &stored
		return
	}
	if t.Deleted || !c.sameSender(in, t) {
		return
	}
	merged := mergeEdit(contentOf(t), edited)
	a := analyze(merged)
	if a.Class != classRow {
		return
	}
	_, text := a.storedFields()
	found, err := c.msgs.ApplyEdit(t.ChatJID, t.ID, text, encodeRawProto(merged))
	if err != nil {
		c.log.Warnf("applying edit to %s failed: %v", t.ID, err)
		return
	}
	if found {
		res.Changed = append(res.Changed, msgRef{t.ChatJID, t.ID})
		res.Preview = true
	}
}

// captionOf returns a pointer to the caption field of msg's media, and
// its context info field, or nils when msg has no captioned media.
func captionOf(msg *waProto.Message) (caption **string, ctx **waProto.ContextInfo) {
	switch {
	case msg.GetImageMessage() != nil:
		return &msg.ImageMessage.Caption, &msg.ImageMessage.ContextInfo
	case msg.GetVideoMessage() != nil:
		return &msg.VideoMessage.Caption, &msg.VideoMessage.ContextInfo
	case msg.GetDocumentMessage() != nil:
		return &msg.DocumentMessage.Caption, &msg.DocumentMessage.ContextInfo
	}
	return nil, nil
}

// hasMediaRef reports whether msg's media points at an uploaded file.
func hasMediaRef(msg *waProto.Message) bool {
	a := analyze(msg)
	return a.Download != nil && (a.Download.GetDirectPath() != "" || len(a.Download.GetMediaKey()) > 0)
}

// mergeEdit returns the content a message has after an edit. The media of
// a message can't change: WhatsApp sends a caption edit as a media
// message carrying only the new caption (or as plain text), so the
// stored media is kept and only its caption and mentions are replaced. A
// reply stays a reply.
func mergeEdit(old, edited *waProto.Message) *waProto.Message {
	if old == nil || edited == nil {
		return edited
	}
	oldCaption, oldCtx := captionOf(old)
	if oldCaption != nil && !hasMediaRef(edited) {
		ea := analyze(edited)
		if ea.Class == classRow && (ea.Kind == KindText || ea.Download != nil) {
			merged := proto.Clone(old).(*waProto.Message)
			caption, ctx := captionOf(merged)
			*caption = proto.String(ea.Text)
			if ea.Text == "" {
				*caption = nil
			}
			*ctx = carryQuote(*oldCtx, ea.Ctx)
			return merged
		}
	}

	// Text (or anything else): the edit is the new content. Keep the
	// quote of a reply, which edits don't repeat.
	oa := analyze(old)
	if oa.Ctx.GetStanzaID() == "" {
		return edited
	}
	ea := analyze(edited)
	if ea.Class != classRow || ea.Kind != KindText || ea.Ctx.GetStanzaID() != "" {
		return edited
	}
	return &waProto.Message{
		ExtendedTextMessage: &waProto.ExtendedTextMessage{
			Text:        proto.String(ea.Text),
			ContextInfo: carryQuote(oa.Ctx, ea.Ctx),
		},
		MessageContextInfo: edited.GetMessageContextInfo(),
	}
}

// carryQuote returns the edit's context info with the original's reply
// quote added when the edit has none.
func carryQuote(old, edited *waProto.ContextInfo) *waProto.ContextInfo {
	if old.GetStanzaID() == "" || edited.GetStanzaID() != "" {
		if edited == nil {
			// Nothing in the edit: keep what the original had, minus its
			// mentions (the text they pointed into is gone).
			if old == nil {
				return nil
			}
			kept := proto.Clone(old).(*waProto.ContextInfo)
			kept.MentionedJID = nil
			return kept
		}
		return edited
	}
	out := &waProto.ContextInfo{}
	if edited != nil {
		out = proto.Clone(edited).(*waProto.ContextInfo)
	}
	out.StanzaID = old.StanzaID
	out.Participant = old.Participant
	out.QuotedMessage = old.QuotedMessage
	if out.Expiration == nil {
		out.Expiration = old.Expiration
	}
	return out
}

// applyChild handles a message that belongs to another one: the HD copy
// of a photo or video replaces the standard copy it was sent after; other
// second copies are dropped; anything else (an album item) is a message
// of its own.
func (c *Client) applyChild(ctx context.Context, in incoming, child *waProto.Message, assoc *waProto.MessageAssociation, res *applied, depth int) {
	in.Msg = child
	parentID := assoc.GetParentMessageKey().GetID()
	if parentID == in.ID {
		parentID = ""
	}
	var hd bool
	switch assoc.GetAssociationType() {
	case waProto.MessageAssociation_HD_IMAGE_DUAL_UPLOAD, waProto.MessageAssociation_HD_VIDEO_DUAL_UPLOAD:
		hd = true
		if parent, ok := c.target(in.ChatJID, parentID); ok {
			c.replaceWithHD(in, parent, child, res)
			return
		}
	case waProto.MessageAssociation_HEVC_VIDEO_DUAL_UPLOAD, waProto.MessageAssociation_MOTION_PHOTO:
		// Another encoding of media that is already there.
		if _, ok := c.target(in.ChatJID, parentID); ok {
			return
		}
	}

	// A message of its own (or its parent isn't stored).
	a := analyze(child)
	switch a.Class {
	case classRow:
		row := in.row(a)
		row.HD = hd && a.Download != nil
		res.Store = &row
	case classEvent:
		if dualUpload(child, in.ID) == nil { // not this same association again
			c.applyEventDepth(ctx, in, res, depth+1)
		}
	}
}

func (c *Client) replaceWithHD(in incoming, parent msgstore.Message, child *waProto.Message, res *applied) {
	if parent.Deleted || !c.sameSender(in, parent) {
		return
	}
	// The HD copy usually has no caption of its own: keep the parent's,
	// along with its reply quote and mentions.
	if caption, ctx := captionOf(child); caption != nil {
		if old := contentOf(parent); old != nil {
			if oldCaption, oldCtx := captionOf(old); oldCaption != nil {
				if *caption == nil || strings.TrimSpace(**caption) == "" {
					*caption = *oldCaption
				}
				if *ctx == nil {
					*ctx = *oldCtx
				}
			}
		}
	}
	a := analyze(child)
	if a.Class != classRow || a.Download == nil {
		return
	}
	mediaType, text := a.storedFields()
	found, err := c.msgs.ReplaceContent(parent.ChatJID, parent.ID, mediaType, text, encodeRawProto(child), true)
	if err != nil {
		c.log.Warnf("replacing %s with its HD copy failed: %v", parent.ID, err)
		return
	}
	if found {
		res.Changed = append(res.Changed, msgRef{parent.ChatJID, parent.ID})
		res.Preview = true
	}
}

// setEphemeral records a chat's disappearing-messages timer (seconds, 0
// for off) and reports whether it changed.
func (c *Client) setEphemeral(chatJID string, seconds int64) bool {
	if c.chats == nil {
		return false
	}
	chat, ok, err := c.chats.Get(chatJID)
	if err != nil || !ok || chat.Ephemeral == seconds {
		return false
	}
	if err := c.chats.SetEphemeral(chatJID, seconds); err != nil {
		c.log.Warnf("storing disappearing timer of %s failed: %v", chatJID, err)
		return false
	}
	return true
}

// legacyFoldMeta is the message-store meta key the storage layer sets to
// "pending" after importing the old JSON store, whose rows were stored
// raw: reactions, edits, deletes, votes and plumbing are all rows there.
const legacyFoldMeta = "legacy_fold"

// foldLegacy walks the imported rows once, applies every event-like row
// to its target and removes it along with the noise, then marks the pass
// done. Interrupting it is safe: applying an event twice gives the same
// result, and rows are only removed after the walk.
func (c *Client) foldLegacy(ctx context.Context) error {
	if c.msgs == nil {
		return nil
	}
	state, err := c.msgs.Meta(legacyFoldMeta)
	if err != nil || state != "pending" {
		return err
	}

	var remove []msgRef
	err = c.msgs.Each(func(m msgstore.Message) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		raw := decodeRawProto(m.RawProto)
		if raw == nil {
			return nil // nothing recorded beyond the text: keep as it is
		}
		in := incoming{
			ChatJID:   m.ChatJID,
			ID:        m.ID,
			SenderJID: m.SenderJID,
			FromMe:    m.FromMe,
			Timestamp: m.Timestamp,
			Msg:       unwrapMessage(raw),
			Status:    m.Status,
			History:   true,
		}
		switch classOf(in) {
		case classDrop:
			remove = append(remove, msgRef{m.ChatJID, m.ID})
		case classEvent:
			res := c.applyEvent(ctx, in)
			if res.Store != nil && res.Store.ID == m.ID {
				// It turned out to be a message of its own (an HD copy
				// whose parent is gone): rewrite the row in place.
				return c.msgs.Put(*res.Store)
			}
			if res.Store != nil {
				if _, err := c.msgs.Insert(*res.Store); err != nil {
					return err
				}
			}
			remove = append(remove, msgRef{m.ChatJID, m.ID})
		case classRow:
			return c.normalizeLegacyRow(m, in)
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, ref := range remove {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := c.msgs.Delete(ref.ChatJID, ref.ID); err != nil {
			return err
		}
	}
	return c.msgs.SetMeta(legacyFoldMeta, "done")
}

// normalizeLegacyRow brings an old row's media type and searchable text
// in line with what is stored for new messages (the old store kept no
// text for templates, polls, locations and the like).
func (c *Client) normalizeLegacyRow(m msgstore.Message, in incoming) error {
	if m.Deleted {
		return nil
	}
	mediaType, text := analyze(in.Msg).storedFields()
	if mediaType == m.MediaType && text == m.Text {
		return nil
	}
	// Re-read: an event applied earlier in this pass may have changed it.
	cur, ok, err := c.msgs.Get(m.ChatJID, m.ID)
	if err != nil || !ok || cur.Deleted || cur.RawProto != m.RawProto {
		return err
	}
	cur.MediaType, cur.Text = mediaType, text
	return c.msgs.Put(cur)
}
