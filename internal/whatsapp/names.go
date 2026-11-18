package whatsapp

import (
	"context"
	"strings"
	"time"

	waProto "go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/codebyoketch/wa-cli/internal/msgstore"
)

// nameCacheMax bounds the resolved-name cache. A personal account knows a
// few thousand people at most; when the cache fills up it is simply
// dropped and refilled.
const nameCacheMax = 4096

// ResolveName returns the best display name for a user JID string, using
// only local data (whatsmeow's contact and LID stores, no network):
// address-book full name, first name, push name, business name, then the
// formatted phone number, then the raw ID. An @lid JID is first mapped to
// its phone-number JID, since address-book names are stored under that.
// Not meant for group JIDs — those have no contact entry and fall
// through to the raw ID.
//
// Results are cached until a name changes, so resolving the senders of a
// page of messages costs one lookup per distinct sender.
func (c *Client) ResolveName(ctx context.Context, jidStr string) string {
	name, _ := c.resolveName(ctx, jidStr)
	return name
}

// knownName is ResolveName without the phone-number fallback: "" unless
// a real name (address book, push or business name) is known.
func (c *Client) knownName(ctx context.Context, jidStr string) string {
	name, known := c.resolveName(ctx, jidStr)
	if !known {
		return ""
	}
	return name
}

// nameEntry is one cached name lookup.
type nameEntry struct {
	name  string
	known bool // a real name, not just the phone number or raw ID
}

// resolveName is the cached name lookup behind ResolveName.
func (c *Client) resolveName(ctx context.Context, jidStr string) (name string, known bool) {
	if jidStr == senderMe {
		return "You", true
	}
	c.nameMu.Lock()
	entry, ok := c.nameCache[jidStr]
	gen := c.nameGen
	c.nameMu.Unlock()
	if ok {
		return entry.name, entry.known
	}

	name, known = c.lookupName(ctx, jidStr)

	c.nameMu.Lock()
	if gen == c.nameGen { // not invalidated while looking up
		if c.nameCache == nil || len(c.nameCache) >= nameCacheMax {
			c.nameCache = make(map[string]nameEntry, 256)
		}
		c.nameCache[jidStr] = nameEntry{name, known}
	}
	c.nameMu.Unlock()
	return name, known
}

// lookupName resolves jidStr without the cache. known is false when the
// result is only a formatted phone number or raw ID.
func (c *Client) lookupName(ctx context.Context, jidStr string) (name string, known bool) {
	jid, err := types.ParseJID(jidStr)
	if err != nil {
		return jidStr, false
	}
	jid = jid.ToNonAD()
	if c.isMe(jid) {
		return "You", true
	}

	pn := jid
	if jid.Server == types.HiddenUserServer {
		if mapped := c.pnForLID(jid); !mapped.IsEmpty() {
			pn = mapped
		}
	}

	if src := c.contacts(); src != nil {
		var infos []types.ContactInfo
		if pn != jid {
			if info, err := src.GetContact(ctx, pn); err == nil {
				infos = append(infos, info)
			}
		}
		if info, err := src.GetContact(ctx, jid); err == nil {
			infos = append(infos, info)
		}
		// A phone-number JID's names may also sit under its LID (push
		// names of people met in LID-addressed groups).
		if jid.Server == types.DefaultUserServer {
			if lid := c.lidForPN(jid); !lid.IsEmpty() {
				if info, err := src.GetContact(ctx, lid); err == nil {
					infos = append(infos, info)
				}
			}
		}
		if name := pickName(infos...); name != "" {
			return name, true
		}
	}
	if pn.Server == types.DefaultUserServer {
		return formatPhone(pn.User), false
	}
	if jid.User == "" {
		return jidStr, false
	}
	return jid.User, false
}

// invalidateNames drops every cached name and schedules a NamesEvent.
// Call it whenever a contact, push or business name may have changed.
func (c *Client) invalidateNames() {
	c.nameMu.Lock()
	c.nameCache = nil
	c.nameGen++
	c.nameMu.Unlock()
	c.scheduleNamesEvent()
}

// namesEventInterval is the shortest time between two NamesEvents: a
// contact sync changes thousands of names in a burst.
const namesEventInterval = time.Second

