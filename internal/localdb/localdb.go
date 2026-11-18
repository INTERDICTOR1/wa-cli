// Package localdb opens wa-cli's own SQLite database, <dataDir>/wa.db,
// which holds the chat list (internal/chatstore) and the message history
// (internal/msgstore).
//
// These used to be two JSON files that were rewritten in full on every
// incoming message and re-read in full on every screen refresh. A database
// fixes the three problems that caused:
//
//   - cost: one message now writes one small row instead of megabytes, and
//     the UI reads only the page of history it shows;
//   - duplicates: the tables have unique keys, so the same message can't be
//     stored twice;
//   - lost writes: SQLite locks the file properly, so `wa chat send` in one
//     terminal and the TUI in another no longer overwrite each other.
//
// (whatsmeow keeps its own, separate database in session.db - see
// internal/store.)
//
// Memory is kept small on purpose: at most two connections, each with a
// 1 MB page cache, and reads fetch only the rows that were asked for (a
// page of a chat, not its whole history).
package localdb

import (
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	// Pure-Go SQLite driver - registers itself as "sqlite".
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// FileName is the database file inside the data dir.
const FileName = "wa.db"

// migrations[i] holds the statements that take the schema from version i
// to version i+1. The version a database file has reached is stored in the
// file itself (PRAGMA user_version). To change the schema, append a step -
// never edit an existing one, because databases that already ran it won't
// run it again.
var migrations = [][]string{
	// 0 -> 1: the first schema.
	{
		// One row per chat. WITHOUT ROWID stores the row inside the
		// primary-key index itself, which is smaller and faster for a
		// table that is always looked up by jid.
		`CREATE TABLE chats (
			jid                  TEXT PRIMARY KEY,
			name                 TEXT    NOT NULL DEFAULT '',
			is_group             INTEGER NOT NULL DEFAULT 0,
			last_message_at      INTEGER NOT NULL DEFAULT 0,
			last_message_preview TEXT    NOT NULL DEFAULT '',
			unread_count         INTEGER NOT NULL DEFAULT 0,
			muted                INTEGER NOT NULL DEFAULT 0,
			pinned               INTEGER NOT NULL DEFAULT 0,
			archived             INTEGER NOT NULL DEFAULT 0,
			muted_until          INTEGER NOT NULL DEFAULT 0,
			marked_unread        INTEGER NOT NULL DEFAULT 0,
			draft                TEXT    NOT NULL DEFAULT '',
			ephemeral            INTEGER NOT NULL DEFAULT 0
		) WITHOUT ROWID`,

		// One row per message. seq is handed out by SQLite in insertion
		// order and never reused (AUTOINCREMENT); together with timestamp
		// it gives every message a fixed place in its chat.
		//
		// UNIQUE (id, chat_jid) is what stops duplicates. id comes first so
		// the same index also answers "find this message ID in any chat".
		//
		// raw_proto holds the decoded protobuf bytes (the Go struct carries
		// them base64-encoded, which is a third bigger).
		`CREATE TABLE messages (
			seq        INTEGER PRIMARY KEY AUTOINCREMENT,
			chat_jid   TEXT    NOT NULL,
			id         TEXT    NOT NULL,
			sender_jid TEXT    NOT NULL DEFAULT '',
			timestamp  INTEGER NOT NULL,
			text       TEXT    NOT NULL DEFAULT '',
			from_me    INTEGER NOT NULL DEFAULT 0,
			media_type TEXT    NOT NULL DEFAULT '',
			raw_proto  BLOB,
			status     INTEGER NOT NULL DEFAULT 0,
			edited     INTEGER NOT NULL DEFAULT 0,
			deleted    INTEGER NOT NULL DEFAULT 0,
			hd         INTEGER NOT NULL DEFAULT 0,
			reactions  TEXT    NOT NULL DEFAULT '',
			poll_votes TEXT    NOT NULL DEFAULT '',
			UNIQUE (id, chat_jid)
		)`,

		// The index every "show me this chat's history" query walks. SQLite
		// adds the row's seq to the end of each index entry by itself, so
		// the entries are in exactly the order we display: chat, then
		// timestamp, then seq.
		`CREATE INDEX messages_by_time ON messages (chat_jid, timestamp)`,

		// Small named values: migration flags and the like.
		`CREATE TABLE meta (
			key   TEXT PRIMARY KEY,
			value TEXT NOT NULL
		) WITHOUT ROWID`,
	},
}

var (
	// mu guards handles.
	mu sync.Mutex
	// handles holds the open database of each data dir, keyed by the
	// dir's absolute path, so every store in the process shares one.
	handles = map[string]*sql.DB{}

	// writeMu makes the writers in this process take turns. SQLite allows
	// only one writer at a time anyway; queueing here instead of inside
	// SQLite means a waiting writer doesn't sit on one of our two
	// connections, so a reader (the UI) always finds a free one, even
	// while a big history batch is being saved. Other processes are held
	// off by SQLite's own file lock (see dsn).
	//
	// It is one lock for the whole process rather than one per database,
	// because a wa process only ever has one data dir open.
	writeMu sync.Mutex
)

func init() {
	// wa_lower(text) lower-cases text the way Go does. SQLite's own
	// lower() and LIKE only know the letters A-Z, so searching for "émile"
	// would miss "Émile"; with this function, searches and name sorting
	// behave exactly like strings.ToLower in Go code.
	sqlite.MustRegisterDeterministicScalarFunction("wa_lower", 1,
		func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
			if s, ok := args[0].(string); ok {
				return strings.ToLower(s), nil
			}
			return args[0], nil
		})
}

