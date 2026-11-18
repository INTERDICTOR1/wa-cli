package whatsapp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"

	waProto "go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"

	"github.com/codebyoketch/wa-cli/internal/chatstore"
	waerrors "github.com/codebyoketch/wa-cli/internal/errors"
	"github.com/codebyoketch/wa-cli/internal/msgstore"
)

// errNoStore is returned by the reading methods of a Client built without
// the local stores.
var errNoStore = waerrors.New("this client has no local chat/message store")

// --- chats ---

// Chats returns every chat (archived ones included, flagged), pinned
// first, then most recent first.
func (c *Client) Chats(ctx context.Context) ([]ChatView, error) {
	if c.chats == nil {
		return nil, errNoStore
	}
	list, err := c.chats.List()
	if err != nil {
		return nil, err
	}
	views := make([]ChatView, 0, len(list))
	for _, ch := range list {
		if isPseudoChat(ch.JID) {
			continue
		}
		views = append(views, c.chatView(ctx, ch))
	}
	sort.SliceStable(views, func(i, j int) bool {
		a, b := views[i], views[j]
		if a.Pinned != b.Pinned {
			return a.Pinned
		}
		if a.LastMessageAt != b.LastMessageAt {
			return a.LastMessageAt > b.LastMessageAt
		}
		return strings.ToLower(a.Title) < strings.ToLower(b.Title)
	})
	return views, nil
}

// Chat returns one chat's list entry.
func (c *Client) Chat(ctx context.Context, jid string) (ChatView, bool, error) {
	if c.chats == nil {
		return ChatView{}, false, errNoStore
	}
	ch, ok, err := c.chats.Get(jid)
	if err == nil && !ok {
		if canonical := c.canonicalChatString(jid); canonical != jid {
			ch, ok, err = c.chats.Get(canonical)
		}
	}
	if err != nil || !ok {
		return ChatView{}, false, err
	}
	return c.chatView(ctx, ch), true, nil
}

// chatView adds the title to a stored chat: the group's subject, or for
// a person the saved contact name, push name, business name or phone
// number, in that order.
func (c *Client) chatView(ctx context.Context, ch chatstore.Chat) ChatView {
	v := ChatView{Chat: ch, Title: strings.TrimSpace(ch.Name)}
	jid, err := types.ParseJID(ch.JID)
	if err != nil {
		if v.Title == "" {
			v.Title = ch.JID
		}
		return v
	}
	switch jid.Server {
	case types.DefaultUserServer, types.HiddenUserServer:
		// The chat with yourself reads "You".
		name, known := c.resolveName(ctx, ch.JID)
		if known || v.Title == "" {
			v.Title = name
		}
	default:
		if v.Title == "" {
			v.Title = jid.User
		}
	}
	if v.Title == "" {
		v.Title = ch.JID
	}
	return v
}

// Contacts returns the address book, sorted by name: everyone saved in
// your phone's contacts, one entry per person under their phone-number
// JID. If the contact list hasn't synced, everyone with any known name.
func (c *Client) Contacts(ctx context.Context) ([]Contact, error) {
	if c.WA == nil || c.WA.Store == nil || c.WA.Store.Contacts == nil {
		return nil, waerrors.New("contacts are not available")
	}
	all, err := c.WA.Store.Contacts.GetAllContacts(ctx)
	if err != nil {
		return nil, waerrors.Wrap(err, "loading contacts")
	}
	build := func(savedOnly bool) []Contact {
		byJID := map[string]Contact{}
		for jid, info := range all {
			if jid.Server != types.DefaultUserServer && jid.Server != types.HiddenUserServer {
				continue
			}
			saved := info.FullName != "" || info.FirstName != ""
			name := pickName(info)
			if name == "" || (savedOnly && !saved) || c.isMe(jid) {
				continue
			}
			key := c.canonicalChat(jid).String()
			// Prefer the entry stored under the phone number itself.
			if _, dup := byJID[key]; dup && jid.Server == types.HiddenUserServer {
				continue
			}
			byJID[key] = Contact{JID: key, Name: name, PushName: info.PushName, BusinessName: info.BusinessName}
		}
		out := make([]Contact, 0, len(byJID))
		for _, ct := range byJID {
			out = append(out, ct)
		}
		return out
	}
	out := build(true)
	if len(out) == 0 {
		out = build(false)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := strings.ToLower(out[i].Name), strings.ToLower(out[j].Name)
		if a != b {
			return a < b
		}
		return out[i].JID < out[j].JID
	})
	return out, nil
}

// --- messages ---

