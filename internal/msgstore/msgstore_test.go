package msgstore

import (
	"encoding/base64"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/codebyoketch/wa-cli/internal/localdb"
)

// TestMain also serves as the entry point of the helper processes the
// two-process tests start (see process_test.go).
func TestMain(m *testing.M) {
	if os.Getenv(helperEnv) != "" {
		os.Exit(runHelper())
	}
	code := m.Run()
	localdb.CloseAll()
	removeBenchDir()
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

// ids joins the IDs of msgs, for compact order assertions.
func ids(msgs []Message) string {
	var b strings.Builder
	for _, m := range msgs {
		b.WriteString(m.ID)
	}
	return b.String()
}

// mustList returns the chat's messages, failing the test on an error.
func mustList(t *testing.T, s *Store, chatJID string) []Message {
	t.Helper()
	msgs, err := s.List(chatJID)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	return msgs
}

// mustGet returns a message that has to exist.
func mustGet(t *testing.T, s *Store, chatJID, id string) Message {
	t.Helper()
	msg, ok, err := s.Get(chatJID, id)
	if err != nil || !ok {
		t.Fatalf("Get(%q, %q): ok=%v err=%v", chatJID, id, ok, err)
	}
	return msg
}

const jid = "a@s.whatsapp.net"

// ---- saving and plain reads ----

func TestAppendThenList(t *testing.T) {
	s := newStore(t)

	_ = s.Append(Message{ID: "1", ChatJID: jid, Text: "hello", Timestamp: 100})
	_ = s.Append(Message{ID: "2", ChatJID: jid, Text: "world", Timestamp: 200})

	msgs, err := s.List(jid)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(msgs))
	}
	if msgs[0].Text != "hello" || msgs[1].Text != "world" {
		t.Fatalf("unexpected order: %+v", msgs)
	}
	if msgs[0].Seq == 0 || msgs[1].Seq <= msgs[0].Seq {
		t.Fatalf("expected increasing Seq, got %d then %d", msgs[0].Seq, msgs[1].Seq)
	}
}

func TestList_OrdersByTimestampNotArrival(t *testing.T) {
	s := newStore(t)
	_ = s.Append(Message{ID: "c", ChatJID: jid, Timestamp: 300})
	_ = s.Append(Message{ID: "a", ChatJID: jid, Timestamp: 100})
	_ = s.Append(Message{ID: "b", ChatJID: jid, Timestamp: 200})
	// Same timestamp as b: arrival order decides.
	_ = s.Append(Message{ID: "B", ChatJID: jid, Timestamp: 200})

	if got := ids(mustList(t, s, jid)); got != "abBc" {
		t.Fatalf("got order %q, want \"abBc\"", got)
	}
}

func TestAppend_DuplicateIDKeepsStoredMessage(t *testing.T) {
	s := newStore(t)
	_ = s.Append(Message{ID: "1", ChatJID: jid, Text: "original", Timestamp: 100})

	if err := s.Append(Message{ID: "1", ChatJID: jid, Text: "again", Timestamp: 999}); err != nil {
		t.Fatalf("Append of a duplicate should be a no-op, got %v", err)
	}

	msgs := mustList(t, s, jid)
	if len(msgs) != 1 || msgs[0].Text != "original" || msgs[0].Timestamp != 100 {
		t.Fatalf("duplicate changed the store: %+v", msgs)
	}
}

func TestAppendMany_SortsAndSkipsDuplicates(t *testing.T) {
	s := newStore(t)
	_ = s.Append(Message{ID: "b", ChatJID: jid, Timestamp: 200})
	err := s.AppendMany([]Message{
		{ID: "c", ChatJID: jid, Timestamp: 300},
		{ID: "a", ChatJID: jid, Timestamp: 100},
		{ID: "b", ChatJID: jid, Timestamp: 200}, // already stored
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(mustList(t, s, jid)); got != "abc" {
		t.Fatalf("got order %q, want \"abc\"", got)
	}
}

func TestAppendMany_EmptyIsNoop(t *testing.T) {
	s := newStore(t)
	if err := s.AppendMany(nil); err != nil {
		t.Fatal(err)
	}
}

func TestInsert_ReportsWhetherNew(t *testing.T) {
	s := newStore(t)

	inserted, err := s.Insert(Message{ID: "1", ChatJID: jid, Timestamp: 100})
	if err != nil || !inserted {
		t.Fatalf("first Insert: inserted=%v err=%v; want true", inserted, err)
	}
	inserted, err = s.Insert(Message{ID: "1", ChatJID: jid, Timestamp: 100})
	if err != nil || inserted {
		t.Fatalf("second Insert: inserted=%v err=%v; want false", inserted, err)
	}
	// The same ID in another chat is a different message.
	inserted, err = s.Insert(Message{ID: "1", ChatJID: "b@s.whatsapp.net", Timestamp: 100})
	if err != nil || !inserted {
		t.Fatalf("Insert into another chat: inserted=%v err=%v; want true", inserted, err)
	}
}

func TestInsert_EmptyIDGetsAUniqueLocalOne(t *testing.T) {
	s := newStore(t)
	for i := 0; i < 3; i++ {
		inserted, err := s.Insert(Message{ChatJID: jid, Timestamp: int64(i)})
		if err != nil || !inserted {
			t.Fatalf("Insert without ID: inserted=%v err=%v", inserted, err)
		}
	}

	msgs := mustList(t, s, jid)
	if len(msgs) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(msgs))
	}
	seen := map[string]bool{}
	for _, m := range msgs {
		if !strings.HasPrefix(m.ID, "local-") || seen[m.ID] {
			t.Fatalf("expected distinct local- IDs, got %q", m.ID)
		}
		seen[m.ID] = true
	}
}