// scheduleNamesEvent emits a NamesEvent now, or, if one went out less
// than namesEventInterval ago, once that interval has passed.
func (c *Client) scheduleNamesEvent() {
	c.namesMu.Lock()
	if c.namesPending {
		c.namesMu.Unlock()
		return
	}
	wait := namesEventInterval - time.Since(c.namesLast)
	if wait <= 0 {
		c.namesLast = time.Now()
		c.namesMu.Unlock()
		c.emit(NamesEvent{})
		return
	}
	c.namesPending = true
	c.namesMu.Unlock()
	time.AfterFunc(wait, func() {
		c.namesMu.Lock()
		c.namesPending = false
		c.namesLast = time.Now()
		c.namesMu.Unlock()
		c.emit(NamesEvent{})
	})
}

// pickName returns the first non-empty name across infos, trying each
// field in priority order across all infos before moving to the next
// field (so a phone contact's push name doesn't beat a LID contact's
// address-book name).
func pickName(infos ...types.ContactInfo) string {
	fields := []func(types.ContactInfo) string{
		func(ci types.ContactInfo) string { return ci.FullName },
		func(ci types.ContactInfo) string { return ci.FirstName },
		func(ci types.ContactInfo) string { return ci.PushName },
		func(ci types.ContactInfo) string { return ci.BusinessName },
	}
	for _, field := range fields {
		for _, ci := range infos {
			if name := field(ci); name != "" && name != "-" {
				return name
			}
		}
	}
	return ""
}

// formatPhone renders a JID's digits as a phone number: Indian numbers as
// "+91 95419 88522", everything else as plain "+<digits>".
func formatPhone(digits string) string {
	if len(digits) == 12 && strings.HasPrefix(digits, "91") {
		return "+91 " + digits[2:7] + " " + digits[7:]
	}
	return "+" + digits
}

// DisplayText renders a stored message as one line for `wa chat open`:
// text and captions, [media] tags and @mentions as @name. hidden is true
// for rows that aren't a message of their own: protocol plumbing, and
// reactions, edits or deletes that haven't been folded into their target
// yet.
func (c *Client) DisplayText(ctx context.Context, m msgstore.Message) (text string, hidden bool) {
	return displayText(contentOf(m), m, func(jid string) string {
		return c.ResolveName(ctx, jid)
	})
}

func displayText(msg *waProto.Message, m msgstore.Message, resolve func(string) string) (string, bool) {
	if m.Deleted {
		return "[deleted]", false
	}
	if msg == nil {
		// No raw content — fall back to what msgstore recorded.
		return withMediaTag(m.MediaType, m.Text), false
	}
	a := analyze(msg)
	if a.Class != classRow {
		return "", true
	}
	body := resolveMentions(a.Text, a.Ctx.GetMentionedJID(), resolve)
	var text string
	switch a.Kind {
	case KindText, KindSystem, KindUnsupported, KindLocation, KindContact:
		text = body
	case KindPoll:
		text = withMediaTag("poll", body)
	default:
		text = withMediaTag(a.MediaType, body)
	}
	if m.Edited && text != "" {
		text += " (edited)"
	}
	return text, false
}

func withMediaTag(mediaType, text string) string {
	switch {
	case mediaType != "" && text != "":
		return "[" + mediaType + "] " + text
	case mediaType != "":
		return "[" + mediaType + "]"
	}
	return text
}

// resolveMentions replaces each "@<user part>" of a mentioned JID in text
// with "@<name>".
func resolveMentions(text string, mentioned []string, resolve func(string) string) string {
	if text == "" || resolve == nil {
		return text
	}
	for _, jid := range mentioned {
		if at := strings.Index(jid, "@"); at > 0 {
			text = strings.ReplaceAll(text, "@"+jid[:at], "@"+resolve(jid))
		}
	}
	return text
}

// isProtocolOnly reports whether msg is not a message of its own: pure
// plumbing, or an event (reaction, edit, delete, vote, ...) that belongs
// to another message.
func isProtocolOnly(msg *waProto.Message) bool {
	return analyze(unwrapMessage(msg)).Class != classRow
}

// firstFieldName returns the name of the first content field set on msg,
// ignoring plumbing fields, with the "Message" suffix trimmed (so
// locationMessage -> "location"). "" if there is no content field.
func firstFieldName(msg *waProto.Message) string {
	if msg == nil {
		return ""
	}
	var name string
	msg.ProtoReflect().Range(func(fd protoreflect.FieldDescriptor, _ protoreflect.Value) bool {
		switch fd.Name() {
		case "messageContextInfo", "senderKeyDistributionMessage", "fastRatchetKeySenderKeyDistributionMessage":
			return true
		}
		name = strings.TrimSuffix(string(fd.Name()), "Message")
		return false
	})
	return name
}
