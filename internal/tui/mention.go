package tui

import (
	"context"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"go.mau.fi/whatsmeow/types"

	"github.com/codebyoketch/wa-cli/internal/whatsapp"
)

// maxSuggestions is how many @mention matches the dropdown shows.
const maxSuggestions = 5

// member is someone who can be @mentioned in the open group.
type member struct {
	jid   string   // e.g. "177403959709918@lid"
	name  string   // resolved display name
	words []string // lower-cased words of name, for matching
}

// mention is a member picked from the dropdown into the input text.
type mention struct{ name, jid string }

type membersLoadedMsg struct {
	groupJID string
	members  []member
}

// loadMembersCmd fetches the group's members (one network request) and
// resolves their names. Only the open group's members are ever held.
func loadMembersCmd(ctx context.Context, client *whatsapp.Client, groupJID string) tea.Cmd {
	return func() tea.Msg {
		jid, err := types.ParseJID(groupJID)
		if err != nil {
			return membersLoadedMsg{groupJID: groupJID}
		}
		_, parts, err := client.GroupInfo(ctx, jid)
		if err != nil {
			return errMsg{err}
		}
		me, myLID := client.WA.Store.GetJID().User, client.WA.Store.GetLID().User
		members := make([]member, 0, len(parts))
		for _, p := range parts {
			user, _, _ := strings.Cut(p.JID, "@")
			if user == me || user == myLID {
				continue
			}
			name := client.ResolveName(ctx, p.JID)
			members = append(members, member{jid: p.JID, name: name, words: strings.Fields(strings.ToLower(name))})
		}
		sort.Slice(members, func(i, j int) bool { return members[i].name < members[j].name })
		return membersLoadedMsg{groupJID: groupJID, members: members}
	}
}

// mentionQuery returns the text typed after an "@" that starts a word,
// up to the cursor (pos, in runes): "@shashwat tri" -> "shashwat tri".
// ok is false when the cursor isn't in a mention.
func mentionQuery(value string, pos int) (query string, start int, ok bool) {
	before := []rune(value)[:pos]
	for i := len(before) - 1; i >= 0; i-- {
		if before[i] != '@' {
			continue
		}
		if i > 0 && before[i-1] != ' ' {
			return "", 0, false // e.g. an email address
		}
		q := string(before[i+1:])
		if len(strings.Fields(q)) > 3 || strings.HasPrefix(q, " ") {
			return "", 0, false
		}
		return q, i, true
	}
	return "", 0, false
}

// matchMembers returns members whose name matches query: each query word
// must be the start of a different name word, in order. "shashwat tri"
// matches "Shashwat Tripathi" but not "Shashwat Pranav".
func matchMembers(members []member, query string) []member {
	q := strings.Fields(strings.ToLower(query))
	var out []member
	for _, m := range members {
		if wordsMatch(m.words, q) {
			out = append(out, m)
			if len(out) == maxSuggestions {
				break
			}
		}
	}
	return out
}

func wordsMatch(name, query []string) bool {
	i := 0
	for _, q := range query {
		for i < len(name) && !strings.HasPrefix(name[i], q) {
			i++
		}
		if i == len(name) {
			return false
		}
		i++
	}
	return true
}

// applyMentions rewrites each picked "@Name" still in text to the
// "@<user>" form WhatsApp needs, returning the JIDs to list as mentioned.
func applyMentions(text string, picked []mention) (string, []string) {
	var jids []string
	for _, m := range picked {
		if !strings.Contains(text, "@"+m.name) {
			continue // deleted from the text before sending
		}
		user, _, _ := strings.Cut(m.jid, "@")
		text = strings.ReplaceAll(text, "@"+m.name, "@"+user)
		jids = append(jids, m.jid)
	}
	return text, jids
}