func TestInsertMany_ReturnsOnlyTheNewOnesWithSeq(t *testing.T) {
	s := newStore(t)
	_ = s.Append(Message{ID: "old", ChatJID: jid, Timestamp: 50})

	inserted, err := s.InsertMany([]Message{
		{ID: "x", ChatJID: jid, Timestamp: 300, Text: "x"},
		{ID: "old", ChatJID: jid, Timestamp: 50}, // already stored
		{ID: "y", ChatJID: jid, Timestamp: 100, Text: "y"},
		{ID: "x", ChatJID: jid, Timestamp: 300, Text: "x again"}, // twice in the batch
		{ChatJID: jid, Timestamp: 400},                           // no ID
	})
	if err != nil {
		t.Fatalf("InsertMany: %v", err)
	}
	if len(inserted) != 3 || inserted[0].ID != "x" || inserted[1].ID != "y" || !strings.HasPrefix(inserted[2].ID, "local-") {
		t.Fatalf("unexpected inserted set: %+v", inserted)
	}
	for _, m := range inserted {
		stored := mustGet(t, s, jid, m.ID)
		if m.Seq == 0 || stored.Seq != m.Seq {
			t.Fatalf("returned Seq %d for %q, stored Seq %d", m.Seq, m.ID, stored.Seq)
		}
	}
	if got := mustGet(t, s, jid, "x").Text; got != "x" {
		t.Fatalf("the first copy in a batch should win, got %q", got)
	}
	if n, _ := s.Count(jid); n != 4 {
		t.Fatalf("Count = %d, want 4", n)
	}
}

func TestPut_InsertsOrReplacesKeepingSeq(t *testing.T) {
	s := newStore(t)

	if err := s.Put(Message{ID: "1", ChatJID: jid, Text: "v1", Timestamp: 100, Status: StatusPending, FromMe: true}); err != nil {
		t.Fatalf("Put (new): %v", err)
	}
	first := mustGet(t, s, jid, "1")

	// Every field is replaced - including ones going back to their zero
	// value, which Append/Insert would never touch.
	if err := s.Put(Message{ID: "1", ChatJID: jid, Text: "v2", Timestamp: 200, Status: StatusSent}); err != nil {
		t.Fatalf("Put (replace): %v", err)
	}
	second := mustGet(t, s, jid, "1")

	if second.Text != "v2" || second.Timestamp != 200 || second.Status != StatusSent || second.FromMe {
		t.Fatalf("Put did not replace every field: %+v", second)
	}
	if second.Seq != first.Seq {
		t.Fatalf("Put changed Seq from %d to %d", first.Seq, second.Seq)
	}
	if n, _ := s.Count(jid); n != 1 {
		t.Fatalf("Count = %d, want 1", n)
	}
}

func TestRoundTrip_EveryField(t *testing.T) {
	s := newStore(t)
	raw := base64.StdEncoding.EncodeToString([]byte{0, 1, 2, 250, 251, 252, 253, 254, 255})
	want := Message{
		ID:        "full",
		ChatJID:   "g@g.us",
		SenderJID: "254700111222@s.whatsapp.net",
		Timestamp: 1700000000123,
		Text:      "caption with ünïcödé and 'quotes'",
		FromMe:    true,
		MediaType: "image",
		RawProto:  raw,
		Status:    StatusPlayed,
		Edited:    true,
		Deleted:   true,
		HD:        true,
		Reactions: []Reaction{{SenderJID: "me", Emoji: "👍", Timestamp: 5}, {SenderJID: "x@s.whatsapp.net", Emoji: "❤️", Timestamp: 6}},
		PollVotes: []PollVote{{VoterJID: "me", Options: []string{"ab12", "cd34"}, Timestamp: 7}},
	}
	if err := s.Append(want); err != nil {
		t.Fatalf("Append: %v", err)
	}

	got := mustGet(t, s, want.ChatJID, want.ID)
	want.Seq = got.Seq
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip changed the message:\n got %+v\nwant %+v", got, want)
	}

	// A message with nothing optional set comes back with nothing set.
	_ = s.Append(Message{ID: "bare", ChatJID: "g@g.us"})
	bare := mustGet(t, s, "g@g.us", "bare")
	if bare.RawProto != "" || bare.Reactions != nil || bare.PollVotes != nil || bare.Status != StatusNone {
		t.Fatalf("bare message gained fields: %+v", bare)
	}
}

