package msgstore

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codebyoketch/wa-cli/internal/localdb"
)

// oldMessagesJSON is a messages.json as the JSON store wrote it: chat JID
// -> list of messages, indented. It includes what the old store let
// through: the same ID twice in a chat, and a message with no ID.
const oldMessagesJSON = `{
  "111@s.whatsapp.net": [
    {
      "id": "A1",
      "chatJid": "111@s.whatsapp.net",
      "senderJid": "111@s.whatsapp.net",
      "timestamp": 1000,
      "text": "first copy",
      "fromMe": false,
      "rawProto": "aGVsbG8gd29ybGQ="
    },
    {
      "id": "A2",
      "chatJid": "111@s.whatsapp.net",
      "senderJid": "me",
      "timestamp": 2000,
      "text": "a photo",
      "fromMe": true,
      "mediaType": "image"
    },
    {
      "id": "A1",
      "chatJid": "111@s.whatsapp.net",
      "senderJid": "111@s.whatsapp.net",
      "timestamp": 1000,
      "text": "second copy",
      "fromMe": false
    },
    {
      "id": "",
      "chatJid": "111@s.whatsapp.net",
      "senderJid": "111@s.whatsapp.net",
      "timestamp": 3000,
      "text": "no id",
      "fromMe": false
    }
  ],
  "222@g.us": [
    {
      "id": "A1",
      "chatJid": "222@g.us",
      "senderJid": "333@s.whatsapp.net",
      "timestamp": 1500,
      "text": "same id, other chat",
      "fromMe": false
    }
  ],
  "empty@s.whatsapp.net": null
}`

// legacyDir returns a temp data dir holding a messages.json with the
// given content; the database is closed again when the test ends.
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

func mustMeta(t *testing.T, s *Store, key string) string {
	t.Helper()
	value, err := s.Meta(key)
	if err != nil {
		t.Fatalf("Meta(%q): %v", key, err)
	}
	return value
}

func TestLegacyImport_ImportsDeduplicatesAndRenames(t *testing.T) {
	dir := legacyDir(t, oldMessagesJSON)
	s := New(dir)

	// The first call of any kind triggers the import.
	msgs, err := s.List("111@s.whatsapp.net")
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	// A1 once (the first copy), A2, and the message without an ID.
	if len(msgs) != 3 {
		t.Fatalf("imported %d messages into the first chat, want 3: %+v", len(msgs), msgs)
	}
	a1 := msgs[0]
	if a1.ID != "A1" || a1.Text != "first copy" || a1.RawProto != "aGVsbG8gd29ybGQ=" || a1.SenderJID != "111@s.whatsapp.net" || a1.Timestamp != 1000 {
		t.Fatalf("A1 imported wrongly: %+v", a1)
	}
	a2 := msgs[1]
	if a2.ID != "A2" || !a2.FromMe || a2.MediaType != "image" || a2.SenderJID != "me" {
		t.Fatalf("A2 imported wrongly: %+v", a2)
	}
	if noID := msgs[2]; !strings.HasPrefix(noID.ID, "local-") || noID.Text != "no id" {
		t.Fatalf("message without ID imported wrongly: %+v", noID)
	}
	// The same ID in another chat is a different message.
	group := mustList(t, s, "222@g.us")
	if len(group) != 1 || group[0].Text != "same id, other chat" {
		t.Fatalf("second chat imported wrongly: %+v", group)
	}

	if got := mustMeta(t, s, "legacy_messages_imported"); got != "1" {
		t.Errorf("legacy_messages_imported = %q, want \"1\"", got)
	}
	if got := mustMeta(t, s, "legacy_fold"); got != "pending" {
		t.Errorf("legacy_fold = %q, want \"pending\"", got)
	}
	if got := mustMeta(t, s, "legacy_messages_error"); got != "" {
		t.Errorf("legacy_messages_error = %q, want unset", got)
	}

	// The file is renamed, never deleted or rewritten.
	old := filepath.Join(dir, legacyFile)
	if fileExists(old) {
		t.Error("messages.json is still there")
	}
	kept, err := os.ReadFile(old + ".migrated")
	if err != nil || string(kept) != oldMessagesJSON {
		t.Errorf("messages.json.migrated missing or changed (err=%v)", err)
	}
}

