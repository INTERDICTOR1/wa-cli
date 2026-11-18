package localdb

import (
	"database/sql"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// openTemp opens a database in a fresh temp dir and closes it again when
// the test ends (Windows can't remove the dir while the file is open).
func openTemp(t *testing.T) (*sql.DB, string) {
	t.Helper()
	dir := t.TempDir()
	db, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { Close(dir) })
	return db, dir
}

func TestOpen_CreatesFileAndSchema(t *testing.T) {
	db, dir := openTemp(t)

	if _, err := os.Stat(filepath.Join(dir, FileName)); err != nil {
		t.Fatalf("expected %s to exist: %v", FileName, err)
	}
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != len(migrations) {
		t.Fatalf("user_version = %d, want %d", version, len(migrations))
	}
	for _, table := range []string{"chats", "messages", "meta"} {
		var n int
		err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&n)
		if err != nil || n != 1 {
			t.Fatalf("table %s missing (n=%d, err=%v)", table, n, err)
		}
	}
}

func TestOpen_CreatesMissingDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "not", "there", "yet")
	if _, err := Open(dir); err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { Close(dir) })
	if _, err := os.Stat(filepath.Join(dir, FileName)); err != nil {
		t.Fatalf("expected database file: %v", err)
	}
}

func TestOpen_SameDirSharesOneHandle(t *testing.T) {
	db, dir := openTemp(t)

	again, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if again != db {
		t.Fatal("second Open returned a different handle")
	}
	// A different spelling of the same directory is still the same one.
	spelled, err := Open(filepath.Join(dir, "sub", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if spelled != db {
		t.Fatal("Open with an unclean path returned a different handle")
	}
}

func TestOpen_AppliesSettings(t *testing.T) {
	db, _ := openTemp(t)

	want := map[string]string{
		"journal_mode": "wal",
		"synchronous":  "1", // NORMAL
		"cache_size":   "-1024",
		"busy_timeout": "10000",
	}
	for pragma, value := range want {
		var got string
		if err := db.QueryRow(`PRAGMA ` + pragma).Scan(&got); err != nil {
			t.Fatalf("PRAGMA %s: %v", pragma, err)
		}
		if got != value {
			t.Errorf("PRAGMA %s = %q, want %q", pragma, got, value)
		}
	}
	if n := db.Stats().MaxOpenConnections; n != 2 {
		t.Errorf("MaxOpenConnections = %d, want 2", n)
	}
}

func TestOpen_PathWithSpecialCharacters(t *testing.T) {
	// % ? and # mean something in SQLite's file: names; the database must
	// still land in exactly this directory. ("?" can't be in a Windows
	// file name, so it is left out of the test.)
	dir := filepath.Join(t.TempDir(), "odd #1 100%25 dir")
	db, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { Close(dir) })

	if err := Update(db, func(tx *sql.Tx) error { return SetMeta(tx, "k", "v") }); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, FileName))
	if err != nil || info.Size() == 0 {
		t.Fatalf("database not written where expected: info=%v err=%v", info, err)
	}
}

func TestClose_ThenOpenAgainKeepsData(t *testing.T) {
	db, dir := openTemp(t)
	if err := Update(db, func(tx *sql.Tx) error { return SetMeta(tx, "k", "v") }); err != nil {
		t.Fatal(err)
	}

	if err := Close(dir); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := Close(dir); err != nil {
		t.Fatalf("second Close should be a no-op: %v", err)
	}
	if err := db.Ping(); err == nil {
		t.Fatal("expected the old handle to be closed")
	}

	reopened, err := Open(dir)
	if err != nil {
		t.Fatalf("Open after Close: %v", err)
	}
	if got, err := Meta(reopened, "k"); err != nil || got != "v" {
		t.Fatalf("Meta after reopen = %q, %v; want \"v\"", got, err)
	}
}

func TestCloseAll(t *testing.T) {
	a, _ := openTemp(t)
	b, _ := openTemp(t)
	CloseAll()
	if a.Ping() == nil || b.Ping() == nil {
		t.Fatal("expected every handle to be closed")
	}
}

func TestOpen_RefusesNewerSchema(t *testing.T) {
	db, dir := openTemp(t)
	if _, err := db.Exec(`PRAGMA user_version = 999`); err != nil {
		t.Fatal(err)
	}
	Close(dir)

	if _, err := Open(dir); err == nil || !strings.Contains(err.Error(), "newer") {
		t.Fatalf("expected a \"newer wa\" error, got %v", err)
	}
}