// Open returns the process-wide handle for <dataDir>/wa.db, creating the
// directory (0700), file and schema as needed. Calling it again for the
// same directory returns the same handle, so it is cheap to call before
// every operation.
func Open(dataDir string) (*sql.DB, error) {
	dir, err := filepath.Abs(dataDir)
	if err != nil {
		return nil, fmt.Errorf("resolving data dir: %w", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if db, ok := handles[dir]; ok {
		return db, nil
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("creating data dir: %w", err)
	}
	db, err := openFile(filepath.Join(dir, FileName))
	if err != nil {
		return nil, err
	}
	handles[dir] = db
	return db, nil
}

// openFile opens the database file at path, creating it and bringing its
// schema up to date as needed.
func openFile(path string) (*sql.DB, error) {
	// Create the file ourselves so it is private to the user (SQLite
	// would create it readable by everyone, and gives its helper files
	// the same permissions as this one).
	if f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600); err == nil {
		f.Close()
	}

	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	// Two connections: one for whoever is writing, one for readers. Each
	// has its own page cache, so this is also what bounds memory.
	db.SetMaxOpenConns(2)
	db.SetMaxIdleConns(2)

	// Normally a locked database is waited for (busy_timeout, below). The
	// one exception is switching a brand-new file to WAL mode: if two wa
	// processes start on a fresh data dir at the same moment, SQLite tells
	// one of them "database is locked" at once. The other process needs
	// only a few milliseconds, so try again for as long as busy_timeout
	// would have waited.
	deadline := time.Now().Add(10 * time.Second)
	for {
		err = migrate(db)
		if err == nil || !isBusy(err) || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("preparing %s: %w", path, err)
	}
	return db, nil
}

// isBusy reports whether err is SQLite's "database is locked".
func isBusy(err error) bool {
	var sqliteErr *sqlite.Error
	// The low byte is the basic error code; the rest says which kind of
	// "busy" it was.
	return errors.As(err, &sqliteErr) && sqliteErr.Code()&0xff == sqlite3.SQLITE_BUSY
}

// dsn builds the connection string for the database file at path. Every
// connection the pool opens runs these settings first:
//
//   - busy_timeout: when another process holds the write lock, wait up to
//     10 s for it instead of failing straight away.
//   - journal_mode(WAL): readers and one writer can work at the same time.
//   - synchronous(NORMAL): the recommended partner of WAL - a power cut can
//     lose the last few writes but never corrupts the file, and writes
//     don't wait for the disk each time.
//   - foreign_keys(1): off by default in SQLite; on, in case a later
//     schema step adds references between tables.
//   - cache_size(-1024): at most 1 MB of pages cached per connection.
//   - journal_size_limit: shrink the write-ahead log back to 4 MB after a
//     burst of writes.
//   - _txlock=immediate: db.Begin takes the write lock at once ("BEGIN
//     IMMEDIATE"). A read-then-write transaction is therefore never
//     interleaved with another process's write - which is what makes
//     operations like chatstore.Merge or msgstore.SetReaction atomic.
func dsn(path string) string {
	// SQLite reads the name as a URI, where these three characters have a
	// special meaning; escape them in case the path contains one.
	escaped := strings.NewReplacer("%", "%25", "?", "%3f", "#", "%23").Replace(path)
	return "file:" + escaped +
		"?_txlock=immediate" +
		"&_pragma=busy_timeout(10000)" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_pragma=foreign_keys(1)" +
		"&_pragma=cache_size(-1024)" +
		"&_pragma=journal_size_limit(4194304)"
}

// migrate brings the schema up to date.
func migrate(db *sql.DB) error {
	want := len(migrations)

	// The usual case: nothing to do, and no need to queue for the write
	// lock to find that out.
	var have int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&have); err != nil {
		return err
	}
	if have == want {
		return nil
	}

	return Update(db, func(tx *sql.Tx) error {
		// Look again now that we hold the write lock: another process
		// starting at the same moment may have just done the work.
		if err := tx.QueryRow(`PRAGMA user_version`).Scan(&have); err != nil {
			return err
		}
		if have > want {
			return fmt.Errorf("database has schema version %d but this wa only knows up to %d - it was written by a newer wa", have, want)
		}
		for version := have; version < want; version++ {
			for _, statement := range migrations[version] {
				if _, err := tx.Exec(statement); err != nil {
					return fmt.Errorf("schema step %d: %w", version+1, err)
				}
			}
		}
		// PRAGMA doesn't take "?" parameters; want is our own number.
		_, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, want))
		return err
	})
}

