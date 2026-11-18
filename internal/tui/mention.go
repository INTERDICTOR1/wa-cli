package tui

import (
	"strings"
)

// maxSuggestions is how many matches a dropdown shows.
const maxSuggestions = 6

// member is someone who can be @mentioned in the open group.
type member struct {
	jid   string   // e.g. "177403959709918@lid"
	name  string   // resolved display name
	words []string // lower-cased words of name, for matching
}

// mention is a member picked from the dropdown into the message text.
type mention struct{ name, jid string }

// mentionQuery returns the text typed after an "@" that starts a word,
// up to the cursor (pos, in runes): "@shashwat tri" -> "shashwat tri".
// ok is false when the cursor isn't in a mention.
func mentionQuery(value string, pos int) (query string, start int, ok bool) {
	runes := []rune(value)
	before := runes[:max(0, min(pos, len(runes)))]
	for i := len(before) - 1; i >= 0; i-- {
		if before[i] == '\n' {
			return "", 0, false // mentions don't span lines
		}
		if before[i] != '@' {
			continue
		}
		if i > 0 && before[i-1] != ' ' && before[i-1] != '\n' {
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
