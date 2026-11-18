package msgstore

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/codebyoketch/wa-cli/internal/localdb"
)

// The tests in this file use real second and third processes, because
// that is the situation they are about: `wa chat send` in one terminal
// while the TUI runs in another. The "other process" is this same test
// binary, started again with helperEnv set; TestMain sees the variable
// and runs runHelper instead of the tests.

const (
	helperEnv     = "WA_MSGSTORE_HELPER" // what the helper should do
	helperDirEnv  = "WA_MSGSTORE_HELPER_DIR"
	helperNameEnv = "WA_MSGSTORE_HELPER_NAME"

	helperChat      = "shared@g.us"
	helperReactions = 40
)

// runHelper is the body of a helper process. It returns the exit code.
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

	switch os.Getenv(helperEnv) {
	case "first-use":
		// The very first use of the store: creates the schema, imports
		// messages.json, then adds a message of its own.
		if err := s.Append(Message{ID: "from-" + name, ChatJID: helperChat, Timestamp: 5}); err != nil {
			fmt.Fprintln(os.Stderr, "Append:", err)
			return 1
		}
	case "react":
		// Read-modify-write on one shared row, many times.
		for i := 0; i < helperReactions; i++ {
			sender := fmt.Sprintf("%s-%d", name, i)
			found, err := s.SetReaction(helperChat, "target", Reaction{SenderJID: sender, Emoji: "👍"})
			if err != nil || !found {
				fmt.Fprintf(os.Stderr, "SetReaction: found=%v err=%v\n", found, err)
				return 1
			}
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

// runHelpers starts n helper processes in the given mode on dir, releases
// them together, and waits for them to finish successfully.
func runHelpers(t *testing.T, dir, mode string, n int) {
	t.Helper()
	type helper struct {
		cmd  *exec.Cmd
		name string
		done chan error
		out  []byte
	}
	helpers := make([]*helper, n)
	for i := range helpers {
		h := &helper{name: fmt.Sprint("p", i), done: make(chan error, 1)}
		// -test.run=^$ matches no test; TestMain takes over before that
		// matters anyway.
		h.cmd = exec.Command(os.Args[0], "-test.run=^$")
		h.cmd.Env = append(os.Environ(), helperEnv+"="+mode, helperDirEnv+"="+dir, helperNameEnv+"="+h.name)
		go func() {
			out, err := h.cmd.CombinedOutput()
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
}

func TestTwoProcesses_FirstUseAtTheSameMoment(t *testing.T) {
	// Nothing has opened this data dir yet: the helpers race to create
	// the database and its schema, and to import the old file.
	dir := legacyDir(t, oldMessagesJSON)
	const processes = 3

	runHelpers(t, dir, "first-use", processes)

	s := New(dir)
	// Imported exactly once: 3 messages in this chat, not 6 or 9.
	if n, err := s.Count("111@s.whatsapp.net"); err != nil || n != 3 {
		t.Fatalf("Count = %d, %v; want 3", n, err)
	}
	if got := mustMeta(t, s, "legacy_messages_imported"); got != "1" {
		t.Errorf("legacy_messages_imported = %q, want \"1\"", got)
	}
	if got := mustMeta(t, s, "legacy_messages_error"); got != "" {
		t.Errorf("legacy_messages_error = %q, want unset", got)
	}
	if fileExists(filepath.Join(dir, legacyFile)) || !fileExists(filepath.Join(dir, legacyFile+".migrated")) {
		t.Error("expected messages.json to be renamed to messages.json.migrated")
	}
	// And every process's own write is there.
	if n, _ := s.Count(helperChat); n != processes {
		t.Errorf("%d of the helpers' own messages stored, want %d", n, processes)
	}
}

func TestTwoProcesses_UpdatesAreNotLost(t *testing.T) {
	dir := t.TempDir()
	t.Cleanup(func() { localdb.Close(dir) })
	s := New(dir)
	if err := s.Append(Message{ID: "target", ChatJID: helperChat, Timestamp: 1}); err != nil {
		t.Fatal(err)
	}
	const processes = 3

	// This process joins in while the helpers run.
	mine := make(chan error, 1)
	go func() {
		for i := 0; i < helperReactions; i++ {
			if _, err := s.SetReaction(helperChat, "target", Reaction{SenderJID: fmt.Sprint("test-", i), Emoji: "🎉"}); err != nil {
				mine <- err
				return
			}
		}
		mine <- nil
	}()
	runHelpers(t, dir, "react", processes)
	if err := <-mine; err != nil {
		t.Fatalf("SetReaction in the test process: %v", err)
	}

	// With the old JSON store, whoever saved last won and the others'
	// reactions vanished. Now every single one must be there.
	got := len(mustGet(t, s, helperChat, "target").Reactions)
	if want := (processes + 1) * helperReactions; got != want {
		t.Fatalf("%d reactions stored, want %d", got, want)
	}
}