// Several wa processes starting at the same moment on a data dir that
// has no database yet must all succeed. Separate openFile calls stand in
// for the processes: each gets its own connections, which share nothing
// but the file.
func TestOpenFile_ManyOpenersOfAFreshFile(t *testing.T) {
	for round := 0; round < 5; round++ {
		path := filepath.Join(t.TempDir(), FileName)

		const openers = 6
		dbs := make([]*sql.DB, openers)
		errs := make([]error, openers)
		var wg sync.WaitGroup
		for i := 0; i < openers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				dbs[i], errs[i] = openFile(path)
			}()
		}
		wg.Wait()

		for i, err := range errs {
			if err != nil {
				t.Errorf("round %d, opener %d: %v", round, i, err)
				continue
			}
			var version int
			if err := dbs[i].QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != len(migrations) {
				t.Errorf("round %d, opener %d: user_version = %d, err = %v", round, i, version, err)
			}
			dbs[i].Close()
		}
	}
}

func TestMeta(t *testing.T) {
	db, _ := openTemp(t)

	if got, err := Meta(db, "missing"); err != nil || got != "" {
		t.Fatalf("unset key = %q, %v; want \"\"", got, err)
	}
	set := func(value string) {
		t.Helper()
		if err := Update(db, func(tx *sql.Tx) error { return SetMeta(tx, "k", value) }); err != nil {
			t.Fatal(err)
		}
	}
	set("one")
	set("two") // overwrites
	if got, _ := Meta(db, "k"); got != "two" {
		t.Fatalf("Meta = %q, want \"two\"", got)
	}
}

