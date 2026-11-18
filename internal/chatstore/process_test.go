package chatstore

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// This file tests the store from several real processes at once, because
// that is the situation it is about: `wa chat send` in one terminal while
// the TUI runs in another. The "other process" is this same test binary,
// started again with helperEnv set; TestMain sees the variable and runs
// runHelper instead of the tests.

const (
	helperEnv     = "WA_CHATSTORE_HELPER" // set to "1" in a helper process
	helperDirEnv  = "WA_CHATSTORE_HELPER_DIR"
	helperNameEnv = "WA_CHATSTORE_HELPER_NAME"

	helperChat   = "shared@g.us"
	helperRounds = 50
)

// runHelper is the body of a helper process: it bumps the shared chat's
// unread count and upserts it, helperRounds times. It returns the exit
// code.
func runHelper() int {
	dir, name := os.Getenv(helperDirEnv), os.Getenv(helperNameEnv)

	// Tell the test we are up, then wait for its go-ahead, so that all
	// helpers hit the database at the same moment.
	if err := os.WriteFile(filepath.Join(dir, "ready-"+name), nil, 0o600); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if !waitFor(func() bool { return fileExists(filepath.Join(dir, "start")) }) {
		fmt.Fprintln(os.Stderr, "never told to start")
		return 1
	}

	s := New(dir)
	defer s.Close()
	for i := 0; i < helperRounds; i++ {
		if err := s.IncrementUnread(helperChat); err != nil {
			fmt.Fprintln(os.Stderr, "IncrementUnread:", err)
			return 1
		}
		if err := s.Upsert(Chat{JID: helperChat, Name: "Shared", LastMessageAt: int64(i)}); err != nil {
			fmt.Fprintln(os.Stderr, "Upsert:", err)
			return 1
		}
		if err := s.Upsert(Chat{JID: name + "@s.whatsapp.net", Name: name}); err != nil {
			fmt.Fprintln(os.Stderr, "Upsert:", err)
			return 1
		}
	}
	return 0
}

// waitFor polls until done reports true, giving up after 30 seconds.
func waitFor(done func() bool) bool {
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if done() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

func TestSeveralProcesses_FirstUseAndNoLostUpdates(t *testing.T) {
	// Nothing has opened this data dir yet: the helpers also race to
	// create the database and to import the old chats.json.
	dir := legacyDir(t, oldChatsJSON)
	const processes = 3

	type helper struct {
		name string
		done chan error
		out  []byte
	}
	helpers := make([]*helper, processes)
	for i := range helpers {
		h := &helper{name: fmt.Sprint("p", i), done: make(chan error, 1)}
		// -test.run=^$ matches no test; TestMain takes over before that
		// matters anyway.
		cmd := exec.Command(os.Args[0], "-test.run=^$")
		cmd.Env = append(os.Environ(), helperEnv+"=1", helperDirEnv+"="+dir, helperNameEnv+"="+h.name)
		go func() {
			out, err := cmd.CombinedOutput()
			h.out = out
			h.done <- err
		}()
		helpers[i] = h
	}

	allReady := waitFor(func() bool {
		for _, h := range helpers {
			if !fileExists(filepath.Join(dir, "ready-"+h.name)) {
				return false
			}
		}
		return true
	})
	// Release them even if something went wrong, so none is left waiting.
	if err := os.WriteFile(filepath.Join(dir, "start"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, h := range helpers {
		if err := <-h.done; err != nil {
			t.Errorf("helper %s failed: %v\n%s", h.name, err, h.out)
		}
	}
	if !allReady {
		t.Fatal("the helper processes did not start in time")
	}

	s := New(dir)
	// With the old JSON store, whoever saved last won and the others'
	// increments vanished. Now every single one must be counted.
	shared := mustGet(t, s, helperChat)
	if shared.UnreadCount != processes*helperRounds {
		t.Errorf("UnreadCount = %d, want %d", shared.UnreadCount, processes*helperRounds)
	}
	if shared.LastMessageAt != helperRounds-1 {
		t.Errorf("LastMessageAt = %d, want %d", shared.LastMessageAt, helperRounds-1)
	}
	// The old file was imported exactly once (3 chats), next to the
	// shared chat and one chat per helper.
	chats, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if want := 3 + 1 + processes; len(chats) != want {
		t.Errorf("%d chats, want %d", len(chats), want)
	}
	if got := mustGet(t, s, "111@s.whatsapp.net"); got.Name != "Alice" || got.UnreadCount != 3 {
		t.Errorf("imported chat = %+v", got)
	}
	if got := meta(t, dir, "legacy_chats_imported"); got != "1" {
		t.Errorf("legacy_chats_imported = %q, want \"1\"", got)
	}
	if got := meta(t, dir, "legacy_chats_error"); got != "" {
		t.Errorf("legacy_chats_error = %q, want unset", got)
	}
	if fileExists(filepath.Join(dir, legacyFile)) || !fileExists(filepath.Join(dir, legacyFile+".migrated")) {
		t.Error("expected chats.json to be renamed to chats.json.migrated")
	}
}
