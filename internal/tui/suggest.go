package tui

import (
	"context"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// suggestions is the dropdown above the message box. It offers group
// members after "@" and emoji after ":" (as in ":smile"), from the text
// just before the cursor.
type suggestions struct {
	kind  sugKind
	items []sugItem
	idx   int
	start int // rune index of the "@" or ":" being completed

	dismissed string // message text when Esc closed the dropdown

	// Members of one group at a time: loaded on the first "@" typed in a
	// group and replaced when another group needs them.
	members    []member
	membersFor string
	loading    bool

	picked []mention // mentions already inserted into the text
}

type sugKind int

const (
	sugNone sugKind = iota
	sugMention
	sugEmoji
)

type sugItem struct {
	label  string // shown in the dropdown
	insert string // replaces the typed query
	jid    string // mentions: who it is
}

type membersLoadedMsg struct {
	groupJID string
	members  []member
	err      error
}

func loadMembersCmd(ctx context.Context, be Backend, groupJID string) tea.Cmd {
	return func() tea.Msg {
		list, err := be.Members(ctx, groupJID)
		if err != nil {
			return membersLoadedMsg{groupJID: groupJID, err: err}
		}
		members := make([]member, 0, len(list))
		for _, p := range list {
			name := oneLine(p.Name)
			members = append(members, member{jid: p.JID, name: name, words: strings.Fields(strings.ToLower(name))})
		}
		sort.SliceStable(members, func(i, j int) bool { return members[i].name < members[j].name })
		return membersLoadedMsg{groupJID: groupJID, members: members}
	}
}

// reset forgets everything tied to the current chat's message.
func (s *suggestions) reset() {
	s.kind, s.items, s.idx = sugNone, nil, 0
	s.dismissed, s.picked = "", nil
}

// active reports whether the dropdown is taking the arrow/Enter keys.
func (s *suggestions) active() bool { return len(s.items) > 0 }

// update refreshes the dropdown for the text before the cursor.
func (s *suggestions) update(m *model) tea.Cmd {
	s.kind, s.items = sugNone, nil
	value, pos := m.ed.Value(), m.ed.Cursor()
	if value == "" || value == s.dismissed {
		return nil
	}

	if q, start, ok := mentionQuery(value, pos); ok && strings.HasSuffix(m.openJID, "@g.us") {
		rest := string([]rune(value)[start:])
		for _, p := range s.picked {
			if strings.HasPrefix(rest, "@"+p.name) {
				return nil // the cursor is just after a mention already picked
			}
		}
		s.kind, s.start = sugMention, start
		if s.membersFor != m.openJID {
			s.membersFor, s.members, s.loading = m.openJID, nil, true
			return loadMembersCmd(m.ctx, m.be, m.openJID)
		}
		for _, mem := range matchMembers(s.members, q) {
			s.items = append(s.items, sugItem{label: mem.name, insert: "@" + mem.name + " ", jid: mem.jid})
		}
		s.idx = min(s.idx, max(0, len(s.items)-1))
		return nil
	}

	if q, start, ok := emojiQuery(value, pos); ok {
		s.kind, s.start = sugEmoji, start
		for _, e := range matchEmoji(q) {
			s.items = append(s.items, sugItem{label: e.char + "  :" + e.name + ":", insert: e.char})
		}
		s.idx = min(s.idx, max(0, len(s.items)-1))
	}
	return nil
}

func (s *suggestions) membersLoaded(m *model, msg membersLoadedMsg) tea.Cmd {
	if msg.groupJID != s.membersFor {
		return nil
	}
	s.loading = false
	if msg.err != nil {
		s.membersFor = "" // try again on the next "@"
		return m.fail(msg.err)
	}
	s.members = msg.members
	return s.update(m)
}

// key handles a key while the dropdown is open. handled is false for keys
// the message box should get as usual.
func (s *suggestions) key(m *model, msg tea.KeyMsg) (cmd tea.Cmd, handled bool) {
	switch msg.Type {
	case tea.KeyUp:
		s.idx = max(0, s.idx-1)
	case tea.KeyDown:
		s.idx = min(len(s.items)-1, s.idx+1)
	case tea.KeyTab, tea.KeyEnter:
		if msg.Alt {
			return nil, false
		}
		pick := s.items[s.idx]
		m.ed.Replace(s.start, m.ed.Cursor(), pick.insert)
		if s.kind == sugMention {
			s.picked = append(s.picked, mention{name: strings.TrimSuffix(strings.TrimPrefix(pick.insert, "@"), " "), jid: pick.jid})
		}
		s.kind, s.items, s.idx = sugNone, nil, 0
		return m.typed(), true
	case tea.KeyEsc:
		s.kind, s.items = sugNone, nil
		s.dismissed = m.ed.Value()
	default:
		return nil, false
	}
	return nil, true
}

// view draws the dropdown, or nothing when it's closed.
func (s *suggestions) view(w int) []string {
	if s.kind == sugMention && s.loading {
		return []string{"   " + dimStyle.Render("loading group members…")}
	}
	lines := make([]string, 0, len(s.items))
	for i, it := range s.items {
		label := truncate(oneLine(it.label), w-6)
		if i == s.idx {
			lines = append(lines, " "+selectedStyle.Render(" › "+label+" "))
		} else {
			lines = append(lines, "    "+chatNameStyle.Render(label))
		}
	}
	return lines
}

// emojiQuery returns the name typed after a ":" that starts a word, up to
// the cursor: ":smi" -> "smi". At least two letters, so "10:30" and ":)"
// don't open the dropdown.
func emojiQuery(value string, pos int) (query string, start int, ok bool) {
	runes := []rune(value)
	before := runes[:max(0, min(pos, len(runes)))]
	i := len(before) - 1
	for i >= 0 && isEmojiNameRune(before[i]) {
		i--
	}
	if i < 0 || before[i] != ':' || len(before)-i-1 < 2 {
		return "", 0, false
	}
	if i > 0 && before[i-1] != ' ' && before[i-1] != '\n' {
		return "", 0, false
	}
	return strings.ToLower(string(before[i+1:])), i, true
}

func isEmojiNameRune(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '+' || r == '-'
}

// matchEmoji returns emoji whose name starts with q, then ones that
// merely contain it.
func matchEmoji(q string) []emoji {
	var out, rest []emoji
	for _, e := range emojiTable {
		switch {
		case strings.HasPrefix(e.name, q):
			out = append(out, e)
		case strings.Contains(e.name, q):
			rest = append(rest, e)
		}
		if len(out) == maxSuggestions {
			return out
		}
	}
	return append(out, rest...)[:min(maxSuggestions, len(out)+len(rest))]
}
