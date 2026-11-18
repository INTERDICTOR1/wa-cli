package whatsapp

import (
	"context"
	"strings"

	"go.mau.fi/whatsmeow/types"
)

// WhatsApp addresses a person in two ways: by phone number
// ("<digits>@s.whatsapp.net") and by a privacy ID ("<digits>@lid"). The
// same 1:1 conversation can arrive under either, so every chat JID that
// enters the stores goes through canonicalChat first: the phone-number
// form whenever the mapping is known, otherwise the LID.

// lidSource maps between a person's two addresses. whatsmeow's LID store
// satisfies it; tests use a stub.
type lidSource interface {
	GetPNForLID(ctx context.Context, lid types.JID) (types.JID, error)
	GetLIDForPN(ctx context.Context, pn types.JID) (types.JID, error)
}

// contactSource looks up the names known for a user. whatsmeow's contact
// store satisfies it; tests use a stub.
type contactSource interface {
	GetContact(ctx context.Context, user types.JID) (types.ContactInfo, error)
}

// lids returns the LID mapping source, or nil when there is none (a
// Client built without a whatsmeow client, as in tests).
func (c *Client) lids() lidSource {
	if c.lidOverride != nil {
		return c.lidOverride
	}
	if c.WA == nil || c.WA.Store == nil || c.WA.Store.LIDs == nil {
		return nil
	}
	return c.WA.Store.LIDs
}

// contacts returns the contact name source, or nil when there is none.
func (c *Client) contacts() contactSource {
	if c.contactOverride != nil {
		return c.contactOverride
	}
	if c.WA == nil || c.WA.Store == nil || c.WA.Store.Contacts == nil {
		return nil
	}
	return c.WA.Store.Contacts
}

// ownJID returns your phone-number JID without the device part, or the
// empty JID when not logged in.
func (c *Client) ownJID() types.JID {
	if !c.selfJID.IsEmpty() {
		return c.selfJID.ToNonAD()
	}
	if c.WA == nil || c.WA.Store == nil {
		return types.EmptyJID
	}
	return c.WA.Store.GetJID().ToNonAD()
}

// ownLID returns your LID without the device part, if known.
func (c *Client) ownLID() types.JID {
	if !c.selfLID.IsEmpty() {
		return c.selfLID.ToNonAD()
	}
	if c.WA == nil || c.WA.Store == nil {
		return types.EmptyJID
	}
	return c.WA.Store.GetLID().ToNonAD()
}

// isMe reports whether jid (in either form) is you.
func (c *Client) isMe(jid types.JID) bool {
	if jid.User == "" {
		return false
	}
	switch jid.Server {
	case types.DefaultUserServer:
		return jid.User == c.ownJID().User
	case types.HiddenUserServer:
		return jid.User == c.ownLID().User
	}
	return false
}

// pnForLID returns the phone-number JID for lid, or the empty JID when
// the mapping isn't known.
func (c *Client) pnForLID(lid types.JID) types.JID {
	if lid.Server != types.HiddenUserServer {
		return types.EmptyJID
	}
	src := c.lids()
	if src == nil {
		return types.EmptyJID
	}
	pn, err := src.GetPNForLID(context.Background(), lid.ToNonAD())
	if err != nil || pn.IsEmpty() {
		return types.EmptyJID
	}
	return pn.ToNonAD()
}

// lidForPN returns the LID for a phone-number JID, or the empty JID.
func (c *Client) lidForPN(pn types.JID) types.JID {
	if pn.Server != types.DefaultUserServer {
		return types.EmptyJID
	}
	src := c.lids()
	if src == nil {
		return types.EmptyJID
	}
	lid, err := src.GetLIDForPN(context.Background(), pn.ToNonAD())
	if err != nil || lid.IsEmpty() {
		return types.EmptyJID
	}
	return lid.ToNonAD()
}

// canonicalChat returns the JID a chat is stored under: no device part,
// and the phone-number form of an @lid user when the mapping is known.
// Groups and everything else are returned unchanged.
func (c *Client) canonicalChat(jid types.JID) types.JID {
	jid = jid.ToNonAD()
	if jid.Server == types.HiddenUserServer {
		if pn := c.pnForLID(jid); !pn.IsEmpty() {
			return pn
		}
	}
	return jid
}