func TestRawProto_StoredAsBytesAndBadBase64KeptAsIs(t *testing.T) {
	s := newStore(t)
	content := []byte("pretend this is a protobuf")
	_ = s.Append(Message{ID: "good", ChatJID: jid, RawProto: base64.StdEncoding.EncodeToString(content)})
	_ = s.Append(Message{ID: "bad", ChatJID: jid, RawProto: "not valid base64!!!"})

	// In the file: the decoded bytes, not their base64 text.
	db, err := localdb.Open(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	var kind string
	var size int
	if err := db.QueryRow(`SELECT typeof(raw_proto), length(raw_proto) FROM messages WHERE id = 'good'`).Scan(&kind, &size); err != nil {
		t.Fatal(err)
	}
	if kind != "blob" || size != len(content) {
		t.Fatalf("stored as %s of %d bytes, want blob of %d", kind, size, len(content))
	}

	// Whatever was given comes back unchanged.
	if got := mustGet(t, s, jid, "bad").RawProto; got != "not valid base64!!!" {
		t.Fatalf("invalid base64 came back as %q", got)
	}
	// That includes base64 a lenient decoder accepts but that would not
	// encode back to the same text: a line break, or stray bits in the
	// last character ("aGl=" and "aGk=" both decode to "hi").
	for i, sloppy := range []string{"aGk=\n", "aGVs\r\nbG8=", "aGl="} {
		id := fmt.Sprint("sloppy", i)
		_ = s.Append(Message{ID: id, ChatJID: jid, RawProto: sloppy})
		if got := mustGet(t, s, jid, id).RawProto; got != sloppy {
			t.Errorf("RawProto %q came back as %q", sloppy, got)
		}
	}
}

func TestList_UnknownChatReturnsEmpty(t *testing.T) {
	s := newStore(t)
	msgs, err := s.List("nonexistent@s.whatsapp.net")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(msgs) != 0 {
		t.Fatalf("expected empty, got %d messages", len(msgs))
	}
}

func TestGet_FindsByID(t *testing.T) {
	s := newStore(t)
	_ = s.Append(Message{ID: "abc123", ChatJID: jid, Text: "target"})
	_ = s.Append(Message{ID: "def456", ChatJID: jid, Text: "other"})

	msg, ok, err := s.Get(jid, "abc123")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !ok || msg.Text != "target" {
		t.Fatalf("expected to find target message, got ok=%v msg=%+v", ok, msg)
	}
}

func TestGet_NotFound(t *testing.T) {
	s := newStore(t)
	_, ok, err := s.Get(jid, "nonexistent")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if ok {
		t.Fatal("expected not found")
	}
}

func TestFind_AcrossChatsPrefersNewest(t *testing.T) {
	s := newStore(t)
	_ = s.Append(Message{ID: "dup", ChatJID: "a@s.whatsapp.net", Text: "older", Timestamp: 100})
	_ = s.Append(Message{ID: "dup", ChatJID: "b@s.whatsapp.net", Text: "newer", Timestamp: 200})
	_ = s.Append(Message{ID: "solo", ChatJID: "c@s.whatsapp.net", Text: "only", Timestamp: 50})

	msg, ok, err := s.Find("dup")
	if err != nil || !ok || msg.Text != "newer" || msg.ChatJID != "b@s.whatsapp.net" {
		t.Fatalf("Find(dup) = %+v, ok=%v, err=%v; want the newer one", msg, ok, err)
	}
	if msg, ok, _ := s.Find("solo"); !ok || msg.ChatJID != "c@s.whatsapp.net" {
		t.Fatalf("Find(solo) = %+v, ok=%v", msg, ok)
	}
	if _, ok, err := s.Find("missing"); ok || err != nil {
		t.Fatalf("Find(missing): ok=%v err=%v; want false, nil", ok, err)
	}
}

func TestChatsAreIsolated(t *testing.T) {
	s := newStore(t)
	_ = s.Append(Message{ID: "1", ChatJID: "a@s.whatsapp.net", Text: "a-msg"})
	_ = s.Append(Message{ID: "2", ChatJID: "b@s.whatsapp.net", Text: "b-msg"})

	aMsgs, _ := s.List("a@s.whatsapp.net")
	bMsgs, _ := s.List("b@s.whatsapp.net")

	if len(aMsgs) != 1 || len(bMsgs) != 1 {
		t.Fatalf("expected 1 message each, got a=%d b=%d", len(aMsgs), len(bMsgs))
	}
}

func TestPersistsAcrossInstances(t *testing.T) {
	dir := t.TempDir()
	t.Cleanup(func() { localdb.Close(dir) })

	s1 := New(dir)
	_ = s1.Append(Message{ID: "1", ChatJID: jid, Text: "hi"})

	// A second Store on the same dir sees the first one's writes at once.
	s2 := New(dir)
	msgs, err := s2.List(jid)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(msgs) != 1 || msgs[0].Text != "hi" {
		t.Fatalf("expected state to persist, got %+v", msgs)
	}

	// ...and in the other direction.
	_ = s2.Append(Message{ID: "2", ChatJID: jid, Text: "there", Timestamp: 1})
	if n, _ := s1.Count(jid); n != 2 {
		t.Fatalf("first store sees %d messages, want 2", n)
	}
}

func TestClose_DataSurvivesAndStoreReopens(t *testing.T) {
	s := newStore(t)
	_ = s.Append(Message{ID: "1", ChatJID: jid, Text: "hi"})

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Using the store again opens the database again, from disk.
	msgs, err := s.List(jid)
	if err != nil || len(msgs) != 1 || msgs[0].Text != "hi" {
		t.Fatalf("after Close: msgs=%+v err=%v", msgs, err)
	}
}

func TestNew_OpenErrorSurfacesOnFirstCall(t *testing.T) {
	// A data dir that can't be created: its parent is a file.
	file := t.TempDir() + "/file"
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	s := New(file + "/data")

	if _, err := s.List(jid); err == nil {
		t.Fatal("expected an error from the first call")
	}
	if err := s.Append(Message{ID: "1", ChatJID: jid}); err == nil {
		t.Fatal("expected an error from Append too")
	}
}

func TestNothingIsTrimmedAutomatically(t *testing.T) {
	s := newStore(t)
	old := time.Now().Add(-10 * RetainFor).UnixMilli()
	total := MaxPerChat + 100
	for i := 0; i < total; i++ {
		_ = s.Append(Message{ID: fmt.Sprint("m", i), ChatJID: jid, Timestamp: old + int64(i)})
	}
	if n, _ := s.Count(jid); n != total {
		t.Fatalf("Count = %d, want all %d kept", n, total)
	}
}

// ---- paging ----

// fillChat stores n messages in chatJID whose timestamps repeat (only
// n/7 distinct values) and arrive in random order, and returns them in
// the order List gives.
func fillChat(t testing.TB, s *Store, chatJID string, n int) []Message {
	t.Helper()
	rng := rand.New(rand.NewSource(1))
	batch := make([]Message, n)
	for i := range batch {
		batch[i] = Message{
			ID:        fmt.Sprintf("m%05d", i),
			ChatJID:   chatJID,
			Timestamp: 1_000_000 + int64(rng.Intn(n/7+1)),
			Text:      fmt.Sprint("message ", i),
			FromMe:    i%3 == 0,
		}
	}
	if err := s.AppendMany(batch); err != nil {
		t.Fatalf("AppendMany: %v", err)
	}
	all, err := s.List(chatJID)
	if err != nil || len(all) != n {
		t.Fatalf("List: %d messages, err=%v; want %d", len(all), err, n)
	}
	return all
}

func TestList_IsSortedByTimestampThenSeq(t *testing.T) {
	s := newStore(t)
	all := fillChat(t, s, jid, 2000)
	for i := 1; i < len(all); i++ {
		a, b := all[i-1], all[i]
		if a.Timestamp > b.Timestamp || (a.Timestamp == b.Timestamp && a.Seq >= b.Seq) {
			t.Fatalf("out of order at %d: (%d, %d) then (%d, %d)", i, a.Timestamp, a.Seq, b.Timestamp, b.Seq)
		}
	}
}

func TestLatestThenBefore_WalksTheWholeChatExactlyOnce(t *testing.T) {
	s := newStore(t)
	const total = 5000
	all := fillChat(t, s, jid, total)
	// A second chat with interleaved timestamps must never leak in.
	fillChat(t, s, "other@s.whatsapp.net", 300)

	for _, pageSize := range []int{1000, 137, 64} {
		// Pages arrive newest page first; each page is oldest first. So
		// the full history is the pages in reverse order of arrival.
		var pages [][]Message
		page, err := s.Latest(jid, pageSize)
		if err != nil {
			t.Fatalf("Latest: %v", err)
		}
		for len(page) > 0 {
			if len(page) > pageSize {
				t.Fatalf("page of %d messages, limit was %d", len(page), pageSize)
			}
			pages = append(pages, page)
			oldest := page[0]
			page, err = s.Before(jid, oldest.Timestamp, oldest.Seq, pageSize)
			if err != nil {
				t.Fatalf("Before: %v", err)
			}
		}

		var walked []Message
		for i := len(pages) - 1; i >= 0; i-- {
			walked = append(walked, pages[i]...)
		}
		if len(walked) != total {
			t.Fatalf("page size %d: walked %d messages, want %d", pageSize, len(walked), total)
		}
		for i := range walked {
			if walked[i].ID != all[i].ID || walked[i].Seq != all[i].Seq {
				t.Fatalf("page size %d: position %d is %q (seq %d), want %q (seq %d)",
					pageSize, i, walked[i].ID, walked[i].Seq, all[i].ID, all[i].Seq)
			}
		}
	}
}

func TestLatestAndBefore_Limits(t *testing.T) {
	s := newStore(t)
	all := fillChat(t, s, jid, 50)
	newest := all[len(all)-1]

	if msgs, _ := s.Latest(jid, 0); len(msgs) != 0 {
		t.Fatalf("Latest with limit 0 returned %d messages", len(msgs))
	}
	if msgs, _ := s.Latest(jid, -1); len(msgs) != 50 {
		t.Fatalf("Latest with no limit returned %d messages, want 50", len(msgs))
	}
	if msgs, _ := s.Latest(jid, 500); len(msgs) != 50 {
		t.Fatalf("Latest with a big limit returned %d messages, want 50", len(msgs))
	}
	msgs, _ := s.Latest(jid, 10)
	if len(msgs) != 10 || msgs[9].ID != newest.ID || msgs[0].ID != all[40].ID {
		t.Fatalf("Latest(10) is not the newest ten, oldest first: %q", ids(msgs))
	}

	// Before excludes the cursor message itself.
	msgs, _ = s.Before(jid, newest.Timestamp, newest.Seq, -1)
	if len(msgs) != 49 || msgs[len(msgs)-1].ID != all[48].ID {
		t.Fatalf("Before(newest) returned %d messages ending %q", len(msgs), msgs[len(msgs)-1].ID)
	}
	// Nothing is older than the oldest.
	if msgs, _ := s.Before(jid, all[0].Timestamp, all[0].Seq, 10); len(msgs) != 0 {
		t.Fatalf("Before(oldest) returned %d messages", len(msgs))
	}
	if msgs, _ := s.Latest("nobody@s.whatsapp.net", 10); len(msgs) != 0 {
		t.Fatalf("Latest of an unknown chat returned %d messages", len(msgs))
	}
}

// The paging queries must be answered straight from the index: reading
// only the rows returned, with no sorting step. If a schema or query
// change breaks that, opening a big chat silently becomes slow.
func TestPagingQueriesUseTheIndex(t *testing.T) {
	s := newStore(t)
	db, err := s.db()
	if err != nil {
		t.Fatal(err)
	}
	queries := map[string]string{
		"Latest": `SELECT ` + columns + ` FROM messages WHERE chat_jid = 'a' ORDER BY timestamp DESC, seq DESC LIMIT 200`,
		"Before": `SELECT ` + columns + ` FROM messages WHERE chat_jid = 'a' AND timestamp <= 5 AND (timestamp < 5 OR seq < 9) ORDER BY timestamp DESC, seq DESC LIMIT 200`,
		"List":   `SELECT ` + columns + ` FROM messages WHERE chat_jid = 'a' ORDER BY timestamp, seq`,
		"Each":   `SELECT ` + columns + ` FROM messages WHERE chat_jid = 'a' AND timestamp >= 5 AND (timestamp > 5 OR seq > 9) ORDER BY timestamp, seq LIMIT 200`,
	}
	for name, query := range queries {
		rows, err := db.Query(`EXPLAIN QUERY PLAN ` + query)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var plan []string
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatal(err)
			}
			plan = append(plan, detail)
		}
		rows.Close()

		joined := strings.Join(plan, "; ")
		if !strings.Contains(joined, "USING INDEX messages_by_time") || strings.Contains(joined, "TEMP B-TREE") {
			t.Errorf("%s does not walk the index in order: %s", name, joined)
		}
	}
}

