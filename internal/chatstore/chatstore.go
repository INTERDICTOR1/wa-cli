// Package chatstore persists a local index of WhatsApp chats (JID, name,
// last message preview/time, unread count, and flags such as muted or
// pinned) so `wa chat` commands and the TUI have something to read across
// separate invocations, without needing an always-on connection.
//
// It's populated by internal/whatsapp's event handlers (message and
// history-sync events). The chats live in the "chats" table of
// <dataDir>/wa.db - see internal/localdb for why this is SQLite and no
// longer a JSON file. In short: every change is now one small row write
// instead of a rewrite of the whole file, and each method is a single
// atomic step, so the TUI and a `wa chat send` running at the same time
// can't undo each other's changes.
//
// Every method is safe to call from several goroutines, and from several
// wa processes at once.
package chatstore

import (
	"database/sql"
	"fmt"
	"strings"
	"sync"

	"github.com/codebyoketch/wa-cli/internal/localdb"
)

// Chat is one conversation's locally cached metadata.
type Chat struct {
	JID                string `json:"jid"`
	Name               string `json:"name"`
	IsGroup            bool   `json:"isGroup"`
	LastMessageAt      int64  `json:"lastMessageAt"` // unix millis, 0 if unknown
	LastMessagePreview string `json:"lastMessagePreview"`
	UnreadCount        int    `json:"unreadCount"`
	// Muted suppresses desktop notifications for this chat. Deliberately
	// not part of Upsert's normal merge logic — only SetMuted changes
	// it, so ordinary message ingestion never accidentally un-mutes a
	// chat (a plain bool has no way to distinguish "explicitly wants
	// false" from "caller didn't consider this field at all").
	Muted bool `json:"muted,omitempty"`

	// The fields below are, like Muted, never touched by Upsert; each has
	// its own setter.

	// Pinned keeps the chat at the top of the list (synced with the phone).
	Pinned bool `json:"pinned,omitempty"`
	// Archived hides the chat from the main list (synced with the phone).
	Archived bool `json:"archived,omitempty"`
	// MutedUntil is the phone's mute for this chat: unix millis, -1 for
	// "always", 0 for not muted.
	MutedUntil int64 `json:"mutedUntil,omitempty"`
	// MarkedUnread is the manual "mark as unread" flag.
	MarkedUnread bool `json:"markedUnread,omitempty"`
	// Draft is unsent text typed into this chat.
	Draft string `json:"draft,omitempty"`
	// Ephemeral is the chat's disappearing-messages timer in seconds, 0
	// when off.
	Ephemeral int64 `json:"ephemeral,omitempty"`
}

// Store is the collection of Chats kept in <dataDir>/wa.db, keyed by JID.
// It holds no data itself, so creating one is free and several can point
// at the same directory - they all see the same chats.
type Store struct {
	dir string

	// mu guards legacyChecked.
	mu sync.Mutex
	// legacyChecked is set once this Store has looked for an old
	// chats.json to import (see legacy.go).
	legacyChecked bool
}

// New creates a Store for the database under dataDir. Nothing is opened
// yet: that happens on the first method call, which is also the one that
// reports a problem opening it.
func New(dataDir string) *Store {
	return &Store{dir: dataDir}
}