// Close closes the handle for dataDir (no-op if not open). Tests need it:
// Windows can't delete an open file when t.TempDir() is cleaned up. A
// later Open simply opens the database again.
func Close(dataDir string) error {
	dir, err := filepath.Abs(dataDir)
	if err != nil {
		return fmt.Errorf("resolving data dir: %w", err)
	}

	mu.Lock()
	defer mu.Unlock()
	db, ok := handles[dir]
	if !ok {
		return nil
	}
	delete(handles, dir)
	return db.Close()
}

// CloseAll closes every open handle.
func CloseAll() {
	mu.Lock()
	defer mu.Unlock()
	for dir, db := range handles {
		db.Close()
		delete(handles, dir)
	}
}

// Exec runs one statement that changes data. Use it (rather than db.Exec)
// for every write, so writers take turns - see writeMu.
func Exec(db *sql.DB, query string, args ...any) (sql.Result, error) {
	writeMu.Lock()
	defer writeMu.Unlock()
	return db.Exec(query, args...)
}

// Update runs fn inside one write transaction: either everything fn did
// is saved (fn returned nil) or nothing is. The transaction holds the
// database's write lock from the start, so what fn reads can't change
// under it - not even from another process.
//
// Inside fn, use only tx - never the *sql.DB, and never a store method.
func Update(db *sql.DB, fn func(tx *sql.Tx) error) error {
	writeMu.Lock()
	defer writeMu.Unlock()

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	// Does nothing once Commit has succeeded.
	defer tx.Rollback()

	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// Meta returns the value stored under key in the meta table, "" if unset.
// q is a *sql.DB or, inside Update, the *sql.Tx.
func Meta(q interface {
	QueryRow(query string, args ...any) *sql.Row
}, key string) (string, error) {
	var value string
	err := q.QueryRow(`SELECT value FROM meta WHERE key = ?`, key).Scan(&value)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return value, err
}

// SetMeta stores value under key in the meta table.
func SetMeta(tx *sql.Tx, key, value string) error {
	_, err := tx.Exec(`INSERT INTO meta (key, value) VALUES (?, ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}