func TestOldestAndCount(t *testing.T) {
	s := newStore(t)
	if _, ok, err := s.Oldest(jid); ok || err != nil {
		t.Fatalf("Oldest of an empty chat: ok=%v err=%v", ok, err)
	}
	if n, err := s.Count(jid); n != 0 || err != nil {
		t.Fatalf("Count of an empty chat = %d, %v", n, err)
	}

	_ = s.Append(Message{ID: "late", ChatJID: jid, Timestamp: 300})
	_ = s.Append(Message{ID: "early", ChatJID: jid, Timestamp: 100})
	_ = s.Append(Message{ID: "early2", ChatJID: jid, Timestamp: 100})
	_ = s.Append(Message{ID: "elsewhere", ChatJID: "b@s.whatsapp.net", Timestamp: 1})

	if msg, ok, _ := s.Oldest(jid); !ok || msg.ID != "early" {
		t.Fatalf("Oldest = %+v, ok=%v; want \"early\"", msg, ok)
	}
	if n, _ := s.Count(jid); n != 3 {
		t.Fatalf("Count = %d, want 3", n)
	}
}

func TestLastIncoming_NewestFirstAndOnlyFromOthers(t *testing.T) {
	s := newStore(t)
	_ = s.AppendMany([]Message{
		{ID: "in1", ChatJID: jid, Timestamp: 100},
		{ID: "out1", ChatJID: jid, Timestamp: 200, FromMe: true},
		{ID: "in2", ChatJID: jid, Timestamp: 300},
		{ID: "in3", ChatJID: jid, Timestamp: 400},
		{ID: "out2", ChatJID: jid, Timestamp: 500, FromMe: true},
		{ID: "other", ChatJID: "b@s.whatsapp.net", Timestamp: 600},
	})

	msgs, err := s.LastIncoming(jid, 2)
	if err != nil {
		t.Fatalf("LastIncoming: %v", err)
	}
	if got := ids(msgs); got != "in3in2" {
		t.Fatalf("LastIncoming(2) = %q, want \"in3in2\"", got)
	}
	if msgs, _ := s.LastIncoming(jid, -1); ids(msgs) != "in3in2in1" {
		t.Fatalf("LastIncoming(all) = %q, want \"in3in2in1\"", ids(msgs))
	}
}