// db returns the shared database handle, opening it if needed. The first
// call also imports an old chats.json, if there is one.
func (s *Store) db() (*sql.DB, error) {
	db, err := localdb.Open(s.dir)
	if err != nil {
		return nil, fmt.Errorf("opening chat store: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.legacyChecked {
		importLegacy(db, s.dir)
		s.legacyChecked = true
	}
	return db, nil
}

// Close closes the database handle shared by every store on this data
// dir (the message store included). Using a store afterwards just opens
// it again. Tests must call it before their temp dir is removed: Windows
// can't delete an open file.
func (s *Store) Close() error {
	return localdb.Close(s.dir)
}

// exec runs one statement that changes data.
func (s *Store) exec(query string, args ...any) error {
	db, err := s.db()
	if err != nil {
		return err
	}
	if _, err := localdb.Exec(db, query, args...); err != nil {
		return fmt.Errorf("updating chat store: %w", err)
	}
	return nil
}

// ---- reading ----

// columns lists every column of the chats table, in the order chatArgs
// supplies them and queryChats reads them.
const columns = `jid, name, is_group, last_message_at, last_message_preview, unread_count,
	muted, pinned, archived, muted_until, marked_unread, draft, ephemeral`

// chatArgs returns c's fields in the order of columns.
func chatArgs(c Chat) []any {
	return []any{
		c.JID, c.Name, c.IsGroup, c.LastMessageAt, c.LastMessagePreview, c.UnreadCount,
		c.Muted, c.Pinned, c.Archived, c.MutedUntil, c.MarkedUnread, c.Draft, c.Ephemeral,
	}
}

// querier is the reading half of *sql.DB and *sql.Tx.
type querier interface {
	Query(query string, args ...any) (*sql.Rows, error)
}

// queryChats runs a SELECT of columns and returns the rows as Chats. It
// reads everything and closes the query before returning.
func queryChats(q querier, query string, args ...any) ([]Chat, error) {
	rows, err := q.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var chats []Chat
	for rows.Next() {
		var c Chat
		err := rows.Scan(&c.JID, &c.Name, &c.IsGroup, &c.LastMessageAt, &c.LastMessagePreview, &c.UnreadCount,
			&c.Muted, &c.Pinned, &c.Archived, &c.MutedUntil, &c.MarkedUnread, &c.Draft, &c.Ephemeral)
		if err != nil {
			return nil, err
		}
		chats = append(chats, c)
	}
	return chats, rows.Err()
}

// query is queryChats on the store's database.
func (s *Store) query(query string, args ...any) ([]Chat, error) {
	db, err := s.db()
	if err != nil {
		return nil, err
	}
	chats, err := queryChats(db, query, args...)
	if err != nil {
		return nil, fmt.Errorf("reading chat store: %w", err)
	}
	return chats, nil
}

// getChat returns the chat for jid as seen by q, and whether it exists.
func getChat(q querier, jid string) (Chat, bool, error) {
	chats, err := queryChats(q, `SELECT `+columns+` FROM chats WHERE jid = ?`, jid)
	if err != nil || len(chats) == 0 {
		return Chat{}, false, err
	}
	return chats[0], true, nil
}

// Get returns the chat for jid, and whether it was found.
func (s *Store) Get(jid string) (Chat, bool, error) {
	db, err := s.db()
	if err != nil {
		return Chat{}, false, err
	}
	c, ok, err := getChat(db, jid)
	if err != nil {
		return Chat{}, false, fmt.Errorf("reading chat store: %w", err)
	}
	return c, ok, nil
}

// listOrder sorts by most recent activity first. Chats with the same
// LastMessageAt - in practice the ones with no known activity, which all
// have 0 and so come last - are ordered by name, ignoring case (wa_lower
// is localdb's Go-style lower-casing), and finally by JID so the order is
// the same every time.
const listOrder = ` ORDER BY last_message_at DESC, wa_lower(name), jid`

// List returns all chats sorted by most recent activity first. Chats with
// no known LastMessageAt sort last, alphabetically by name.
func (s *Store) List() ([]Chat, error) {
	return s.query(`SELECT ` + columns + ` FROM chats` + listOrder)
}

// Search returns chats whose name or JID contains query, case-insensitive,
// sorted the same way as List.
func (s *Store) Search(query string) ([]Chat, error) {
	// instr(a, b) > 0 is SQL for strings.Contains(a, b).
	needle := strings.ToLower(strings.TrimSpace(query))
	return s.query(`SELECT `+columns+` FROM chats
		WHERE instr(wa_lower(name), ?) > 0 OR instr(wa_lower(jid), ?) > 0`+listOrder,
		needle, needle)
}

// ---- Upsert and the counters ----

// upsertSQL inserts a chat, or merges it into the existing row. In the
// merge, "chats.x" is the stored value and "excluded.x" the one passed
// in. Columns not listed after DO UPDATE SET (muted, pinned, ...) are
// left alone.
const upsertSQL = `INSERT INTO chats (` + columns + `)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT (jid) DO UPDATE SET
		name = CASE WHEN excluded.name = '' THEN chats.name ELSE excluded.name END,
		last_message_preview = CASE WHEN excluded.last_message_at > chats.last_message_at
			THEN excluded.last_message_preview ELSE chats.last_message_preview END,
		last_message_at = max(chats.last_message_at, excluded.last_message_at),
		is_group = chats.is_group OR excluded.is_group,
		unread_count = CASE WHEN excluded.unread_count = 0
			THEN chats.unread_count ELSE excluded.unread_count END`

// Upsert inserts or updates a chat record. Fields left zero-valued on an
// update do not overwrite existing values — callers can pass partial
// updates without first reading the record:
//
//   - an empty Name keeps the stored name;
//   - LastMessageAt and LastMessagePreview only ever move forward: a
//     history batch with older messages arriving after live ones must not
//     push the chat down the list;
//   - IsGroup, once true, stays true;
//   - an UnreadCount of 0 keeps the stored count (use MarkRead or
//     SetUnread to clear it);
//   - Muted, Pinned, Archived, MutedUntil, MarkedUnread, Draft and
//     Ephemeral of an existing chat are never changed here - each has its
//     own setter.
//
// A chat that isn't stored yet is saved exactly as given, all fields.
//
// The merge happens inside SQLite as one statement, so two Upserts at the
// same moment - even from two processes - can't lose each other's update.
func (s *Store) Upsert(c Chat) error {
	return s.exec(upsertSQL, chatArgs(c)...)
}

// IncrementUnread bumps jid's unread count by one, creating a bare record
// if it doesn't exist yet (a subsequent Upsert fills in name/preview).
func (s *Store) IncrementUnread(jid string) error {
	return s.exec(`INSERT INTO chats (jid, unread_count) VALUES (?, 1)
		ON CONFLICT (jid) DO UPDATE SET unread_count = unread_count + 1`, jid)
}

// SetMuted sets jid's muted flag, creating a bare record if it doesn't
// exist yet (a subsequent Upsert fills in name/preview). This is the
// only way Muted ever changes — see the field comment on Chat.
func (s *Store) SetMuted(jid string, muted bool) error {
	return s.exec(`INSERT INTO chats (jid, muted) VALUES (?, ?)
		ON CONFLICT (jid) DO UPDATE SET muted = excluded.muted`, jid, muted)
}

// MarkRead zeroes the unread count for jid and clears its manual "marked
// as unread" flag. No-op if jid isn't known.
func (s *Store) MarkRead(jid string) error {
	return s.exec(`UPDATE chats SET unread_count = 0, marked_unread = 0 WHERE jid = ?`, jid)
}

// ---- setters ----
//
// Each of these changes one thing about a chat that is already stored,
// and does nothing if jid isn't known.

// set stores value in one column of jid's row. column is always one of
// the names written in this file, never text from outside, so building
// the statement with it is safe.
func (s *Store) set(jid, column string, value any) error {
	return s.exec(`UPDATE chats SET `+column+` = ? WHERE jid = ?`, value, jid)
}

// SetName renames the chat. Unlike Upsert it always overwrites, even
// with an empty name - use it when the new name is known to be right.
func (s *Store) SetName(jid, name string) error {
	return s.set(jid, "name", name)
}

// SetPinned sets whether the chat is pinned to the top of the list.
func (s *Store) SetPinned(jid string, on bool) error {
	return s.set(jid, "pinned", on)
}

// SetArchived sets whether the chat is archived.
func (s *Store) SetArchived(jid string, on bool) error {
	return s.set(jid, "archived", on)
}

// SetMutedUntil records the phone's mute for the chat: unix millis, -1
// for "always", 0 for not muted.
func (s *Store) SetMutedUntil(jid string, until int64) error {
	return s.set(jid, "muted_until", until)
}

// SetMarkedUnread sets the manual "mark as unread" flag (MarkRead clears
// it again).
func (s *Store) SetMarkedUnread(jid string, on bool) error {
	return s.set(jid, "marked_unread", on)
}

// SetDraft saves the unsent text typed into the chat; "" clears it.
func (s *Store) SetDraft(jid, draft string) error {
	return s.set(jid, "draft", draft)
}

// SetEphemeral records the chat's disappearing-messages timer in
// seconds, 0 for off.
func (s *Store) SetEphemeral(jid string, seconds int64) error {
	return s.set(jid, "ephemeral", seconds)
}

// SetUnread sets the unread count to exactly n (Upsert can't lower it,
// and IncrementUnread only adds one).
func (s *Store) SetUnread(jid string, n int) error {
	if n < 0 {
		n = 0
	}
	return s.set(jid, "unread_count", n)
}

// SetLastMessage sets the chat's last-message time and preview no matter
// what is stored - unlike Upsert it can move them backwards, which is
// needed when the newest message is deleted or edited.
func (s *Store) SetLastMessage(jid string, at int64, preview string) error {
	return s.exec(`UPDATE chats SET last_message_at = ?, last_message_preview = ?
		WHERE jid = ?`, at, preview, jid)
}

// ---- merging and deleting ----

// Merge folds the chat fromJID into toJID and deletes fromJID - for when
// one conversation turns out to be known under two JIDs. Nothing happens
// if fromJID isn't known.
//
// If toJID isn't known either, it becomes a copy of fromJID under the
// new JID. Otherwise toJID keeps what it has and takes over what it
// lacks:
//
//   - Name, Draft, Ephemeral: toJID's, unless empty;
//   - LastMessageAt and its preview: whichever chat's is newer;
//   - UnreadCount: the two added up;
//   - Muted, Pinned, Archived, MarkedUnread, IsGroup: on if either was;
//   - MutedUntil: -1 ("always") if either was, else the later time.
//
// The whole fold is one transaction, so no other writer can change either
// chat halfway through. Messages are moved separately: see
// msgstore.MoveChat.
func (s *Store) Merge(fromJID, toJID string) error {
	if fromJID == toJID {
		return nil
	}
	db, err := s.db()
	if err != nil {
		return err
	}
	err = localdb.Update(db, func(tx *sql.Tx) error {
		from, ok, err := getChat(tx, fromJID)
		if err != nil || !ok {
			return err
		}
		to, ok, err := getChat(tx, toJID)
		if err != nil {
			return err
		}
		if ok {
			to = merged(from, to)
		} else {
			to = from
			to.JID = toJID
		}

		// REPLACE overwrites the whole row for toJID, or creates it.
		if _, err := tx.Exec(`REPLACE INTO chats (`+columns+`)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, chatArgs(to)...); err != nil {
			return err
		}
		_, err = tx.Exec(`DELETE FROM chats WHERE jid = ?`, fromJID)
		return err
	})
	if err != nil {
		return fmt.Errorf("merging chats: %w", err)
	}
	return nil
}

// merged returns the chat "to" with the chat "from" folded into it,
// following the rules described on Merge.
func merged(from, to Chat) Chat {
	if to.Name == "" {
		to.Name = from.Name
	}
	if to.Draft == "" {
		to.Draft = from.Draft
	}
	if to.Ephemeral == 0 {
		to.Ephemeral = from.Ephemeral
	}
	if from.LastMessageAt > to.LastMessageAt {
		to.LastMessageAt = from.LastMessageAt
		to.LastMessagePreview = from.LastMessagePreview
	}
	to.UnreadCount += from.UnreadCount
	to.Muted = to.Muted || from.Muted
	to.Pinned = to.Pinned || from.Pinned
	to.Archived = to.Archived || from.Archived
	to.MarkedUnread = to.MarkedUnread || from.MarkedUnread
	to.IsGroup = to.IsGroup || from.IsGroup
	if to.MutedUntil == -1 || from.MutedUntil == -1 {
		to.MutedUntil = -1
	} else if from.MutedUntil > to.MutedUntil {
		to.MutedUntil = from.MutedUntil
	}
	return to
}

// Delete removes the chat record for jid (its messages, if any, are in
// msgstore and are not touched). Deleting an unknown chat is not an
// error.
func (s *Store) Delete(jid string) error {
	return s.exec(`DELETE FROM chats WHERE jid = ?`, jid)
}