// canonicalChatString is canonicalChat for a JID string; a string that
// doesn't parse is returned as it is.
func (c *Client) canonicalChatString(jid string) string {
	if !strings.Contains(jid, "@") {
		return jid
	}
	parsed, err := types.ParseJID(jid)
	if err != nil {
		return jid
	}
	return c.canonicalChat(parsed).String()
}

// canonicalUser returns one stable key for a person whichever address a
// message used: "me" for you, otherwise the canonical JID string.
func (c *Client) canonicalUser(jid string) string {
	if jid == "" || jid == senderMe {
		return jid
	}
	parsed, err := types.ParseJID(jid)
	if err != nil {
		return jid
	}
	if c.isMe(parsed) {
		return senderMe
	}
	return c.canonicalChat(parsed).String()
}

// sameUser reports whether two sender JID strings (or "me") are the same
// person.
func (c *Client) sameUser(a, b string) bool {
	if a == b {
		return true
	}
	return c.canonicalUser(a) == c.canonicalUser(b)
}

// senderMe is how the message store records you as a sender.
const senderMe = "me"

// isGroupJID reports whether jid is a group chat.
func isGroupJID(jid string) bool {
	return strings.HasSuffix(jid, "@"+types.GroupServer)
}

// isLIDJID reports whether jid is an @lid user.
func isLIDJID(jid string) bool {
	return strings.HasSuffix(jid, "@"+types.HiddenUserServer)
}

// mergeLIDChat folds a chat stored under lid into the chat stored under
// pn, messages included. A no-op when nothing is stored under lid.
func (c *Client) mergeLIDChat(lid, pn string) (merged bool) {
	if lid == pn || lid == "" || pn == "" {
		return false
	}
	if c.msgs != nil {
		if err := c.msgs.MoveChat(lid, pn); err != nil {
			c.log.Warnf("moving messages of %s to %s failed: %v", lid, pn, err)
			return false
		}
	}
	if c.chats != nil {
		if _, ok, err := c.chats.Get(lid); err != nil || !ok {
			return false
		}
		if err := c.chats.Merge(lid, pn); err != nil {
			c.log.Warnf("merging chat %s into %s failed: %v", lid, pn, err)
			return false
		}
		return true
	}
	return false
}

// mergeLIDChats merges every stored @lid chat whose phone number is now
// known into its phone-number chat. Local only: one chat listing plus a
// cached mapping lookup per @lid chat.
func (c *Client) mergeLIDChats() (merged int) {
	if c.chats == nil || c.lids() == nil {
		return 0
	}
	list, err := c.chats.List()
	if err != nil {
		c.log.Warnf("listing chats for the LID merge failed: %v", err)
		return 0
	}
	for _, ch := range list {
		if !isLIDJID(ch.JID) {
			continue
		}
		lid, err := types.ParseJID(ch.JID)
		if err != nil {
			continue
		}
		pn := c.pnForLID(lid)
		if pn.IsEmpty() {
			continue
		}
		if c.mergeLIDChat(ch.JID, pn.String()) {
			merged++
		}
	}
	return merged
}

// noteLIDChat is the per-message version of mergeLIDChats: the first time
// this run a message arrives addressed to raw while its canonical chat is
// something else, whatever was stored under raw is merged over.
func (c *Client) noteLIDChat(raw, canonical types.JID) {
	raw = raw.ToNonAD()
	if raw.Server != types.HiddenUserServer || raw == canonical {
		return
	}
	key := raw.String()
	c.mergeMu.Lock()
	if c.mergedLIDs == nil {
		c.mergedLIDs = map[string]struct{}{}
	}
	_, seen := c.mergedLIDs[key]
	c.mergedLIDs[key] = struct{}{}
	c.mergeMu.Unlock()
	if seen {
		return
	}
	if c.mergeLIDChat(key, canonical.String()) {
		c.emit(ChatEvent{})
	}
}