// ---- status ----

func TestSetStatus_IsUnconditional(t *testing.T) {
	s := newStore(t)
	_ = s.AppendMany([]Message{
		{ID: "a", ChatJID: jid, FromMe: true, Status: StatusRead},
		{ID: "b", ChatJID: jid, Status: StatusNone},
		{ID: "c", ChatJID: jid, FromMe: true, Status: StatusPending},
	})

	if err := s.SetStatus(jid, []string{"a", "b", "missing"}, StatusFailed); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}

	if got := mustGet(t, s, jid, "a").Status; got != StatusFailed {
		t.Fatalf("a: status %d, want it lowered to StatusFailed", got)
	}
	if got := mustGet(t, s, jid, "b").Status; got != StatusFailed {
		t.Fatalf("b: status %d, want StatusFailed", got)
	}
	if got := mustGet(t, s, jid, "c").Status; got != StatusPending {
		t.Fatalf("c was not listed but changed to %d", got)
	}
}

func TestRaiseStatus_OnlyRaisesOwnMessages(t *testing.T) {
	s := newStore(t)
	_ = s.AppendMany([]Message{
		{ID: "pending", ChatJID: jid, FromMe: true, Status: StatusPending},
		{ID: "read", ChatJID: jid, FromMe: true, Status: StatusRead},
		{ID: "delivered", ChatJID: jid, FromMe: true, Status: StatusDelivered},
		{ID: "incoming", ChatJID: jid, FromMe: false},
		{ID: "elsewhere", ChatJID: "b@s.whatsapp.net", FromMe: true, Status: StatusPending},
	})

	changed, err := s.RaiseStatus(jid, []string{"pending", "read", "delivered", "incoming", "elsewhere", "missing"}, StatusDelivered)
	if err != nil {
		t.Fatalf("RaiseStatus: %v", err)
	}
	if !reflect.DeepEqual(changed, []string{"pending"}) {
		t.Fatalf("changed = %v, want [pending]", changed)
	}

	want := map[string]Status{
		"pending":   StatusDelivered, // raised
		"read":      StatusRead,      // already further along
		"delivered": StatusDelivered, // already there
		"incoming":  StatusNone,      // not ours
	}
	for id, status := range want {
		if got := mustGet(t, s, jid, id).Status; got != status {
			t.Errorf("%s: status %d, want %d", id, got, status)
		}
	}
	if got := mustGet(t, s, "b@s.whatsapp.net", "elsewhere").Status; got != StatusPending {
		t.Errorf("a message of another chat was changed to %d", got)
	}

	if changed, err := s.RaiseStatus(jid, nil, StatusRead); changed != nil || err != nil {
		t.Fatalf("RaiseStatus with no IDs = %v, %v", changed, err)
	}
}

// ---- reactions and poll votes ----

func TestSetReaction(t *testing.T) {
	s := newStore(t)
	_ = s.Append(Message{ID: "target", ChatJID: jid, Text: "hello"})
	reactions := func() []Reaction { return mustGet(t, s, jid, "target").Reactions }

	set := func(r Reaction) {
		t.Helper()
		if found, err := s.SetReaction(jid, "target", r); err != nil || !found {
			t.Fatalf("SetReaction(%+v): found=%v err=%v", r, found, err)
		}
	}

	set(Reaction{SenderJID: "alice", Emoji: "👍", Timestamp: 1})
	set(Reaction{SenderJID: "me", Emoji: "😂", Timestamp: 2})
	if got := reactions(); len(got) != 2 {
		t.Fatalf("expected 2 reactions, got %+v", got)
	}

	// A second reaction from the same person replaces the first.
	set(Reaction{SenderJID: "alice", Emoji: "❤️", Timestamp: 3})
	want := []Reaction{{SenderJID: "me", Emoji: "😂", Timestamp: 2}, {SenderJID: "alice", Emoji: "❤️", Timestamp: 3}}
	if got := reactions(); !reflect.DeepEqual(got, want) {
		t.Fatalf("after replacing: %+v, want %+v", got, want)
	}

	// An empty emoji removes that person's reaction only.
	set(Reaction{SenderJID: "me", Emoji: "", Timestamp: 4})
	if got := reactions(); len(got) != 1 || got[0].SenderJID != "alice" {
		t.Fatalf("after removing mine: %+v", got)
	}
	set(Reaction{SenderJID: "alice", Emoji: ""})
	if got := reactions(); got != nil {
		t.Fatalf("after removing all: %+v, want none", got)
	}
	// Removing a reaction that isn't there is fine.
	set(Reaction{SenderJID: "nobody", Emoji: ""})

	// The rest of the message is untouched.
	if got := mustGet(t, s, jid, "target").Text; got != "hello" {
		t.Fatalf("text changed to %q", got)
	}
}

func TestSetReaction_UnknownTarget(t *testing.T) {
	s := newStore(t)
	_ = s.Append(Message{ID: "target", ChatJID: "b@s.whatsapp.net"})

	found, err := s.SetReaction(jid, "target", Reaction{SenderJID: "me", Emoji: "👍"})
	if found || err != nil {
		t.Fatalf("found=%v err=%v; want false, nil (the ID exists only in another chat)", found, err)
	}
}

