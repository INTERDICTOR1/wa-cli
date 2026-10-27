package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/codebyoketch/wa-cli/internal/chatstore"
)

func mk(jid, name string) member {
	return member{jid: jid, name: name, words: strings.Fields(strings.ToLower(name))}
}

var group = []member{
	mk("1@lid", "Pratham Agarwal"),
	mk("2@lid", "Shashwat Pranav"),
	mk("3@lid", "Shashwat Tripathi"),
	mk("4@lid", "Tec IITB"),
}

func names(ms []member) string {
	var n []string
	for _, m := range ms {
		n = append(n, m.name)
	}
	return strings.Join(n, ", ")
}

func TestMatchMembers(t *testing.T) {
	cases := map[string]string{
		"":             "Pratham Agarwal, Shashwat Pranav, Shashwat Tripathi, Tec IITB",
		"shashwat":     "Shashwat Pranav, Shashwat Tripathi",
		"shashwat tri": "Shashwat Tripathi",
		"shashwat pr":  "Shashwat Pranav",
		"Shash Tr":     "Shashwat Tripathi",
		"tri":          "Shashwat Tripathi",
		"tri shashwat": "", // words must be in order
		"xyz":          "",
	}
	for q, want := range cases {
		if got := names(matchMembers(group, q)); got != want {
			t.Errorf("matchMembers(%q) = %q, want %q", q, got, want)
		}
	}
}

func TestMentionQuery(t *testing.T) {
	cases := []struct {
		value string
		q     string
		ok    bool
	}{
		{"@shashwat tri", "shashwat tri", true},
		{"hey @sha", "sha", true},
		{"@", "", true},
		{"mail me a@b.com", "", false},
		{"no mention", "", false},
		{"@a b c d e", "", false}, // too many words: not a mention anymore
	}
	for _, c := range cases {
		q, _, ok := mentionQuery(c.value, len([]rune(c.value)))
		if q != c.q || ok != c.ok {
			t.Errorf("mentionQuery(%q) = (%q, %v), want (%q, %v)", c.value, q, ok, c.q, c.ok)
		}
	}
}

func TestMentionDropdownFlow(t *testing.T) {
	var m tea.Model = newModel(nil, nil, nil, nil, nil)
	m, _ = m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m, _ = m.Update(chatsLoadedMsg{chats: []chatstore.Chat{{JID: "g@g.us", Name: "GradPipe Core", IsGroup: true}}})
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})

	// First "@" in the group starts loading its members.
	m, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'@'}})
	if got := m.(model); got.membersFor != "g@g.us" || !got.membersLoading || cmd == nil {
		t.Fatalf("typing @ should load members: for=%q loading=%v", got.membersFor, got.membersLoading)
	}
	if !strings.Contains(m.View(), "loading group members") {
		t.Fatal("no loading line while members load")
	}
	m, _ = m.Update(membersLoadedMsg{groupJID: "g@g.us", members: group})

	m = typeKeys(m, "shashwat")
	view := m.View()
	if !strings.Contains(view, "Shashwat Pranav") || !strings.Contains(view, "Shashwat Tripathi") {
		t.Fatalf("dropdown should list both Shashwats")
	}
	if n := strings.Count(view, "\n") + 1; n > 30 {
		t.Fatalf("dropdown grew the frame to %d lines", n)
	}

	m = typeKeys(m, " tri")
	if got := m.(model).suggestions; len(got) != 1 || got[0].name != "Shashwat Tripathi" {
		t.Fatalf("suggestions = %v", names(got))
	}
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab})
	got := m.(model)
	if got.input.Value() != "@Shashwat Tripathi " || len(got.mentions) != 1 || len(got.suggestions) != 0 {
		t.Fatalf("after Tab: value=%q mentions=%d open=%d", got.input.Value(), len(got.mentions), len(got.suggestions))
	}

	// Typing on after the mention must not reopen the dropdown.
	m = typeKeys(m, "kal")
	if got := m.(model); got.input.Value() != "@Shashwat Tripathi kal" || len(got.suggestions) != 0 {
		t.Fatalf("value=%q suggestions=%d", got.input.Value(), len(got.suggestions))
	}
}

func TestApplyMentions(t *testing.T) {
	text, jids := applyMentions("hi @Shashwat Tripathi and @Tec IITB",
		[]mention{{"Shashwat Tripathi", "3@lid"}, {"Tec IITB", "4@lid"}, {"Pratham Agarwal", "1@lid"}})
	if text != "hi @3 and @4" || strings.Join(jids, ",") != "3@lid,4@lid" {
		t.Fatalf("got %q %v", text, jids)
	}
}