// Messages returns up to limit messages of a chat, oldest first: the
// newest ones when before is nil, otherwise the ones just older than
// before. Rows that aren't a message of their own are left out.
func (c *Client) Messages(ctx context.Context, chatJID string, before *msgstore.Message, limit int) ([]MessageView, error) {
	if c.msgs == nil {
		return nil, errNoStore
	}
	if limit <= 0 {
		limit = 50
	}
	chatJID = c.canonicalChatString(chatJID)

	var views []MessageView
	cursor := before
	// Hidden rows (leftovers that couldn't be folded) may thin a page out;
	// read on a little so a page is never empty while older messages exist.
	for page := 0; page < 4 && len(views) < limit; page++ {
		var rows []msgstore.Message
		var err error
		if cursor == nil {
			rows, err = c.msgs.Latest(chatJID, limit)
		} else {
			rows, err = c.msgs.Before(chatJID, cursor.Timestamp, cursor.Seq, limit)
		}
		if err != nil {
			return nil, err
		}
		if len(rows) == 0 {
			break
		}
		visible := make([]MessageView, 0, len(rows))
		for _, m := range rows {
			if v, ok := c.view(ctx, m); ok {
				visible = append(visible, v)
			}
		}
		views = append(visible, views...)
		if len(rows) < limit {
			break
		}
		oldest := rows[0]
		cursor = &oldest
	}
	if len(views) > limit {
		views = views[len(views)-limit:]
	}
	return views, nil
}

// Message returns one message, or false if it isn't stored (any more).
func (c *Client) Message(ctx context.Context, chatJID, id string) (MessageView, bool, error) {
	if c.msgs == nil {
		return MessageView{}, false, errNoStore
	}
	m, ok, err := c.msgs.Get(c.canonicalChatString(chatJID), id)
	if err != nil || !ok {
		return MessageView{}, false, err
	}
	v, ok := c.view(ctx, m)
	return v, ok, nil
}

// Search finds messages containing query (case-insensitive), newest
// first; chatJID "" searches every chat.
func (c *Client) Search(ctx context.Context, chatJID, query string, limit int) ([]MessageView, error) {
	if c.msgs == nil {
		return nil, errNoStore
	}
	if strings.TrimSpace(query) == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 50
	}
	if chatJID != "" {
		chatJID = c.canonicalChatString(chatJID)
	}
	rows, err := c.msgs.Search(chatJID, query, limit)
	if err != nil {
		return nil, err
	}
	views := make([]MessageView, 0, len(rows))
	for _, m := range rows {
		if v, ok := c.view(ctx, m); ok {
			views = append(views, v)
		}
	}
	return views, nil
}

// view builds what the TUI draws for a stored message. ok is false for a
// row that isn't a message of its own (plumbing, or an event that could
// not be folded into its target).
func (c *Client) view(ctx context.Context, m msgstore.Message) (MessageView, bool) {
	v := MessageView{Message: m}
	resolve := func(jid string) string { return c.ResolveName(ctx, jid) }
	if !m.FromMe {
		v.Sender = resolve(m.SenderJID)
	}
	if m.Deleted {
		v.Kind = KindDeleted
		v.Body = "This message was deleted"
		if m.FromMe {
			v.Body = "You deleted this message"
		}
		return v, true
	}
	v.Reactions = c.reactionViews(ctx, m.Reactions)

	msg := contentOf(m)
	if msg == nil {
		// Only the text was recorded (e.g. sent by an old version).
		v.Kind, v.Body = kindOfMediaType(m.MediaType), m.Text
		if v.Kind != KindText {
			v.Media = &MediaInfo{}
		}
		return v, true
	}
	a := analyze(msg)
	if a.Class != classRow {
		return v, false
	}
	v.Kind = a.Kind
	v.Media = a.Media
	v.Forwarded = a.Ctx.GetIsForwarded()
	v.Mentioned = c.mentionsMe(a.Ctx.GetMentionedJID())
	v.Quote = c.quoteView(ctx, m, a.Ctx)
	if a.Kind == KindPoll {
		// The question is part of the poll, not a caption beside it.
		v.Poll = c.pollView(a.Poll, m.PollVotes)
		return v, true
	}
	v.Body = resolveMentions(a.Text, a.Ctx.GetMentionedJID(), resolve)
	return v, true
}

// kindOfMediaType maps a stored media type to a view kind, for rows
// without content.
func kindOfMediaType(mediaType string) string {
	switch mediaType {
	case "image":
		return KindImage
	case "video":
		return KindVideo
	case "audio":
		return KindAudio
	case "voice note":
		return KindVoice
	case "document":
		return KindDocument
	case "sticker":
		return KindSticker
	}
	return KindText
}

// mentionsMe reports whether one of the mentioned JIDs is you.
func (c *Client) mentionsMe(mentioned []string) bool {
	for _, jid := range mentioned {
		if parsed, err := types.ParseJID(jid); err == nil && c.isMe(parsed) {
			return true
		}
	}
	return false
}