func TestSetPollVote(t *testing.T) {
	s := newStore(t)
	_ = s.Append(Message{ID: "poll", ChatJID: jid, Text: "lunch?"})
	votes := func() []PollVote { return mustGet(t, s, jid, "poll").PollVotes }

	set := func(v PollVote) {
		t.Helper()
		if found, err := s.SetPollVote(jid, "poll", v); err != nil || !found {
			t.Fatalf("SetPollVote(%+v): found=%v err=%v", v, found, err)
		}
	}

	set(PollVote{VoterJID: "alice", Options: []string{"aa"}, Timestamp: 1})
	set(PollVote{VoterJID: "me", Options: []string{"aa", "bb"}, Timestamp: 2})
	// Changing a vote replaces it.
	set(PollVote{VoterJID: "alice", Options: []string{"bb"}, Timestamp: 3})

	want := []PollVote{
		{VoterJID: "me", Options: []string{"aa", "bb"}, Timestamp: 2},
		{VoterJID: "alice", Options: []string{"bb"}, Timestamp: 3},
	}
	if got := votes(); !reflect.DeepEqual(got, want) {
		t.Fatalf("votes = %+v, want %+v", got, want)
	}

	// No options: the vote is withdrawn.
	set(PollVote{VoterJID: "me", Timestamp: 4})
	if got := votes(); len(got) != 1 || got[0].VoterJID != "alice" {
		t.Fatalf("after withdrawing mine: %+v", got)
	}

	if found, err := s.SetPollVote(jid, "missing", PollVote{VoterJID: "me", Options: []string{"aa"}}); found || err != nil {
		t.Fatalf("unknown poll: found=%v err=%v; want false, nil", found, err)
	}
}

// ---- edits, deletion, content swaps ----

func TestApplyEdit(t *testing.T) {
	s := newStore(t)
	oldRaw := base64.StdEncoding.EncodeToString([]byte("old content"))
	newRaw := base64.StdEncoding.EncodeToString([]byte("new content"))
	_ = s.Append(Message{ID: "m", ChatJID: jid, Text: "tpyo", RawProto: oldRaw, Timestamp: 100,
		Reactions: []Reaction{{SenderJID: "alice", Emoji: "👍"}}})
	before := mustGet(t, s, jid, "m")

	found, err := s.ApplyEdit(jid, "m", "typo", newRaw)
	if err != nil || !found {
		t.Fatalf("ApplyEdit: found=%v err=%v", found, err)
	}

	got := mustGet(t, s, jid, "m")
	if got.Text != "typo" || got.RawProto != newRaw || !got.Edited {
		t.Fatalf("edit not applied: %+v", got)
	}
	if got.Timestamp != 100 || got.Seq != before.Seq || len(got.Reactions) != 1 {
		t.Fatalf("edit changed more than text and content: %+v", got)
	}

	if found, err := s.ApplyEdit(jid, "missing", "x", ""); found || err != nil {
		t.Fatalf("unknown target: found=%v err=%v; want false, nil", found, err)
	}
}

func TestMarkDeleted(t *testing.T) {
	s := newStore(t)
	_ = s.Append(Message{ID: "m", ChatJID: jid, Text: "oops", MediaType: "image", Timestamp: 100, FromMe: true,
		RawProto:  base64.StdEncoding.EncodeToString([]byte("content")),
		Reactions: []Reaction{{SenderJID: "alice", Emoji: "👍"}}})

	found, err := s.MarkDeleted(jid, "m")
	if err != nil || !found {
		t.Fatalf("MarkDeleted: found=%v err=%v", found, err)
	}

	got := mustGet(t, s, jid, "m")
	if !got.Deleted || got.Text != "" || got.RawProto != "" || got.MediaType != "" || got.Reactions != nil {
		t.Fatalf("content not cleared: %+v", got)
	}
	// The row stays in place, so the chat can show a "deleted" marker.
	if got.Timestamp != 100 || !got.FromMe {
		t.Fatalf("deletion changed the message's place or sender: %+v", got)
	}

	if found, err := s.MarkDeleted(jid, "missing"); found || err != nil {
		t.Fatalf("unknown target: found=%v err=%v; want false, nil", found, err)
	}
}

func TestReplaceContent(t *testing.T) {
	s := newStore(t)
	sd := base64.StdEncoding.EncodeToString([]byte("standard image"))
	hd := base64.StdEncoding.EncodeToString([]byte("high definition image"))
	_ = s.Append(Message{ID: "m", ChatJID: jid, Text: "caption", MediaType: "image", RawProto: sd})

	// Empty text: the stored caption is kept.
	found, err := s.ReplaceContent(jid, "m", "image", "", hd, true)
	if err != nil || !found {
		t.Fatalf("ReplaceContent: found=%v err=%v", found, err)
	}
	got := mustGet(t, s, jid, "m")
	if got.RawProto != hd || !got.HD || got.Text != "caption" || got.MediaType != "image" {
		t.Fatalf("after HD upgrade: %+v", got)
	}

	// Non-empty text replaces it; so do the other fields.
	if _, err := s.ReplaceContent(jid, "m", "video", "new caption", sd, false); err != nil {
		t.Fatal(err)
	}
	got = mustGet(t, s, jid, "m")
	if got.RawProto != sd || got.HD || got.Text != "new caption" || got.MediaType != "video" {
		t.Fatalf("after second swap: %+v", got)
	}

	if found, err := s.ReplaceContent(jid, "missing", "image", "", hd, true); found || err != nil {
		t.Fatalf("unknown target: found=%v err=%v; want false, nil", found, err)
	}
}

// ---- removing and moving ----