func TestUpdate_ErrorRollsEverythingBack(t *testing.T) {
	db, _ := openTemp(t)
	boom := errors.New("boom")

	err := Update(db, func(tx *sql.Tx) error {
		if err := SetMeta(tx, "k", "v"); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("Update error = %v, want boom", err)
	}
	if got, _ := Meta(db, "k"); got != "" {
		t.Fatalf("change survived a failed Update: %q", got)
	}
}

// Two separate connection pools on one file behave like two processes:
// neither knows about the other's Go-side locks. Counting up from both at
// once only gives the right total if a transaction's read and write can't
// be separated by the other side's write.
func TestUpdate_IsAtomicAcrossSeparateHandles(t *testing.T) {
	db, dir := openTemp(t)
	other, err := sql.Open("sqlite", dsn(filepath.Join(dir, FileName)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { other.Close() })

	// Like Update, minus writeMu - the other "process" has its own.
	increment := func(db *sql.DB) error {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		defer tx.Rollback()
		var n int
		if err := tx.QueryRow(`SELECT count(*) FROM meta`).Scan(&n); err != nil {
			return err
		}
		var value int
		if n > 0 {
			if err := tx.QueryRow(`SELECT CAST(value AS INTEGER) FROM meta WHERE key = 'counter'`).Scan(&value); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(`REPLACE INTO meta (key, value) VALUES ('counter', ?)`, value+1); err != nil {
			return err
		}
		return tx.Commit()
	}

	const each = 200
	var wg sync.WaitGroup
	errs := make(chan error, 2*each)
	for _, handle := range []*sql.DB{db, other} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < each; i++ {
				if err := increment(handle); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("increment: %v", err)
	}

	if got, _ := Meta(db, "counter"); got != "400" {
		t.Fatalf("counter = %s, want 400 (updates were lost)", got)
	}
}

func TestWaLower_LowersLikeGo(t *testing.T) {
	db, _ := openTemp(t)
	var got string
	if err := db.QueryRow(`SELECT wa_lower('ÉMILE Zola ΑΒΓ')`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if want := strings.ToLower("ÉMILE Zola ΑΒΓ"); got != want {
		t.Fatalf("wa_lower = %q, want %q", got, want)
	}
}

// ---- ImportLegacy ----

// writeLegacy puts an "old state file" with the given content in dir.
func writeLegacy(t *testing.T, dir, content string) string {
	t.Helper()
	path := filepath.Join(dir, "old.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// loadAsMeta is a stand-in for a real importer: it stores the file's
// content under the meta key "loaded" and counts how often it ran.
func loadAsMeta(calls *int) func(tx *sql.Tx, file io.Reader) error {
	return func(tx *sql.Tx, file io.Reader) error {
		*calls++
		content, err := io.ReadAll(file)
		if err != nil {
			return err
		}
		return SetMeta(tx, "loaded", string(content))
	}
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func TestImportLegacy_ImportsOnceAndRenames(t *testing.T) {
	db, dir := openTemp(t)
	path := writeLegacy(t, dir, "first")
	calls := 0

	ImportLegacy(db, "things", path, loadAsMeta(&calls))

	if got, _ := Meta(db, "loaded"); got != "first" {
		t.Fatalf("loaded = %q, want \"first\"", got)
	}
	if got, _ := Meta(db, "legacy_things_imported"); got != "1" {
		t.Fatalf("legacy_things_imported = %q, want \"1\"", got)
	}
	if exists(path) || !exists(path+".migrated") {
		t.Fatal("expected the file to be renamed to .migrated")
	}

	// A new old-style file appearing later is not imported again, and
	// the existing .migrated backup is not overwritten by it.
	writeLegacy(t, dir, "second")
	ImportLegacy(db, "things", path, loadAsMeta(&calls))

	if calls != 1 {
		t.Fatalf("load ran %d times, want 1", calls)
	}
	if got, _ := Meta(db, "loaded"); got != "first" {
		t.Fatalf("loaded = %q after second run, want \"first\"", got)
	}
	backup, _ := os.ReadFile(path + ".migrated")
	if string(backup) != "first" || !exists(path) {
		t.Fatalf("backup = %q, later file still there = %v; want \"first\", true", backup, exists(path))
	}
}

func TestImportLegacy_NoFileDoesNothing(t *testing.T) {
	db, dir := openTemp(t)
	calls := 0

	ImportLegacy(db, "things", filepath.Join(dir, "old.json"), loadAsMeta(&calls))

	if calls != 0 {
		t.Fatal("load ran without a file")
	}
	if got, _ := Meta(db, "legacy_things_imported"); got != "" {
		t.Fatalf("flag set without a file: %q", got)
	}
}

func TestImportLegacy_FailureKeepsFileAndRecordsWhy(t *testing.T) {
	db, dir := openTemp(t)
	path := writeLegacy(t, dir, "damaged")

	ImportLegacy(db, "things", path, func(tx *sql.Tx, file io.Reader) error {
		// Half the work, then a failure: the half must not be kept.
		if err := SetMeta(tx, "loaded", "half"); err != nil {
			return err
		}
		return errors.New("bad file")
	})

	if got, _ := Meta(db, "loaded"); got != "" {
		t.Fatalf("partial import was kept: %q", got)
	}
	if got, _ := Meta(db, "legacy_things_imported"); got != "" {
		t.Fatalf("flag set after a failed import: %q", got)
	}
	if got, _ := Meta(db, "legacy_things_error"); got != "bad file" {
		t.Fatalf("legacy_things_error = %q, want \"bad file\"", got)
	}
	if !exists(path) || exists(path+".migrated") {
		t.Fatal("a file that failed to import must stay where it is")
	}

	// Once the file is fixed, the next attempt imports it and clears the
	// recorded error.
	calls := 0
	ImportLegacy(db, "things", path, loadAsMeta(&calls))
	if got, _ := Meta(db, "loaded"); got != "damaged" {
		t.Fatalf("retry did not import: %q", got)
	}
	if got, _ := Meta(db, "legacy_things_error"); got != "" {
		t.Fatalf("error not cleared after a successful retry: %q", got)
	}
	if !exists(path + ".migrated") {
		t.Fatal("expected the file to be renamed after the successful retry")
	}
}

func TestImportLegacy_FinishesAnInterruptedRename(t *testing.T) {
	db, dir := openTemp(t)
	path := writeLegacy(t, dir, "content")
	// As if an earlier run imported the file but stopped before renaming.
	if err := Update(db, func(tx *sql.Tx) error { return SetMeta(tx, "legacy_things_imported", "1") }); err != nil {
		t.Fatal(err)
	}
	calls := 0

	ImportLegacy(db, "things", path, loadAsMeta(&calls))

	if calls != 0 {
		t.Fatal("imported a second time")
	}
	if exists(path) || !exists(path+".migrated") {
		t.Fatal("expected the leftover file to be renamed")
	}
}

func TestImportLegacy_ConcurrentCallersImportOnce(t *testing.T) {
	db, dir := openTemp(t)
	path := writeLegacy(t, dir, "content")

	var mu sync.Mutex
	calls := 0
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ImportLegacy(db, "things", path, func(tx *sql.Tx, file io.Reader) error {
				mu.Lock()
				calls++
				mu.Unlock()
				return nil
			})
		}()
	}
	wg.Wait()

	if calls != 1 {
		t.Fatalf("load ran %d times, want 1", calls)
	}
	if exists(path) || !exists(path+".migrated") {
		t.Fatal("expected the file to be renamed to .migrated")
	}
	if got, _ := Meta(db, "legacy_things_error"); got != "" {
		t.Fatalf("unexpected error recorded: %q", got)
	}
}