// quoteView builds the replied-to snippet from a reply's context info.
func (c *Client) quoteView(ctx context.Context, m msgstore.Message, ci *waProto.ContextInfo) *Quote {
	id := ci.GetStanzaID()
	if id == "" {
		return nil
	}
	q := &Quote{ID: id}
	resolve := func(jid string) string { return c.ResolveName(ctx, jid) }

	if a := analyze(unwrapMessage(ci.GetQuotedMessage())); a.Class == classRow {
		a.Text = resolveMentions(a.Text, a.Ctx.GetMentionedJID(), resolve)
		q.Text = a.quoteText()
	}
	participant := ci.GetParticipant()
	if participant != "" {
		if jid, err := types.ParseJID(participant); err == nil && c.isMe(jid) {
			q.Sender = "You"
		} else {
			q.Sender = resolve(participant)
		}
	}
	if (q.Text == "" || q.Sender == "") && c.msgs != nil {
		// The reply didn't carry the quote: use the stored original.
		if orig, ok, err := c.msgs.Get(m.ChatJID, id); err == nil && ok {
			if q.Sender == "" {
				q.Sender = "You"
				if !orig.FromMe {
					q.Sender = resolve(orig.SenderJID)
				}
			}
			if q.Text == "" {
				switch a := analyze(contentOf(orig)); {
				case orig.Deleted:
					q.Text = "This message was deleted"
				case a.Class == classRow:
					a.Text = resolveMentions(a.Text, a.Ctx.GetMentionedJID(), resolve)
					q.Text = a.quoteText()
				default:
					q.Text = oneLine(withMediaTag(orig.MediaType, orig.Text), 120)
				}
			}
		}
	}
	return q
}

// latestPerPerson reduces entries to one per person (the newest), since
// the same person can be recorded under both of their addresses. keys
// and stamps give each entry's sender and time; the result holds the
// indexes kept, oldest first.
func (c *Client) latestPerPerson(n int, sender func(int) string, stamp func(int) int64) []int {
	newest := map[string]int{}
	for i := 0; i < n; i++ {
		who := c.canonicalUser(sender(i))
		if j, ok := newest[who]; !ok || stamp(i) >= stamp(j) {
			newest[who] = i
		}
	}
	kept := make([]int, 0, len(newest))
	for _, i := range newest {
		kept = append(kept, i)
	}
	sort.Slice(kept, func(a, b int) bool {
		if stamp(kept[a]) != stamp(kept[b]) {
			return stamp(kept[a]) < stamp(kept[b])
		}
		return kept[a] < kept[b]
	})
	return kept
}

// reactionViews aggregates a message's reactions per emoji, most used
// first.
func (c *Client) reactionViews(ctx context.Context, reactions []msgstore.Reaction) []ReactionView {
	if len(reactions) == 0 {
		return nil
	}
	kept := c.latestPerPerson(len(reactions),
		func(i int) string { return reactions[i].SenderJID },
		func(i int) int64 { return reactions[i].Timestamp })

	var views []ReactionView
	index := map[string]int{}
	for _, i := range kept {
		r := reactions[i]
		if r.Emoji == "" {
			continue
		}
		at, ok := index[r.Emoji]
		if !ok {
			at = len(views)
			index[r.Emoji] = at
			views = append(views, ReactionView{Emoji: r.Emoji})
		}
		views[at].Count++
		if c.canonicalUser(r.SenderJID) == senderMe {
			views[at].Mine = true
			views[at].Names = append(views[at].Names, "You")
		} else {
			views[at].Names = append(views[at].Names, c.ResolveName(ctx, r.SenderJID))
		}
	}
	sort.SliceStable(views, func(i, j int) bool { return views[i].Count > views[j].Count })
	return views
}

// pollView builds a poll with its tally. WhatsApp identifies an option
// in a vote by the SHA-256 of its name.
func (c *Client) pollView(poll *waProto.PollCreationMessage, votes []msgstore.PollVote) *PollView {
	pv := &PollView{
		Question: poll.GetName(),
		// 0 means "any number of options"; only exactly 1 is single-choice.
		MultiSelect: poll.GetSelectableOptionsCount() != 1,
	}
	byHash := map[string]int{}
	for i, opt := range poll.GetOptions() {
		name := opt.GetOptionName()
		sum := sha256.Sum256([]byte(name))
		byHash[hex.EncodeToString(sum[:])] = i
		pv.Options = append(pv.Options, PollOption{Name: name})
	}
	kept := c.latestPerPerson(len(votes),
		func(i int) string { return votes[i].VoterJID },
		func(i int) int64 { return votes[i].Timestamp })
	for _, i := range kept {
		vote := votes[i]
		mine := c.canonicalUser(vote.VoterJID) == senderMe
		counted := map[int]bool{}
		for _, hash := range vote.Options {
			at, ok := byHash[strings.ToLower(hash)]
			if !ok || counted[at] {
				continue
			}
			counted[at] = true
			pv.Options[at].Votes++
			if mine {
				pv.Options[at].Mine = true
			}
		}
	}
	return pv
}