func TestDeleteAndDeleteChat(t *testing.T) {
	s := newStore(t)
	other := "b@s.whatsapp.net"
	_ = s.AppendMany([]Message{
		{ID: "1", ChatJID: jid, Timestamp: 1},
		{ID: "2", ChatJID: jid, Timestamp: 2},
		{ID: "3", ChatJID: jid, Timestamp: 3},
		{ID: "1", ChatJID: other, Timestamp: 1},
	})

	if err := s.Delete(jid, "2"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := s.Delete(jid, "missing"); err != nil {
		t.Fatalf("Delete of an unknown message: %v", err)
	}
	if got := ids(mustList(t, s, jid)); got != "13" {
		t.Fatalf("after Delete: %q, want \"13\"", got)
	}

	if err := s.DeleteChat(jid); err != nil {
		t.Fatalf("DeleteChat: %v", err)
	}
	if n, _ := s.Count(jid); n != 0 {
		t.Fatalf("chat still has %d messages", n)
	}
	if n, _ := s.Count(other); n != 1 {
		t.Fatalf("the other chat has %d messages, want 1", n)
	}
}

func TestMoveChat_KeepsDestinationOnClash(t *testing.T) {
	s := newStore(t)
	from, to := "123@lid", "254700111222@s.whatsapp.net"
	_ = s.AppendMany([]Message{
		{ID: "only-from", ChatJID: from, Timestamp: 100, Text: "f1"},
		{ID: "both", ChatJID: from, Timestamp: 200, Text: "from's copy"},
		{ID: "late-from", ChatJID: from, Timestamp: 500, Text: "f2"},
		{ID: "both", ChatJID: to, Timestamp: 200, Text: "to's copy"},
		{ID: "only-to", ChatJID: to, Timestamp: 300, Text: "t1"},
		{ID: "bystander", ChatJID: "c@s.whatsapp.net", Timestamp: 1},
	})
	seqBefore := mustGet(t, s, from, "only-from").Seq

	if err := s.MoveChat(from, to); err != nil {
		t.Fatalf("MoveChat: %v", err)
	}

	if n, _ := s.Count(from); n != 0 {
		t.Fatalf("%d messages left under the old JID", n)
	}
	// Merged by timestamp, one copy of "both".
	msgs := mustList(t, s, to)
	if got := ids(msgs); got != "only-from"+"both"+"only-to"+"late-from" {
		t.Fatalf("merged order = %q", got)
	}
	if got := mustGet(t, s, to, "both").Text; got != "to's copy" {
		t.Fatalf("clash kept %q, want the destination's copy", got)
	}
	moved := mustGet(t, s, to, "only-from")
	if moved.ChatJID != to || moved.Seq != seqBefore {
		t.Fatalf("moved message: %+v, want ChatJID %q and Seq %d", moved, to, seqBefore)
	}
	if n, _ := s.Count("c@s.whatsapp.net"); n != 1 {
		t.Fatal("an unrelated chat was touched")
	}
}

func TestMoveChat_SameOrUnknownJIDIsNoop(t *testing.T) {
	s := newStore(t)
	_ = s.Append(Message{ID: "1", ChatJID: jid})

	if err := s.MoveChat(jid, jid); err != nil {
		t.Fatalf("MoveChat onto itself: %v", err)
	}
	if err := s.MoveChat("unknown@s.whatsapp.net", jid); err != nil {
		t.Fatalf("MoveChat from an unknown chat: %v", err)
	}
	if n, _ := s.Count(jid); n != 1 {
		t.Fatalf("Count = %d, want 1", n)
	}
}

// ---- search ----

func TestSearch(t *testing.T) {
	s := newStore(t)
	other := "b@s.whatsapp.net"
	_ = s.AppendMany([]Message{
		{ID: "1", ChatJID: jid, Timestamp: 100, Text: "Lunch at noon?"},
		{ID: "2", ChatJID: jid, Timestamp: 200, Text: "no lunch today"},
		{ID: "3", ChatJID: jid, Timestamp: 300, Text: "see you at the CAFÉ"},
		{ID: "4", ChatJID: jid, Timestamp: 400, Text: "100% sure_thing"},
		{ID: "5", ChatJID: other, Timestamp: 250, Text: "LUNCH is ready"},
		{ID: "6", ChatJID: other, Timestamp: 50, Text: "unrelated"},
	})

	search := func(chatJID, query string, limit int) string {
		t.Helper()
		msgs, err := s.Search(chatJID, query, limit)
		if err != nil {
			t.Fatalf("Search(%q, %q): %v", chatJID, query, err)
		}
		return ids(msgs)
	}

	if got := search(jid, "lunch", -1); got != "21" {
		t.Errorf("in one chat: %q, want \"21\" (newest first)", got)
	}
	if got := search("", "LuNcH", -1); got != "521" {
		t.Errorf("in every chat: %q, want \"521\" (newest first)", got)
	}
	if got := search("", "lunch", 2); got != "52" {
		t.Errorf("limited to 2: %q, want \"52\"", got)
	}
	// Case is ignored beyond A-Z too.
	if got := search("", "café", -1); got != "3" {
		t.Errorf("non-ASCII case: %q, want \"3\"", got)
	}
	// % and _ are ordinary characters, not wildcards.
	if got := search("", "100%", -1); got != "4" {
		t.Errorf("literal %%: %q, want \"4\"", got)
	}
	if got := search("", "sure_", -1); got != "4" {
		t.Errorf("literal _: %q, want \"4\"", got)
	}
	if got := search("", "s_re", -1); got != "" {
		t.Errorf("_ acted as a wildcard: %q", got)
	}
	if got := search("", "nothing like this", -1); got != "" {
		t.Errorf("no match expected, got %q", got)
	}
}

// ---- pruning ----

func TestPrune_DeletesOldButKeepsNewestPerChat(t *testing.T) {
	s := newStore(t)
	now := time.Now()
	old := now.Add(-72 * time.Hour).UnixMilli()
	recent := now.Add(-time.Hour).UnixMilli()

	var batch []Message
	// "busy": 10 old messages and 5 recent ones.
	for i := 0; i < 10; i++ {
		batch = append(batch, Message{ID: fmt.Sprint("old", i), ChatJID: "busy@g.us", Timestamp: old + int64(i)})
	}
	for i := 0; i < 5; i++ {
		batch = append(batch, Message{ID: fmt.Sprint("new", i), ChatJID: "busy@g.us", Timestamp: recent + int64(i)})
	}
	// "quiet": only 10 old messages.
	for i := 0; i < 10; i++ {
		batch = append(batch, Message{ID: fmt.Sprint("old", i), ChatJID: "quiet@s.whatsapp.net", Timestamp: old + int64(i)})
	}
	// "fresh": only recent messages.
	for i := 0; i < 4; i++ {
		batch = append(batch, Message{ID: fmt.Sprint("new", i), ChatJID: "fresh@s.whatsapp.net", Timestamp: recent + int64(i)})
	}
	if err := s.AppendMany(batch); err != nil {
		t.Fatal(err)
	}

	deleted, err := s.Prune(now.Add(-48*time.Hour), 3)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	// busy: its newest 3 are recent, so all 10 old ones go.
	// quiet: its newest 3 (old7..old9) are spared, 7 go.
	if deleted != 17 {
		t.Fatalf("deleted = %d, want 17", deleted)
	}
	if got := ids(mustList(t, s, "busy@g.us")); got != "new0new1new2new3new4" {
		t.Errorf("busy chat = %q", got)
	}
	if got := ids(mustList(t, s, "quiet@s.whatsapp.net")); got != "old7old8old9" {
		t.Errorf("quiet chat = %q, want its newest three kept", got)
	}
	if n, _ := s.Count("fresh@s.whatsapp.net"); n != 4 {
		t.Errorf("fresh chat has %d messages, want 4", n)
	}

	// Nothing left to do the second time.
	if deleted, err := s.Prune(now.Add(-48*time.Hour), 3); deleted != 0 || err != nil {
		t.Fatalf("second Prune = %d, %v; want 0, nil", deleted, err)
	}
}

func TestPrune_KeepZeroDeletesEverythingOld(t *testing.T) {
	s := newStore(t)
	now := time.Now()
	_ = s.AppendMany([]Message{
		{ID: "old1", ChatJID: jid, Timestamp: now.Add(-100 * time.Hour).UnixMilli()},
		{ID: "old2", ChatJID: jid, Timestamp: now.Add(-90 * time.Hour).UnixMilli()},
		{ID: "new", ChatJID: jid, Timestamp: now.UnixMilli()},
	})

	deleted, err := s.Prune(now.Add(-RetainFor), 0)
	if err != nil || deleted != 2 {
		t.Fatalf("Prune = %d, %v; want 2, nil", deleted, err)
	}
	if got := ids(mustList(t, s, jid)); got != "new" {
		t.Fatalf("left = %q, want \"new\"", got)
	}
}

// ---- Each ----

func TestEach_VisitsEverythingChatByChatOldestFirst(t *testing.T) {
	s := newStore(t)
	// More than one page in the first chat, so paging is exercised.
	wantA := fillChat(t, s, "a@s.whatsapp.net", 2*eachPage+17)
	wantB := fillChat(t, s, "b@s.whatsapp.net", eachPage) // exactly one page
	wantC := fillChat(t, s, "c@s.whatsapp.net", 3)

	var got []Message
	err := s.Each(func(m Message) error {
		got = append(got, m)
		return nil
	})
	if err != nil {
		t.Fatalf("Each: %v", err)
	}

	want := append(append(wantA, wantB...), wantC...)
	if len(got) != len(want) {
		t.Fatalf("visited %d messages, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].ChatJID != want[i].ChatJID || got[i].ID != want[i].ID {
			t.Fatalf("position %d: %s/%s, want %s/%s", i, got[i].ChatJID, got[i].ID, want[i].ChatJID, want[i].ID)
		}
	}
}

func TestEach_CallbackMayChangeTheStore(t *testing.T) {
	s := newStore(t)
	const total = 3*eachPage + 5
	all := fillChat(t, s, jid, total)

	// Delete every second message and edit the others while walking -
	// what the legacy fold does. Every message must still be seen once.
	visited := 0
	err := s.Each(func(m Message) error {
		visited++
		if visited%2 == 0 {
			return s.Delete(m.ChatJID, m.ID)
		}
		_, err := s.ApplyEdit(m.ChatJID, m.ID, "seen", "")
		return err
	})
	if err != nil {
		t.Fatalf("Each: %v", err)
	}
	if visited != total {
		t.Fatalf("visited %d messages, want %d", visited, total)
	}

	left := mustList(t, s, jid)
	if len(left) != (total+1)/2 {
		t.Fatalf("%d messages left, want %d", len(left), (total+1)/2)
	}
	for i, m := range left {
		if m.Text != "seen" || m.ID != all[2*i].ID {
			t.Fatalf("left[%d] = %q %q, want %q edited", i, m.ID, m.Text, all[2*i].ID)
		}
	}
}

func TestEach_StopsAtTheCallbacksError(t *testing.T) {
	s := newStore(t)
	fillChat(t, s, jid, 10)
	stop := errors.New("stop")

	calls := 0
	err := s.Each(func(Message) error {
		calls++
		if calls == 3 {
			return stop
		}
		return nil
	})
	if !errors.Is(err, stop) || calls != 3 {
		t.Fatalf("Each = %v after %d calls; want stop after 3", err, calls)
	}
}

// ---- meta ----

func TestMeta(t *testing.T) {
	s := newStore(t)

	if got, err := s.Meta("unset"); got != "" || err != nil {
		t.Fatalf("unset key = %q, %v; want \"\", nil", got, err)
	}
	if err := s.SetMeta("legacy_fold", "pending"); err != nil {
		t.Fatalf("SetMeta: %v", err)
	}
	if err := s.SetMeta("legacy_fold", "done"); err != nil {
		t.Fatalf("SetMeta (overwrite): %v", err)
	}
	if got, _ := s.Meta("legacy_fold"); got != "done" {
		t.Fatalf("Meta = %q, want \"done\"", got)
	}
}

// ---- concurrency ----

func TestConcurrentUse(t *testing.T) {
	s := newStore(t)
	_ = s.Append(Message{ID: "target", ChatJID: jid, Timestamp: 1})

	const writers, perWriter = 8, 150
	var wg sync.WaitGroup
	errs := make(chan error, 4*writers)

	for w := 0; w < writers; w++ {
		// Each writer adds its own messages (singly and in batches) and
		// reacts to the shared target under its own name...
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				msg := Message{ID: fmt.Sprintf("w%d-%d", w, i), ChatJID: jid, Timestamp: int64(1000 + i)}
				var err error
				if i%10 == 0 {
					err = s.AppendMany([]Message{msg, {ID: "shared", ChatJID: jid, Timestamp: 5}})
				} else {
					err = s.Append(msg)
				}
				if err != nil {
					errs <- err
					return
				}
			}
			if _, err := s.SetReaction(jid, "target", Reaction{SenderJID: fmt.Sprint("writer", w), Emoji: "👍"}); err != nil {
				errs <- err
			}
		}()
		// ...while a reader pages through the chat.
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 40; i++ {
				page, err := s.Latest(jid, 50)
				if err != nil {
					errs <- err
					return
				}
				if len(page) > 1 {
					if _, err := s.Before(jid, page[0].Timestamp, page[0].Seq, 50); err != nil {
						errs <- err
						return
					}
				}
				if _, err := s.Count(jid); err != nil {
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

	// target + shared (stored once, however many tried) + everyone's own.
	if n, _ := s.Count(jid); n != 2+writers*perWriter {
		t.Fatalf("Count = %d, want %d", n, 2+writers*perWriter)
	}
	// No reaction was lost to a concurrent read-modify-write.
	if got := len(mustGet(t, s, jid, "target").Reactions); got != writers {
		t.Fatalf("%d reactions stored, want %d", got, writers)
	}
}
