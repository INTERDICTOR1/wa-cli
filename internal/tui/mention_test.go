package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/codebyoketch/wa-cli/internal/whatsapp"
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
	be := newFake()
	be.chats = []whatsapp.ChatView{chat("g@g.us", "GradPipe Core", 1)}
	for _, mem := range group {
		be.members["g@g.us"] = append(be.members["g@g.us"], whatsapp.Member{JID: mem.jid, Name: mem.name})
	}
	h := newHarness(t, be, 100, 30)
	h.key(tea.KeyEnter)

	// The first "@" in the group loads its members.
	h.typeText("@")
	if h.m.sug.membersFor != "g@g.us" || len(h.m.sug.members) != len(group) {
		t.Fatalf("typing @ should load members: for=%q n=%d", h.m.sug.membersFor, len(h.m.sug.members))
	}

	h.typeText("shashwat")
	view := h.frame()
	if !strings.Contains(view, "Shashwat Pranav") || !strings.Contains(view, "Shashwat Tripathi") {
		t.Fatalf("dropdown should list both Shashwats")
	}

	h.typeText(" tri")
	if got := h.m.sug.items; len(got) != 1 || got[0].label != "Shashwat Tripathi" {
		t.Fatalf("suggestions = %+v", got)
	}
	h.key(tea.KeyTab)
	if v := h.m.ed.Value(); v != "@Shashwat Tripathi " || len(h.m.sug.picked) != 1 || h.m.sug.active() {
		t.Fatalf("after Tab: value=%q picked=%d open=%v", v, len(h.m.sug.picked), h.m.sug.active())
	}

	// Typing on after the mention must not reopen the dropdown.
	h.typeText("kal")
	if v := h.m.ed.Value(); v != "@Shashwat Tripathi kal" || h.m.sug.active() {
		t.Fatalf("value=%q open=%v", v, h.m.sug.active())
	}

	// Sending rewrites the name to the form WhatsApp needs.
	h.key(tea.KeyEnter)
	if len(be.sent) != 1 || be.sent[0].out.Text != "@3 kal" || strings.Join(be.sent[0].out.Mentions, ",") != "3@lid" {
		t.Fatalf("sent = %+v", be.sent)
	}
}

func TestApplyMentions(t *testing.T) {
	text, jids := applyMentions("hi @Shashwat Tripathi and @Tec IITB",
		[]mention{{"Shashwat Tripathi", "3@lid"}, {"Tec IITB", "4@lid"}, {"Pratham Agarwal", "1@lid"}})
	if text != "hi @3 and @4" || strings.Join(jids, ",") != "3@lid,4@lid" {
		t.Fatalf("got %q %v", text, jids)
	}
}
