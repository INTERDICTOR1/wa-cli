package chatstore

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/codebyoketch/wa-cli/internal/localdb"
	"github.com/codebyoketch/wa-cli/internal/msgstore"
)

// TestMain also serves as the entry point of the helper processes the
// two-process test starts (see process_test.go).
func TestMain(m *testing.M) {
	if os.Getenv(helperEnv) != "" {
		os.Exit(runHelper())
	}
	code := m.Run()
	localdb.CloseAll()
	os.Exit(code)
}

// newStore returns a Store on a fresh temp dir, closed again when the
// test ends (Windows can't remove the dir while the database is open).
func newStore(t *testing.T) *Store {
	t.Helper()
	s := New(t.TempDir())
	t.Cleanup(func() { s.Close() })
	return s
}

// mustGet returns a chat that has to exist.
func mustGet(t *testing.T, s *Store, jid string) Chat {
	t.Helper()
	c, ok, err := s.Get(jid)
	if err != nil || !ok {
		t.Fatalf("Get(%q): ok=%v err=%v", jid, ok, err)
	}
	return c
}

// jids joins the JIDs of chats (without the server part), for compact
// order assertions.
func jids(chats []Chat) string {
	var names []string
	for _, c := range chats {
		name, _, _ := strings.Cut(c.JID, "@")
		names = append(names, name)
	}
	return strings.Join(names, " ")
}

const jid = "a@s.whatsapp.net"

// ---- Upsert ----

func TestUpsertThenGet(t *testing.T) {
	s := newStore(t)
	err := s.Upsert(Chat{JID: "a@s.whatsapp.net", Name: "Alice", LastMessageAt: 100, LastMessagePreview: "hi"})
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	got, ok, err := s.Get("a@s.whatsapp.net")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !ok {
		t.Fatal("expected chat to be found")
	}
	if got.Name != "Alice" || got.LastMessagePreview != "hi" {
		t.Fatalf("unexpected chat: %+v", got)
	}
}

func TestGet_UnknownChat(t *testing.T) {
	s := newStore(t)
	if _, ok, err := s.Get("nobody@s.whatsapp.net"); ok || err != nil {
		t.Fatalf("ok=%v err=%v; want false, nil", ok, err)
	}
}

func TestUpsert_NewChatIsStoredAsGiven(t *testing.T) {
	s := newStore(t)
	want := Chat{
		JID: "g@g.us", Name: "Group", IsGroup: true, LastMessageAt: 100, LastMessagePreview: "hi", UnreadCount: 2,
		Muted: true, Pinned: true, Archived: true, MutedUntil: -1, MarkedUnread: true, Draft: "half a thought", Ephemeral: 86400,
	}
	if err := s.Upsert(want); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if got := mustGet(t, s, "g@g.us"); got != want {
		t.Fatalf("stored %+v, want %+v", got, want)
	}
}

