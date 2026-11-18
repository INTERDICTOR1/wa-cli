package msgstore

import (
	"encoding/base64"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/codebyoketch/wa-cli/internal/localdb"
)

// The benchmarks measure what the TUI does all day - saving an incoming
// message and loading a page of a chat - against a database the size a
// heavy user would build up: 100,000 messages, each carrying about 1 KB
// of content (RawProto).
//
// Run them with:
//
//	go test ./internal/msgstore/ -run '^$' -bench .
//
// Building the database takes several seconds and happens once, shared by
// all benchmarks. The reading benchmarks come first so they see exactly
// the 100,000 messages; the writing ones then add to it.
//
// When reading the results, note that B/op and allocs/op count Go memory
// only, and that the time of the writing benchmarks is mostly the disk:
// every few MB written, SQLite copies its write-ahead log into the main
// file and waits for the disk to confirm (a "checkpoint"). That wait
// lands on whichever write triggered it, and the average includes it.

const (
	benchChats       = 50
	benchPerChat     = 2000 // 50 x 2000 = 100,000 messages
	benchContentSize = 1024
	benchText        = "a message of ordinary length, the kind people send all day"
)

var bench struct {
	once sync.Once
	dir  string
	err  error
	// next numbers the messages the benchmarks add, so IDs stay unique
	// however often the benchmark functions are called.
	next atomic.Int64
}

// benchContent returns about 1 KB of random bytes in base64, like a real
// message's RawProto.
func benchContent(rng *rand.Rand) string {
	content := make([]byte, benchContentSize)
	rng.Read(content)
	return base64.StdEncoding.EncodeToString(content)
}

func benchChatJID(chat int) string {
	return fmt.Sprintf("2547000%05d@s.whatsapp.net", chat)
}

// benchID is the ID of preloaded message number i of chat number chat.
func benchID(chat, i int) string {
	return fmt.Sprintf("3EB0%08X%08X", chat, i)
}

// benchStore returns a Store on the preloaded database, building it on
// first use.
func benchStore(b *testing.B) *Store {
	b.Helper()
	bench.once.Do(func() {
		bench.dir, bench.err = os.MkdirTemp("", "wa-msgstore-bench-")
		if bench.err != nil {
			return
		}
		s := New(bench.dir)
		rng := rand.New(rand.NewSource(42))
		for chat := 0; chat < benchChats; chat++ {
			batch := make([]Message, benchPerChat)
			for i := range batch {
				batch[i] = Message{
					ID:        benchID(chat, i),
					ChatJID:   benchChatJID(chat),
					SenderJID: benchChatJID(chat),
					// One a minute, give or take.
					Timestamp: 1_700_000_000_000 + int64(i)*60_000 + int64(rng.Intn(1000)),
					Text:      benchText,
					FromMe:    i%2 == 0,
					RawProto:  benchContent(rng),
				}
			}
			if bench.err = s.AppendMany(batch); bench.err != nil {
				return
			}
		}
	})
	if bench.err != nil {
		b.Fatalf("building the benchmark database: %v", bench.err)
	}
	return New(bench.dir)
}

// removeBenchDir deletes the benchmark database; TestMain calls it at the
// end of the run, after closing the database.
func removeBenchDir() {
	if bench.dir != "" {
		os.RemoveAll(bench.dir)
	}
}

// benchMessage returns a new message for chat number chat, timestamped
// after everything preloaded.
func benchMessage(chat int, content string) Message {
	n := bench.next.Add(1)
	return Message{
		ID:        fmt.Sprintf("BENCH%012d", n),
		ChatJID:   benchChatJID(chat),
		SenderJID: benchChatJID(chat),
		Timestamp: 1_900_000_000_000 + n,
		Text:      benchText,
		RawProto:  content,
	}
}

// BenchmarkDatabaseSize isn't a timing: it reports how big the preloaded
// database is on disk, as a check on the storage cost per message.
func BenchmarkDatabaseSize(b *testing.B) {
	s := benchStore(b)
	db, err := s.db()
	if err != nil {
		b.Fatal(err)
	}
	// Fold the write-ahead log into the main file, so that the main
	// file's size is the whole story.
	if _, err := localdb.Exec(db, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		b.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(bench.dir, localdb.FileName))
	if err != nil {
		b.Fatal(err)
	}
	var total int
	if err := db.QueryRow(`SELECT count(*) FROM messages`).Scan(&total); err != nil {
		b.Fatal(err)
	}
	b.ReportMetric(0, "ns/op") // nothing was timed
	b.ReportMetric(float64(total), "messages")
	b.ReportMetric(float64(info.Size())/(1<<20), "MB-on-disk")
	b.ReportMetric(float64(info.Size())/float64(total), "bytes/message")
}

// BenchmarkLatest200: opening a chat - its newest 200 messages.
func BenchmarkLatest200(b *testing.B) {
	s := benchStore(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		msgs, err := s.Latest(benchChatJID(i%benchChats), 200)
		if err != nil || len(msgs) != 200 {
			b.Fatalf("Latest: %d messages, err=%v", len(msgs), err)
		}
	}
}

// BenchmarkBefore200: scrolling up - the 200 messages before a point in
// the middle of a chat's history.
func BenchmarkBefore200(b *testing.B) {
	s := benchStore(b)
	// The timestamp of preloaded message number benchPerChat/2.
	middle := int64(1_700_000_000_000 + benchPerChat/2*60_000)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		msgs, err := s.Before(benchChatJID(i%benchChats), middle, 0, 200)
		if err != nil || len(msgs) != 200 {
			b.Fatalf("Before: %d messages, err=%v", len(msgs), err)
		}
	}
}

// BenchmarkGet: looking one message up by chat and ID (the message a
// reply quotes, the target of a receipt).
func BenchmarkGet(b *testing.B) {
	s := benchStore(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		chat := i % benchChats
		// 7919 is a prime, to hop around the chat instead of walking it.
		id := benchID(chat, (i*7919)%benchPerChat)
		if _, ok, err := s.Get(benchChatJID(chat), id); err != nil || !ok {
			b.Fatalf("Get: ok=%v err=%v", ok, err)
		}
	}
}

// BenchmarkInsert: saving one incoming message (its own transaction).
func BenchmarkInsert(b *testing.B) {
	s := benchStore(b)
	content := benchContent(rand.New(rand.NewSource(1)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		inserted, err := s.Insert(benchMessage(i%benchChats, content))
		if err != nil || !inserted {
			b.Fatalf("Insert: inserted=%v err=%v", inserted, err)
		}
	}
}

// BenchmarkInsertDuplicate: being handed a message that is already
// stored, as happens when WhatsApp re-delivers history. Nothing is
// written to disk.
func BenchmarkInsertDuplicate(b *testing.B) {
	s := benchStore(b)
	msg := benchMessage(0, benchContent(rand.New(rand.NewSource(1))))
	if _, err := s.Insert(msg); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		inserted, err := s.Insert(msg)
		if err != nil || inserted {
			b.Fatalf("Insert: inserted=%v err=%v", inserted, err)
		}
	}
}

// BenchmarkInsertMany100: saving a history batch of 100 messages in one
// transaction. Divide by 100 for the cost per message.
func BenchmarkInsertMany100(b *testing.B) {
	s := benchStore(b)
	content := benchContent(rand.New(rand.NewSource(1)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		batch := make([]Message, 100)
		for j := range batch {
			batch[j] = benchMessage(i%benchChats, content)
		}
		inserted, err := s.InsertMany(batch)
		if err != nil || len(inserted) != len(batch) {
			b.Fatalf("InsertMany: %d inserted, err=%v", len(inserted), err)
		}
	}
}