func TestLegacyImport_LargeFileWrittenTheOldWay(t *testing.T) {
	// Build the file exactly as the old store's save did - MarshalIndent
	// of chat JID -> messages - with every fourth message stored twice.
	const chats, perChat = 20, 300
	old := map[string][]Message{}
	unique := 0
	for c := 0; c < chats; c++ {
		chatJID := fmt.Sprintf("2547%08d@s.whatsapp.net", c)
		for i := 0; i < perChat; i++ {
			msg := Message{
				ID: fmt.Sprintf("ID%d-%d", c, i), ChatJID: chatJID, SenderJID: chatJID,
				Timestamp: int64(1000 + i), Text: fmt.Sprint("message ", i),
				RawProto: base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", 300+i))),
			}
			old[chatJID] = append(old[chatJID], msg)
			unique++
			if i%4 == 0 {
				again := msg
				again.Text = "duplicate"
				old[chatJID] = append(old[chatJID], again)
			}
		}
	}
	data, err := json.MarshalIndent(old, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	dir := legacyDir(t, string(data))
	s := New(dir)

	total := 0
	err = s.Each(func(m Message) error {
		total++
		if m.Text == "duplicate" {
			return fmt.Errorf("a second copy of %s was kept instead of the first", m.ID)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Each: %v", err)
	}
	if total != unique {
		t.Fatalf("imported %d messages, want %d", total, unique)
	}
	// Spot-check one chat against the original, content included.
	chatJID := fmt.Sprintf("2547%08d@s.whatsapp.net", 7)
	got := mustList(t, s, chatJID)
	if len(got) != perChat {
		t.Fatalf("chat has %d messages, want %d", len(got), perChat)
	}
	for i, m := range got {
		want := old[chatJID][i+(i+3)/4] // skip the duplicates before it
		if m.ID != want.ID || m.Text != want.Text || m.RawProto != want.RawProto || m.Timestamp != want.Timestamp {
			t.Fatalf("message %d: got %+v, want %+v", i, m, want)
		}
	}
}

func TestLegacyImport_HappensOnce(t *testing.T) {
	dir := legacyDir(t, oldMessagesJSON)
	first := New(dir)
	if n, err := first.Count("111@s.whatsapp.net"); err != nil || n != 3 {
		t.Fatalf("Count after import = %d, %v", n, err)
	}
	// The WhatsApp layer finishes its part...
	if err := first.SetMeta("legacy_fold", "done"); err != nil {
		t.Fatal(err)
	}

	// ...and later an old-style file shows up again (say an old wa
	// binary was run). It is not imported, and nothing is reset.
	another := `{"999@s.whatsapp.net": [{"id": "Z", "chatJid": "999@s.whatsapp.net", "timestamp": 1, "text": "late"}]}`
	if err := os.WriteFile(filepath.Join(dir, legacyFile), []byte(another), 0o600); err != nil {
		t.Fatal(err)
	}
	second := New(dir)
	if n, _ := second.Count("999@s.whatsapp.net"); n != 0 {
		t.Fatalf("a second file was imported (%d messages)", n)
	}
	if got := mustMeta(t, second, "legacy_fold"); got != "done" {
		t.Fatalf("legacy_fold = %q, want it left at \"done\"", got)
	}
	kept, _ := os.ReadFile(filepath.Join(dir, legacyFile+".migrated"))
	if string(kept) != oldMessagesJSON {
		t.Fatal("the first backup was overwritten")
	}
}

func TestLegacyImport_MetaAsFirstCallSeesTheResult(t *testing.T) {
	// The WhatsApp layer asks for legacy_fold at startup, possibly before
	// anything else touched the store.
	s := New(legacyDir(t, oldMessagesJSON))
	if got := mustMeta(t, s, "legacy_fold"); got != "pending" {
		t.Fatalf("legacy_fold = %q, want \"pending\"", got)
	}
}

func TestLegacyImport_NothingToImport(t *testing.T) {
	for name, content := range map[string]string{"empty object": `{}`, "null": `null`, "empty file": ``} {
		t.Run(name, func(t *testing.T) {
			dir := legacyDir(t, content)
			s := New(dir)

			if err := s.Append(Message{ID: "1", ChatJID: jid}); err != nil {
				t.Fatalf("Append: %v", err)
			}
			if got := mustMeta(t, s, "legacy_messages_imported"); got != "1" {
				t.Errorf("legacy_messages_imported = %q, want \"1\"", got)
			}
			// Nothing came in, so there is nothing to fold.
			if got := mustMeta(t, s, "legacy_fold"); got != "" {
				t.Errorf("legacy_fold = %q, want unset", got)
			}
			if !fileExists(filepath.Join(dir, legacyFile+".migrated")) {
				t.Error("expected the file to be renamed")
			}
		})
	}
}

func TestLegacyImport_NoFile(t *testing.T) {
	s := newStore(t)
	_ = s.Append(Message{ID: "1", ChatJID: jid})
	if got := mustMeta(t, s, "legacy_messages_imported"); got != "" {
		t.Fatalf("legacy_messages_imported = %q without a file, want unset", got)
	}
}

func TestLegacyImport_DamagedFileIsLeftAloneAndStoreStillWorks(t *testing.T) {
	damaged := map[string]string{
		// Stops in the middle of the second chat: the first chat's
		// messages must not be kept either.
		"cut off":      oldMessagesJSON[:len(oldMessagesJSON)-150],
		"not json":     `this is not JSON`,
		"wrong shape":  `["a", "list"]`,
		"wrong values": `{"111@s.whatsapp.net": {"id": "A1"}}`,
	}
	for name, content := range damaged {
		t.Run(name, func(t *testing.T) {
			dir := legacyDir(t, content)
			s := New(dir)

			// Normal operations work and report no error.
			if err := s.Append(Message{ID: "new", ChatJID: jid, Text: "works"}); err != nil {
				t.Fatalf("Append: %v", err)
			}
			if n, err := s.Count(jid); err != nil || n != 1 {
				t.Fatalf("Count = %d, %v; want 1, nil", n, err)
			}
			// Nothing from the file was kept.
			if n, _ := s.Count("111@s.whatsapp.net"); n != 0 {
				t.Errorf("%d messages of the damaged file were kept", n)
			}

			if got := mustMeta(t, s, "legacy_messages_imported"); got != "" {
				t.Errorf("legacy_messages_imported = %q, want unset", got)
			}
			if got := mustMeta(t, s, "legacy_fold"); got != "" {
				t.Errorf("legacy_fold = %q, want unset", got)
			}
			if got := mustMeta(t, s, "legacy_messages_error"); !strings.Contains(got, legacyFile) {
				t.Errorf("legacy_messages_error = %q, want it to name the file", got)
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

func TestLegacyImport_RepairedFileIsImportedOnNextStart(t *testing.T) {
	dir := legacyDir(t, `{"broken`)
	first := New(dir)
	if n, _ := first.Count("111@s.whatsapp.net"); n != 0 {
		t.Fatal("imported from a broken file")
	}

	// The user fixes the file; the next start (a new Store) picks it up.
	if err := os.WriteFile(filepath.Join(dir, legacyFile), []byte(oldMessagesJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	second := New(dir)
	if n, err := second.Count("111@s.whatsapp.net"); err != nil || n != 3 {
		t.Fatalf("Count after repair = %d, %v; want 3", n, err)
	}
	if got := mustMeta(t, second, "legacy_messages_error"); got != "" {
		t.Fatalf("legacy_messages_error = %q, want it cleared", got)
	}
}