func TestUpsert_PartialUpdateDoesNotClobber(t *testing.T) {
	s := newStore(t)

	if err := s.Upsert(Chat{JID: jid, Name: "Alice", LastMessageAt: 100, LastMessagePreview: "hi"}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	// Partial update: bump unread count only, leave Name/LastMessageAt zero.
	if err := s.Upsert(Chat{JID: jid, UnreadCount: 3}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	got, _, err := s.Get(jid)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Name != "Alice" {
		t.Fatalf("expected Name preserved, got %q", got.Name)
	}
	if got.LastMessageAt != 100 {
		t.Fatalf("expected LastMessageAt preserved, got %d", got.LastMessageAt)
	}
	if got.LastMessagePreview != "hi" {
		t.Fatalf("expected LastMessagePreview preserved, got %q", got.LastMessagePreview)
	}
	if got.UnreadCount != 3 {
		t.Fatalf("expected UnreadCount updated, got %d", got.UnreadCount)
	}
}

func TestUpsert_NewNameReplacesOld(t *testing.T) {
	s := newStore(t)
	_ = s.Upsert(Chat{JID: jid, Name: "Alice"})
	_ = s.Upsert(Chat{JID: jid, Name: "Alice W."})
	if got := mustGet(t, s, jid).Name; got != "Alice W." {
		t.Fatalf("Name = %q, want \"Alice W.\"", got)
	}
}

func TestUpsert_LastMessageAtNeverMovesBackwards(t *testing.T) {
	s := newStore(t)
	_ = s.Upsert(Chat{JID: jid, LastMessageAt: 200, LastMessagePreview: "new"})
	_ = s.Upsert(Chat{JID: jid, LastMessageAt: 100, LastMessagePreview: "old"})
	got, _, _ := s.Get(jid)
	if got.LastMessageAt != 200 || got.LastMessagePreview != "new" {
		t.Fatalf("older update moved chat back: %d %q", got.LastMessageAt, got.LastMessagePreview)
	}
	// The same time again doesn't replace the preview either.
	_ = s.Upsert(Chat{JID: jid, LastMessageAt: 200, LastMessagePreview: "same time"})
	if got, _, _ := s.Get(jid); got.LastMessagePreview != "new" {
		t.Fatalf("equal timestamp replaced the preview: %q", got.LastMessagePreview)
	}
	_ = s.Upsert(Chat{JID: jid, LastMessageAt: 300, LastMessagePreview: "newest"})
	if got, _, _ := s.Get(jid); got.LastMessageAt != 300 || got.LastMessagePreview != "newest" {
		t.Fatalf("newer update ignored: %d %q", got.LastMessageAt, got.LastMessagePreview)
	}
}

func TestUpsert_IsGroupIsSticky(t *testing.T) {
	s := newStore(t)
	_ = s.Upsert(Chat{JID: "g@g.us", IsGroup: true})
	_ = s.Upsert(Chat{JID: "g@g.us", Name: "Group"}) // IsGroup left false
	if !mustGet(t, s, "g@g.us").IsGroup {
		t.Fatal("IsGroup was reset by an update that didn't mention it")
	}

	_ = s.Upsert(Chat{JID: jid})
	_ = s.Upsert(Chat{JID: jid, IsGroup: true})
	if !mustGet(t, s, jid).IsGroup {
		t.Fatal("IsGroup could not be turned on by a later update")
	}
}

func TestUpsert_PreservesUnreadCount(t *testing.T) {
	s := newStore(t)

	if err := s.IncrementUnread(jid); err != nil {
		t.Fatalf("IncrementUnread: %v", err)
	}
	if err := s.IncrementUnread(jid); err != nil {
		t.Fatalf("IncrementUnread: %v", err)
	}

	// Simulate ingestMessage's metadata Upsert, which shouldn't stomp the
	// unread count that IncrementUnread just set.
	if err := s.Upsert(Chat{JID: jid, Name: "Alice", LastMessageAt: 100, LastMessagePreview: "hi"}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	got, _, err := s.Get(jid)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.UnreadCount != 2 {
		t.Fatalf("expected UnreadCount preserved at 2, got %d", got.UnreadCount)
	}
}

func TestUpsert_PreservesMuted(t *testing.T) {
	s := newStore(t)

	if err := s.SetMuted(jid, true); err != nil {
		t.Fatalf("SetMuted: %v", err)
	}

	// Simulate ordinary message ingestion, which never sets Muted in its
	// Chat literal (so it's always the zero value, false) — this must
	// NOT silently unmute the chat.
	if err := s.Upsert(Chat{JID: jid, Name: "Alice", LastMessageAt: 100}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	got, _, err := s.Get(jid)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !got.Muted {
		t.Fatal("expected Muted to remain true after an unrelated Upsert")
	}
}

func TestUpsert_NeverTouchesSetterOnlyFields(t *testing.T) {
	s := newStore(t)
	_ = s.Upsert(Chat{JID: jid, Name: "Alice"})
	_ = s.SetMuted(jid, true)
	_ = s.SetPinned(jid, true)
	_ = s.SetArchived(jid, true)
	_ = s.SetMutedUntil(jid, 123456)
	_ = s.SetMarkedUnread(jid, true)
	_ = s.SetDraft(jid, "unsent")
	_ = s.SetEphemeral(jid, 604800)

	// Neither leaving them zero...
	_ = s.Upsert(Chat{JID: jid, LastMessageAt: 100, LastMessagePreview: "hi"})
	// ...nor passing other values changes them.
	_ = s.Upsert(Chat{JID: jid, MutedUntil: -1, Draft: "other", Ephemeral: 5})

	want := Chat{
		JID: jid, Name: "Alice", LastMessageAt: 100, LastMessagePreview: "hi",
		Muted: true, Pinned: true, Archived: true, MutedUntil: 123456, MarkedUnread: true, Draft: "unsent", Ephemeral: 604800,
	}
	if got := mustGet(t, s, jid); got != want {
		t.Fatalf("got  %+v\nwant %+v", got, want)
	}
}

// ---- List and Search ----

func TestList_SortedByRecency(t *testing.T) {
	s := newStore(t)
	_ = s.Upsert(Chat{JID: "old@s.whatsapp.net", Name: "Old", LastMessageAt: 100})
	_ = s.Upsert(Chat{JID: "new@s.whatsapp.net", Name: "New", LastMessageAt: 200})
	_ = s.Upsert(Chat{JID: "none@s.whatsapp.net", Name: "NoActivity"})

	chats, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(chats) != 3 {
		t.Fatalf("expected 3 chats, got %d", len(chats))
	}
	if chats[0].JID != "new@s.whatsapp.net" || chats[1].JID != "old@s.whatsapp.net" {
		t.Fatalf("unexpected order: %+v", chats)
	}
	if chats[2].JID != "none@s.whatsapp.net" {
		t.Fatalf("expected no-activity chat last, got %+v", chats[2])
	}
}

func TestList_TiesAreSortedByNameIgnoringCase(t *testing.T) {
	s := newStore(t)
	_ = s.Upsert(Chat{JID: "1@s.whatsapp.net", Name: "bob"})
	_ = s.Upsert(Chat{JID: "2@s.whatsapp.net", Name: "Émile"})
	_ = s.Upsert(Chat{JID: "3@s.whatsapp.net", Name: "Alice"})
	_ = s.Upsert(Chat{JID: "4@s.whatsapp.net", Name: "Carol"})
	_ = s.Upsert(Chat{JID: "5@s.whatsapp.net", Name: "émile"})
	_ = s.Upsert(Chat{JID: "6@s.whatsapp.net", Name: "Recent", LastMessageAt: 5})

	chats, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	// Recent first; then by lower-cased name (the two Émiles are equal
	// there, so their JIDs decide).
	if got := jids(chats); got != "6 3 1 4 2 5" {
		t.Fatalf("order = %q, want \"6 3 1 4 2 5\"", got)
	}
}

func TestList_Empty(t *testing.T) {
	s := newStore(t)
	chats, err := s.List()
	if err != nil || len(chats) != 0 {
		t.Fatalf("List of an empty store = %+v, %v", chats, err)
	}
}

func TestSearch_MatchesNameCaseInsensitive(t *testing.T) {
	s := newStore(t)
	_ = s.Upsert(Chat{JID: "a@s.whatsapp.net", Name: "Alice Wanjiru"})
	_ = s.Upsert(Chat{JID: "b@s.whatsapp.net", Name: "Bob Otieno"})

	results, err := s.Search("wanjiru")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 || results[0].Name != "Alice Wanjiru" {
		t.Fatalf("unexpected results: %+v", results)
	}
}

func TestSearch_MatchesJID(t *testing.T) {
	s := newStore(t)
	_ = s.Upsert(Chat{JID: "254700111222@s.whatsapp.net", Name: ""})

	results, err := s.Search("254700111222")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
}

func TestSearch_TrimsQueryIgnoresCaseAndSortsLikeList(t *testing.T) {
	s := newStore(t)
	_ = s.Upsert(Chat{JID: "1@s.whatsapp.net", Name: "Family GROUP", LastMessageAt: 100})
	_ = s.Upsert(Chat{JID: "2@s.whatsapp.net", Name: "work group", LastMessageAt: 300})
	_ = s.Upsert(Chat{JID: "3@s.whatsapp.net", Name: "Someone else", LastMessageAt: 200})
	_ = s.Upsert(Chat{JID: "4@s.whatsapp.net", Name: "CAFÉ friends"})
	_ = s.Upsert(Chat{JID: "5@s.whatsapp.net", Name: "100% real_name"})

	search := func(query string) string {
		t.Helper()
		results, err := s.Search(query)
		if err != nil {
			t.Fatalf("Search(%q): %v", query, err)
		}
		return jids(results)
	}

	if got := search("  Group "); got != "2 1" {
		t.Errorf("Search(\"  Group \") = %q, want \"2 1\" (most recent first)", got)
	}
	if got := search("café"); got != "4" {
		t.Errorf("non-ASCII case: %q, want \"4\"", got)
	}
	// % and _ are ordinary characters, not wildcards.
	if got := search("100%"); got != "5" {
		t.Errorf("literal %%: %q, want \"5\"", got)
	}
	if got := search("r_al"); got != "" {
		t.Errorf("_ acted as a wildcard: %q", got)
	}
	if got := search("no such chat"); got != "" {
		t.Errorf("expected no results, got %q", got)
	}
	// An empty query matches every chat, as it always did.
	if got := search(""); got != "2 3 1 5 4" {
		t.Errorf("Search(\"\") = %q, want all five in List order", got)
	}
}

// ---- unread, mute, read ----

func TestIncrementUnread(t *testing.T) {
	s := newStore(t)

	for i := 0; i < 3; i++ {
		if err := s.IncrementUnread(jid); err != nil {
			t.Fatalf("IncrementUnread: %v", err)
		}
	}

	got, _, err := s.Get(jid)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.UnreadCount != 3 {
		t.Fatalf("expected UnreadCount 3, got %d", got.UnreadCount)
	}
	// It created a bare record: nothing else is set.
	if want := (Chat{JID: jid, UnreadCount: 3}); got != want {
		t.Fatalf("bare record = %+v, want %+v", got, want)
	}
}

func TestSetMuted(t *testing.T) {
	s := newStore(t)

	if err := s.SetMuted(jid, true); err != nil {
		t.Fatalf("SetMuted: %v", err)
	}
	got, _, err := s.Get(jid)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !got.Muted {
		t.Fatal("expected Muted true")
	}

	if err := s.SetMuted(jid, false); err != nil {
		t.Fatalf("SetMuted: %v", err)
	}
	got, _, _ = s.Get(jid)
	if got.Muted {
		t.Fatal("expected Muted false after unmute")
	}
}

func TestSetMuted_KeepsTheRestOfAnExistingChat(t *testing.T) {
	s := newStore(t)
	_ = s.Upsert(Chat{JID: jid, Name: "Alice", UnreadCount: 4, LastMessageAt: 100})
	_ = s.SetMuted(jid, true)

	want := Chat{JID: jid, Name: "Alice", UnreadCount: 4, LastMessageAt: 100, Muted: true}
	if got := mustGet(t, s, jid); got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestMarkRead(t *testing.T) {
	s := newStore(t)
	_ = s.Upsert(Chat{JID: jid, Name: "Alice", UnreadCount: 5})
	_ = s.SetMarkedUnread(jid, true)

	if err := s.MarkRead(jid); err != nil {
		t.Fatalf("MarkRead: %v", err)
	}
	got, _, _ := s.Get(jid)
	if got.UnreadCount != 0 {
		t.Fatalf("expected unread cleared, got %d", got.UnreadCount)
	}
	if got.MarkedUnread {
		t.Fatal("expected the marked-unread flag cleared")
	}
	if got.Name != "Alice" {
		t.Fatalf("MarkRead changed the name to %q", got.Name)
	}
}

func TestMarkRead_UnknownChatIsNoop(t *testing.T) {
	s := newStore(t)
	if err := s.MarkRead("nobody@s.whatsapp.net"); err != nil {
		t.Fatalf("MarkRead: %v", err)
	}
	if _, ok, _ := s.Get("nobody@s.whatsapp.net"); ok {
		t.Fatal("MarkRead created a record")
	}
}

// ---- setters ----

func TestSetters(t *testing.T) {
	s := newStore(t)
	_ = s.Upsert(Chat{JID: jid, Name: "Alice", LastMessageAt: 500, LastMessagePreview: "latest", UnreadCount: 9})

	steps := []struct {
		name string
		do   func() error
		want func(c *Chat) // the one change expected
	}{
		{"SetName", func() error { return s.SetName(jid, "Alice W.") }, func(c *Chat) { c.Name = "Alice W." }},
		{"SetPinned", func() error { return s.SetPinned(jid, true) }, func(c *Chat) { c.Pinned = true }},
		{"SetArchived", func() error { return s.SetArchived(jid, true) }, func(c *Chat) { c.Archived = true }},
		{"SetMutedUntil", func() error { return s.SetMutedUntil(jid, -1) }, func(c *Chat) { c.MutedUntil = -1 }},
		{"SetMarkedUnread", func() error { return s.SetMarkedUnread(jid, true) }, func(c *Chat) { c.MarkedUnread = true }},
		{"SetDraft", func() error { return s.SetDraft(jid, "to be continued") }, func(c *Chat) { c.Draft = "to be continued" }},
		{"SetEphemeral", func() error { return s.SetEphemeral(jid, 7776000) }, func(c *Chat) { c.Ephemeral = 7776000 }},
		{"SetUnread", func() error { return s.SetUnread(jid, 2) }, func(c *Chat) { c.UnreadCount = 2 }},
		// Unlike Upsert, this one can go back in time.
		{"SetLastMessage", func() error { return s.SetLastMessage(jid, 400, "the one before") },
			func(c *Chat) { c.LastMessageAt, c.LastMessagePreview = 400, "the one before" }},
		// And they all switch things off again.
		{"SetName empty", func() error { return s.SetName(jid, "") }, func(c *Chat) { c.Name = "" }},
		{"SetPinned off", func() error { return s.SetPinned(jid, false) }, func(c *Chat) { c.Pinned = false }},
		{"SetArchived off", func() error { return s.SetArchived(jid, false) }, func(c *Chat) { c.Archived = false }},
		{"SetMutedUntil off", func() error { return s.SetMutedUntil(jid, 0) }, func(c *Chat) { c.MutedUntil = 0 }},
		{"SetMarkedUnread off", func() error { return s.SetMarkedUnread(jid, false) }, func(c *Chat) { c.MarkedUnread = false }},
		{"SetDraft empty", func() error { return s.SetDraft(jid, "") }, func(c *Chat) { c.Draft = "" }},
		{"SetEphemeral off", func() error { return s.SetEphemeral(jid, 0) }, func(c *Chat) { c.Ephemeral = 0 }},
		{"SetUnread zero", func() error { return s.SetUnread(jid, 0) }, func(c *Chat) { c.UnreadCount = 0 }},
		{"SetUnread negative", func() error { return s.SetUnread(jid, -3) }, func(c *Chat) { c.UnreadCount = 0 }},
	}

	want := mustGet(t, s, jid)
	for _, step := range steps {
		if err := step.do(); err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}
		step.want(&want)
		// Each setter changes exactly its own field.
		if got := mustGet(t, s, jid); got != want {
			t.Fatalf("after %s:\n got %+v\nwant %+v", step.name, got, want)
		}
	}
}

func TestSetters_UnknownChatIsNoop(t *testing.T) {
	s := newStore(t)
	unknown := "nobody@s.whatsapp.net"

	calls := map[string]func() error{
		"SetName":         func() error { return s.SetName(unknown, "x") },
		"SetPinned":       func() error { return s.SetPinned(unknown, true) },
		"SetArchived":     func() error { return s.SetArchived(unknown, true) },
		"SetMutedUntil":   func() error { return s.SetMutedUntil(unknown, -1) },
		"SetMarkedUnread": func() error { return s.SetMarkedUnread(unknown, true) },
		"SetDraft":        func() error { return s.SetDraft(unknown, "x") },
		"SetEphemeral":    func() error { return s.SetEphemeral(unknown, 60) },
		"SetUnread":       func() error { return s.SetUnread(unknown, 3) },
		"SetLastMessage":  func() error { return s.SetLastMessage(unknown, 1, "x") },
		"Delete":          func() error { return s.Delete(unknown) },
	}
	for name, call := range calls {
		if err := call(); err != nil {
			t.Errorf("%s on an unknown chat: %v", name, err)
		}
	}
	if chats, _ := s.List(); len(chats) != 0 {
		t.Fatalf("a setter created a record: %+v", chats)
	}
}

// ---- Merge and Delete ----

func TestMerge_IntoUnknownChatMovesTheRecord(t *testing.T) {
	s := newStore(t)
	from := Chat{
		JID: "123@lid", Name: "Alice", LastMessageAt: 100, LastMessagePreview: "hi", UnreadCount: 2,
		Muted: true, Pinned: true, MutedUntil: 999, Draft: "draft", Ephemeral: 60,
	}
	_ = s.Upsert(from)

	if err := s.Merge("123@lid", "254700111222@s.whatsapp.net"); err != nil {
		t.Fatalf("Merge: %v", err)
	}

	if _, ok, _ := s.Get("123@lid"); ok {
		t.Fatal("the old record is still there")
	}
	want := from
	want.JID = "254700111222@s.whatsapp.net"
	if got := mustGet(t, s, "254700111222@s.whatsapp.net"); got != want {
		t.Fatalf("got  %+v\nwant %+v", got, want)
	}
}

func TestMerge_FoldsTwoChatsTogether(t *testing.T) {
	const fromJID, toJID = "123@lid", "254700111222@s.whatsapp.net"
	cases := []struct {
		name           string
		from, to, want Chat
	}{
		{
			name: "to keeps what it has; flags and counts combine",
			from: Chat{Name: "From Name", LastMessageAt: 100, LastMessagePreview: "older", UnreadCount: 2, Pinned: true, MarkedUnread: true, Draft: "from draft", Ephemeral: 60, MutedUntil: 500},
			to:   Chat{Name: "To Name", LastMessageAt: 200, LastMessagePreview: "newer", UnreadCount: 3, Muted: true, Archived: true, Draft: "to draft", Ephemeral: 3600, MutedUntil: 900},
			want: Chat{Name: "To Name", LastMessageAt: 200, LastMessagePreview: "newer", UnreadCount: 5, Muted: true, Pinned: true, Archived: true, MarkedUnread: true, Draft: "to draft", Ephemeral: 3600, MutedUntil: 900},
		},
		{
			name: "to takes over what it lacks, and the newer last message",
			from: Chat{Name: "From Name", IsGroup: true, LastMessageAt: 300, LastMessagePreview: "newest", Muted: true, Archived: true, Draft: "from draft", Ephemeral: 60, MutedUntil: 900},
			to:   Chat{LastMessageAt: 200, LastMessagePreview: "older", UnreadCount: 1, MutedUntil: 500},
			want: Chat{Name: "From Name", IsGroup: true, LastMessageAt: 300, LastMessagePreview: "newest", UnreadCount: 1, Muted: true, Archived: true, Draft: "from draft", Ephemeral: 60, MutedUntil: 900},
		},
		{
			name: "equal last-message times keep to's preview",
			from: Chat{LastMessageAt: 200, LastMessagePreview: "from's"},
			to:   Chat{LastMessageAt: 200, LastMessagePreview: "to's"},
			want: Chat{LastMessageAt: 200, LastMessagePreview: "to's"},
		},
		{
			name: "muted always (-1) on from wins over a time",
			from: Chat{MutedUntil: -1},
			to:   Chat{MutedUntil: 99999},
			want: Chat{MutedUntil: -1},
		},
		{
			name: "muted always (-1) on to wins over a time",
			from: Chat{MutedUntil: 99999},
			to:   Chat{MutedUntil: -1},
			want: Chat{MutedUntil: -1},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newStore(t)
			tc.from.JID, tc.to.JID, tc.want.JID = fromJID, toJID, toJID
			_ = s.Upsert(tc.from)
			_ = s.Upsert(tc.to)
			_ = s.Upsert(Chat{JID: "bystander@s.whatsapp.net", Name: "Bystander"})

			if err := s.Merge(fromJID, toJID); err != nil {
				t.Fatalf("Merge: %v", err)
			}

			if got := mustGet(t, s, toJID); got != tc.want {
				t.Fatalf("got  %+v\nwant %+v", got, tc.want)
			}
			if _, ok, _ := s.Get(fromJID); ok {
				t.Fatal("the merged-away record is still there")
			}
			if got := mustGet(t, s, "bystander@s.whatsapp.net"); got.Name != "Bystander" {
				t.Fatalf("an unrelated chat changed: %+v", got)
			}
		})
	}
}

func TestMerge_UnknownSourceOrSameJIDIsNoop(t *testing.T) {
	s := newStore(t)
	original := Chat{JID: jid, Name: "Alice", UnreadCount: 2}
	_ = s.Upsert(original)

	if err := s.Merge("nobody@s.whatsapp.net", jid); err != nil {
		t.Fatalf("Merge from an unknown chat: %v", err)
	}
	if err := s.Merge(jid, jid); err != nil {
		t.Fatalf("Merge onto itself: %v", err)
	}
	if err := s.Merge("nobody@s.whatsapp.net", "nobody-else@s.whatsapp.net"); err != nil {
		t.Fatalf("Merge between unknown chats: %v", err)
	}

	if got := mustGet(t, s, jid); got != original {
		t.Fatalf("chat changed: %+v", got)
	}
	if chats, _ := s.List(); len(chats) != 1 {
		t.Fatalf("expected 1 chat, got %d", len(chats))
	}
}

func TestDelete(t *testing.T) {
	s := newStore(t)
	_ = s.Upsert(Chat{JID: jid, Name: "Alice"})
	_ = s.Upsert(Chat{JID: "b@s.whatsapp.net", Name: "Bob"})

	if err := s.Delete(jid); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok, _ := s.Get(jid); ok {
		t.Fatal("chat still there after Delete")
	}
	if chats, _ := s.List(); len(chats) != 1 || chats[0].Name != "Bob" {
		t.Fatalf("unexpected chats left: %+v", chats)
	}
}

// ---- persistence and sharing ----

func TestPersistsAcrossInstances(t *testing.T) {
	dir := t.TempDir()
	t.Cleanup(func() { localdb.Close(dir) })

	s1 := New(dir)
	_ = s1.Upsert(Chat{JID: jid, Name: "Alice"})

	// A second Store on the same dir sees the first one's writes at once.
	s2 := New(dir)
	got, ok, err := s2.Get(jid)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !ok || got.Name != "Alice" {
		t.Fatalf("expected state to persist, got ok=%v chat=%+v", ok, got)
	}

	// ...and in the other direction.
	_ = s2.IncrementUnread(jid)
	if got := mustGet(t, s1, jid); got.UnreadCount != 1 {
		t.Fatalf("first store sees UnreadCount %d, want 1", got.UnreadCount)
	}
}

func TestClose_DataSurvivesAndStoreReopens(t *testing.T) {
	s := newStore(t)
	_ = s.Upsert(Chat{JID: jid, Name: "Alice"})

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Using the store again opens the database again, from disk.
	if got := mustGet(t, s, jid); got.Name != "Alice" {
		t.Fatalf("after Close: %+v", got)
	}
}

func TestSharesOneDatabaseWithTheMessageStore(t *testing.T) {
	dir := t.TempDir()
	t.Cleanup(func() { localdb.Close(dir) })
	chats, msgs := New(dir), msgstore.New(dir)

	if err := chats.Upsert(Chat{JID: jid, Name: "Alice"}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if err := msgs.Append(msgstore.Message{ID: "1", ChatJID: jid, Text: "hi"}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	// One file holds both, and no JSON files are written any more.
	entries, _ := os.ReadDir(dir)
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), localdb.FileName) {
			t.Errorf("unexpected file in the data dir: %s", entry.Name())
		}
	}

	// Closing through one store closes the handle both share; each simply
	// opens it again when next used.
	if err := msgs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := mustGet(t, chats, jid); got.Name != "Alice" {
		t.Fatalf("chat after the message store closed the database: %+v", got)
	}
	if list, err := msgs.List(jid); err != nil || len(list) != 1 {
		t.Fatalf("messages after Close: %+v, %v", list, err)
	}
}

func TestNew_OpenErrorSurfacesOnFirstCall(t *testing.T) {
	// A data dir that can't be created: its parent is a file.
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	s := New(filepath.Join(file, "data"))

	if _, err := s.List(); err == nil {
		t.Fatal("expected an error from the first call")
	}
	if err := s.Upsert(Chat{JID: jid}); err == nil {
		t.Fatal("expected an error from Upsert too")
	}
}

// ---- concurrency ----

func TestConcurrentUse(t *testing.T) {
	s := newStore(t)
	const workers, perWorker = 8, 100

	var wg sync.WaitGroup
	errs := make(chan error, 2*workers)
	for w := 0; w < workers; w++ {
		// Writers: everyone bumps the same chat's unread count and keeps
		// upserting it, plus a chat of their own...
		wg.Add(1)
		go func() {
			defer wg.Done()
			own := fmt.Sprintf("worker%d@s.whatsapp.net", w)
			for i := 0; i < perWorker; i++ {
				err := s.IncrementUnread(jid)
				if err == nil {
					err = s.Upsert(Chat{JID: jid, Name: "Shared", LastMessageAt: int64(i), LastMessagePreview: fmt.Sprint(i)})
				}
				if err == nil {
					err = s.Upsert(Chat{JID: own, Name: own, LastMessageAt: int64(i)})
				}
				if err != nil {
					errs <- err
					return
				}
			}
		}()
		// ...while readers list and search.
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				if _, err := s.List(); err != nil {
					errs <- err
					return
				}
				if _, err := s.Search("worker"); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent use: %v", err)
	}

	shared := mustGet(t, s, jid)
	// No increment was lost, and the last-message time ended at its peak.
	if shared.UnreadCount != workers*perWorker {
		t.Fatalf("UnreadCount = %d, want %d", shared.UnreadCount, workers*perWorker)
	}
	if shared.LastMessageAt != perWorker-1 || shared.LastMessagePreview != fmt.Sprint(perWorker-1) {
		t.Fatalf("last message = %d %q, want %d", shared.LastMessageAt, shared.LastMessagePreview, perWorker-1)
	}
	if chats, _ := s.List(); len(chats) != workers+1 {
		t.Fatalf("%d chats, want %d", len(chats), workers+1)
	}
}

// ---- legacy import ----

// oldChatsJSON is a chats.json as the JSON store wrote it: JID -> chat.
const oldChatsJSON = `{
  "111@s.whatsapp.net": {
    "jid": "111@s.whatsapp.net",
    "name": "Alice",
    "isGroup": false,
    "lastMessageAt": 2000,
    "lastMessagePreview": "see you",
    "unreadCount": 3,
    "muted": true
  },
  "222@g.us": {
    "jid": "222@g.us",
    "name": "Family",
    "isGroup": true,
    "lastMessageAt": 3000,
    "lastMessagePreview": "[image]",
    "unreadCount": 0
  },
  "333@s.whatsapp.net": {
    "jid": "333@s.whatsapp.net",
    "name": "",
    "isGroup": false,
    "lastMessageAt": 0,
    "lastMessagePreview": "",
    "unreadCount": 0
  }
}`

// legacyDir returns a temp data dir holding a chats.json with the given
// content; the database is closed again when the test ends.
func legacyDir(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, legacyFile), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { localdb.Close(dir) })
	return dir
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// meta reads a value from the meta table of the database in dir.
func meta(t *testing.T, dir, key string) string {
	t.Helper()
	db, err := localdb.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	value, err := localdb.Meta(db, key)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func TestLegacyImport_ImportsAndRenames(t *testing.T) {
	dir := legacyDir(t, oldChatsJSON)
	s := New(dir)

	// The first call of any kind triggers the import.
	chats, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	want := []Chat{
		{JID: "222@g.us", Name: "Family", IsGroup: true, LastMessageAt: 3000, LastMessagePreview: "[image]"},
		{JID: "111@s.whatsapp.net", Name: "Alice", LastMessageAt: 2000, LastMessagePreview: "see you", UnreadCount: 3, Muted: true},
		{JID: "333@s.whatsapp.net"},
	}
	if !reflect.DeepEqual(chats, want) {
		t.Fatalf("imported\n got %+v\nwant %+v", chats, want)
	}

	if got := meta(t, dir, "legacy_chats_imported"); got != "1" {
		t.Errorf("legacy_chats_imported = %q, want \"1\"", got)
	}
	if got := meta(t, dir, "legacy_chats_error"); got != "" {
		t.Errorf("legacy_chats_error = %q, want unset", got)
	}
	// The file is renamed, never deleted or rewritten.
	old := filepath.Join(dir, legacyFile)
	if fileExists(old) {
		t.Error("chats.json is still there")
	}
	kept, err := os.ReadFile(old + ".migrated")
	if err != nil || string(kept) != oldChatsJSON {
		t.Errorf("chats.json.migrated missing or changed (err=%v)", err)
	}
}

func TestLegacyImport_HappensOnce(t *testing.T) {
	dir := legacyDir(t, oldChatsJSON)
	first := New(dir)
	_ = first.MarkRead("111@s.whatsapp.net")
	_ = first.Delete("333@s.whatsapp.net")

	// An old-style file showing up again later is not imported: it must
	// not bring back the unread count or the deleted chat.
	if err := os.WriteFile(filepath.Join(dir, legacyFile), []byte(oldChatsJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	second := New(dir)
	if got := mustGet(t, second, "111@s.whatsapp.net"); got.UnreadCount != 0 {
		t.Fatalf("UnreadCount = %d: the file was imported a second time", got.UnreadCount)
	}
	if _, ok, _ := second.Get("333@s.whatsapp.net"); ok {
		t.Fatal("a deleted chat came back: the file was imported a second time")
	}
}

func TestLegacyImport_NoFile(t *testing.T) {
	s := newStore(t)
	_ = s.Upsert(Chat{JID: jid})
	if got := meta(t, s.dir, "legacy_chats_imported"); got != "" {
		t.Fatalf("legacy_chats_imported = %q without a file, want unset", got)
	}
}

func TestLegacyImport_NothingToImport(t *testing.T) {
	for name, content := range map[string]string{"empty object": `{}`, "null": `null`, "empty file": ``} {
		t.Run(name, func(t *testing.T) {
			dir := legacyDir(t, content)
			s := New(dir)

			if chats, err := s.List(); err != nil || len(chats) != 0 {
				t.Fatalf("List = %+v, %v; want none, nil", chats, err)
			}
			if got := meta(t, dir, "legacy_chats_imported"); got != "1" {
				t.Errorf("legacy_chats_imported = %q, want \"1\"", got)
			}
			if !fileExists(filepath.Join(dir, legacyFile+".migrated")) {
				t.Error("expected the file to be renamed")
			}
		})
	}
}

func TestLegacyImport_DamagedFileIsLeftAloneAndStoreStillWorks(t *testing.T) {
	damaged := map[string]string{
		"cut off":     oldChatsJSON[:len(oldChatsJSON)/2],
		"not json":    `this is not JSON`,
		"wrong shape": `["a", "list"]`,
	}
	for name, content := range damaged {
		t.Run(name, func(t *testing.T) {
			dir := legacyDir(t, content)
			s := New(dir)

			// Normal operations work and report no error.
			if err := s.Upsert(Chat{JID: jid, Name: "Alice"}); err != nil {
				t.Fatalf("Upsert: %v", err)
			}
			chats, err := s.List()
			if err != nil || len(chats) != 1 {
				t.Fatalf("List = %+v, %v; want just the new chat", chats, err)
			}

			if got := meta(t, dir, "legacy_chats_imported"); got != "" {
				t.Errorf("legacy_chats_imported = %q, want unset", got)
			}
			if got := meta(t, dir, "legacy_chats_error"); !strings.Contains(got, legacyFile) {
				t.Errorf("legacy_chats_error = %q, want it to name the file", got)
			}
			kept, err := os.ReadFile(filepath.Join(dir, legacyFile))
			if err != nil || string(kept) != content {
				t.Errorf("the damaged file was moved or changed (err=%v)", err)
			}
			if fileExists(filepath.Join(dir, legacyFile+".migrated")) {
				t.Error("a damaged file must not be renamed")
			}
		})
	}
}
