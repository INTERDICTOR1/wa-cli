// Package msgstore persists the message history of every chat, so the TUI
// and `wa chat open` can show it, and `wa chat reply`/`wa chat forward`
// can reference a specific message by its number or its WhatsApp ID.
//
// The messages live in the "messages" table of <dataDir>/wa.db (see
// internal/localdb for why this is SQLite and no longer a JSON file). The
// things worth knowing when calling this package:
//
//   - Order. Unless a method says otherwise, messages come back "oldest
//     first": by Timestamp, and for equal timestamps by Seq, the number
//     the store gives each message as it is saved.
//   - Paging. Nothing here needs the whole history in memory: Latest
//     returns the newest page and Before the page older than a message you
//     already have. A chat with 100,000 messages costs the same to open as
//     one with 100.
//   - No duplicates. A chat holds at most one message per ID; saving the
//     same message again is a harmless no-op (see Insert and Put).
//   - Limits. Where a method takes a limit, 0 returns nothing and a
//     negative number means "no limit".
//   - Nothing is trimmed automatically. History grows until Prune is
//     called.
//
// Every method is safe to call from several goroutines, and from several
// wa processes at once.
package msgstore

import (
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/codebyoketch/wa-cli/internal/localdb"
)

// MaxPerChat and RetainFor described the window the old JSON store kept
// (the newest 50 messages per chat, plus everything from the last 48
// hours). The store no longer trims anything by itself - see Prune - and
// these remain only as defaults for callers that still refer to them.
const (
	MaxPerChat = 50
	RetainFor  = 48 * time.Hour
)

// Message is one stored message.
type Message struct {
	ID        string `json:"id"`
	ChatJID   string `json:"chatJid"`
	SenderJID string `json:"senderJid"`
	Timestamp int64  `json:"timestamp"` // unix millis
	Text      string `json:"text"`      // caption, for media messages
	FromMe    bool   `json:"fromMe"`
	// MediaType is "" for plain text, or one of "image", "video",
	// "audio", "document", "sticker".
	MediaType string `json:"mediaType,omitempty"`
	// RawProto is the base64-encoded serialized WhatsApp message content,
	// used to reconstruct quote/forward context when replying or
	// forwarding, and to redownload media. Empty if unavailable
	// (unsupported message type, or encoding failed).
	RawProto string `json:"rawProto,omitempty"`

	// Status is the delivery state of a message you sent.
	Status Status `json:"status,omitempty"`
	// Edited is set once the message's content was replaced by an edit.
	Edited bool `json:"edited,omitempty"`
	// Deleted is set when the message was deleted for everyone; its text
	// and content are cleared.
	Deleted bool `json:"deleted,omitempty"`
	// HD is set when an image's content was replaced by its HD version.
	HD bool `json:"hd,omitempty"`
	// Reactions holds the current reaction of each person who reacted.
	Reactions []Reaction `json:"reactions,omitempty"`
	// PollVotes holds each voter's current vote on a poll.
	PollVotes []PollVote `json:"pollVotes,omitempty"`
	// Seq is the storage order within the store, used with Timestamp as
	// the paging cursor. Assigned by the store; ignored on insert.
	Seq int64 `json:"-"`
}

// Status is the delivery state of a message you sent. Higher is further
// along; receipts only ever raise it.
type Status int

const (
	StatusNone      Status = iota // an incoming message, or unknown
	StatusFailed                  // sending failed
	StatusPending                 // being sent
	StatusSent                    // accepted by WhatsApp's server (one tick)
	StatusDelivered               // delivered to the recipient (two ticks)
	StatusRead                    // read (blue ticks)
	StatusPlayed                  // voice note played
)

// Reaction is one person's current reaction to a message.
type Reaction struct {
	SenderJID string `json:"sender"` // "me" for your own
	Emoji     string `json:"emoji"`
	Timestamp int64  `json:"timestamp"` // unix millis
}

// PollVote is one person's current vote on a poll.
type PollVote struct {
	VoterJID string `json:"voter"` // "me" for your own
	// Options holds the lower-case hex SHA-256 of each selected option's
	// name (how WhatsApp identifies poll options).
	Options   []string `json:"options"`
	Timestamp int64    `json:"timestamp"` // unix millis
}

// Store is the message history kept in <dataDir>/wa.db. It holds no data
// itself, so creating one is free and several can point at the same
// directory - they all see the same messages.
type Store struct {
	dir string

	// mu guards legacyChecked.
	mu sync.Mutex
	// legacyChecked is set once this Store has looked for an old
	// messages.json to import (see legacy.go).
	legacyChecked bool
}

// New creates a Store for the database under dataDir. Nothing is opened
// yet: that happens on the first method call, which is also the one that
// reports a problem opening it.
func New(dataDir string) *Store {
	return &Store{dir: dataDir}
}

// db returns the shared database handle, opening it if needed. The first
// call also imports an old messages.json, if there is one.
func (s *Store) db() (*sql.DB, error) {
	db, err := localdb.Open(s.dir)
	if err != nil {
		return nil, fmt.Errorf("opening message store: %w", err)
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
// dir (the chat store included). Using a store afterwards just opens it
// again. Tests must call it before their temp dir is removed: Windows
// can't delete an open file.
func (s *Store) Close() error {
	return localdb.Close(s.dir)
}

// ---- saving ----

// insertSQL saves one message; the two endings below say what happens
// when the chat already has a message with that ID.
const insertSQL = `INSERT INTO messages
	(chat_jid, id, sender_jid, timestamp, text, from_me, media_type, raw_proto,
	 status, edited, deleted, hd, reactions, poll_votes)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

// insertNewSQL leaves an existing message exactly as it is.
const insertNewSQL = insertSQL + ` ON CONFLICT (id, chat_jid) DO NOTHING`

// putSQL overwrites every field of an existing message ("excluded" is
// SQLite's name for the row we tried to insert). The row keeps its seq.
const putSQL = insertSQL + ` ON CONFLICT (id, chat_jid) DO UPDATE SET
	sender_jid = excluded.sender_jid,
	timestamp  = excluded.timestamp,
	text       = excluded.text,
	from_me    = excluded.from_me,
	media_type = excluded.media_type,
	raw_proto  = excluded.raw_proto,
	status     = excluded.status,
	edited     = excluded.edited,
	deleted    = excluded.deleted,
	hd         = excluded.hd,
	reactions  = excluded.reactions,
	poll_votes = excluded.poll_votes`

// insertArgs returns msg's fields in the order insertSQL expects them.
func insertArgs(msg Message) []any {
	return []any{
		msg.ChatJID, msg.ID, msg.SenderJID, msg.Timestamp, msg.Text, msg.FromMe, msg.MediaType,
		rawProtoValue(msg.RawProto),
		int64(msg.Status), msg.Edited, msg.Deleted, msg.HD,
		encodeList(msg.Reactions), encodeList(msg.PollVotes),
	}
}

// strictBase64 decodes base64 without the leniency of the standard
// decoder (which accepts stray bits in the last character), so that only
// text which encodes back to exactly itself is accepted.
var strictBase64 = base64.StdEncoding.Strict()

// rawProtoValue turns a Message.RawProto into what is stored: the decoded
// bytes (a third smaller than their base64 text), or nothing when empty.
//
// A value that isn't clean base64 is stored unchanged as text instead, so
// whatever was saved is always exactly what comes back. (Line breaks are
// checked separately because every decoder skips them silently.)
func rawProtoValue(rawProto string) any {
	if rawProto == "" {
		return nil
	}
	if strings.ContainsAny(rawProto, "\r\n") {
		return rawProto
	}
	decoded, err := strictBase64.DecodeString(rawProto)
	if err != nil {
		return rawProto
	}
	return decoded
}

// encodeList turns reactions or poll votes into the JSON text stored in
// their column; "" when there are none, which is the usual case.
func encodeList[T any](list []T) string {
	if len(list) == 0 {
		return ""
	}
	data, err := json.Marshal(list)
	if err != nil {
		return "" // can't happen for these plain structs
	}
	return string(data)
}

// decodeList is the reverse of encodeList. Text that doesn't parse counts
// as "none" rather than as an error, so one bad cell can't make a whole
// chat unreadable.
func decodeList[T any](stored string) []T {
	if stored == "" {
		return nil
	}
	var list []T
	if err := json.Unmarshal([]byte(stored), &list); err != nil {
		return nil
	}
	return list
}

// newLocalID makes up an ID for a message that arrived without one, so
// it can still be stored and referred to. 12 random bytes: two never
// collide in practice.
func newLocalID() string {
	var random [12]byte
	rand.Read(random[:])
	return "local-" + hex.EncodeToString(random[:])
}

// execer is what a *sql.Stmt offers for running the insert; it lets
// insertNew serve both InsertMany and the legacy import.
type execer interface {
	Exec(args ...any) (sql.Result, error)
}

// insertNew saves msg through insert (a prepared insertNewSQL). It returns
// the message as stored (with its Seq, and its ID if one had to be made
// up) and whether it was new.
func insertNew(insert execer, msg Message) (Message, bool, error) {
	if msg.ID == "" {
		msg.ID = newLocalID()
	}
	result, err := insert.Exec(insertArgs(msg)...)
	if err != nil {
		return msg, false, err
	}
	if n, err := result.RowsAffected(); err != nil || n == 0 {
		return msg, false, err // the chat already has this ID
	}
	msg.Seq, err = result.LastInsertId()
	return msg, true, err
}

// Append saves msg. If its chat already has a message with the same ID,
// nothing happens and the stored one is kept.
func (s *Store) Append(msg Message) error {
	_, err := s.Insert(msg)
	return err
}

// AppendMany saves msgs in one transaction, skipping any whose ID is
// already stored in its chat. The order of msgs doesn't matter: reads
// sort by timestamp.
func (s *Store) AppendMany(msgs []Message) error {
	_, err := s.InsertMany(msgs)
	return err
}

// Insert is Append that also reports whether msg was new. A message with
// an empty ID is given a unique made-up one ("local-...") and is
// therefore always new.
func (s *Store) Insert(msg Message) (inserted bool, err error) {
	db, err := s.db()
	if err != nil {
		return false, err
	}
	if msg.ID == "" {
		msg.ID = newLocalID()
	}
	result, err := localdb.Exec(db, insertNewSQL, insertArgs(msg)...)
	if err != nil {
		return false, fmt.Errorf("saving message: %w", err)
	}
	n, err := result.RowsAffected()
	return n > 0, err
}

// InsertMany saves msgs in one transaction - all of them or, on an error,
// none. It returns the ones that were actually new, in the order given,
// with Seq filled in; messages whose ID was already stored in their chat
// (or appears twice in msgs) are skipped.
func (s *Store) InsertMany(msgs []Message) (inserted []Message, err error) {
	if len(msgs) == 0 {
		return nil, nil
	}
	db, err := s.db()
	if err != nil {
		return nil, err
	}
	err = localdb.Update(db, func(tx *sql.Tx) error {
		// Preparing once saves SQLite from re-reading the same statement
		// for every message of a big history batch.
		insert, err := tx.Prepare(insertNewSQL)
		if err != nil {
			return err
		}
		defer insert.Close()

		for _, msg := range msgs {
			stored, isNew, err := insertNew(insert, msg)
			if err != nil {
				return err
			}
			if isNew {
				inserted = append(inserted, stored)
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("saving messages: %w", err)
	}
	return inserted, nil
}

// Put saves msg, replacing every field of the stored message with the
// same chat and ID if there is one (it keeps its Seq, so its place among
// messages with the same timestamp doesn't change).
func (s *Store) Put(msg Message) error {
	db, err := s.db()
	if err != nil {
		return err
	}
	if msg.ID == "" {
		msg.ID = newLocalID()
	}
	if _, err := localdb.Exec(db, putSQL, insertArgs(msg)...); err != nil {
		return fmt.Errorf("saving message: %w", err)
	}
	return nil
}

// ---- reading ----

// columns lists what every read selects, in the order queryMessages
// expects. raw_proto is followed by a flag saying whether it was stored
// as text (see rawProtoValue).
const columns = `seq, chat_jid, id, sender_jid, timestamp, text, from_me, media_type,
	raw_proto, typeof(raw_proto) = 'text',
	status, edited, deleted, hd, reactions, poll_votes`

// queryMessages runs a SELECT of columns and returns the rows as
// Messages. It reads everything and closes the query before returning,
// so the connection is free again by the time the caller acts on the
// result (the pool has only two).
func queryMessages(db *sql.DB, query string, args ...any) ([]Message, error) {
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var msgs []Message
	// encoded is scratch space for turning each row's content into
	// base64, reused from row to row to save an allocation per message.
	var encoded []byte
	for rows.Next() {
		var (
			msg Message
			// RawBytes lends us the driver's bytes instead of copying
			// them; they are only valid until the next row, which is
			// fine because they are encoded straight away.
			rawProto       sql.RawBytes
			rawProtoIsText bool
			status         int64
			reactions      string
			pollVotes      string
		)
		err := rows.Scan(&msg.Seq, &msg.ChatJID, &msg.ID, &msg.SenderJID, &msg.Timestamp, &msg.Text, &msg.FromMe, &msg.MediaType,
			&rawProto, &rawProtoIsText,
			&status, &msg.Edited, &msg.Deleted, &msg.HD, &reactions, &pollVotes)
		if err != nil {
			return nil, err
		}
		switch {
		case rawProtoIsText:
			msg.RawProto = string(rawProto)
		case len(rawProto) > 0:
			encoded = base64.StdEncoding.AppendEncode(encoded[:0], rawProto)
			msg.RawProto = string(encoded)
		}
		msg.Status = Status(status)
		msg.Reactions = decodeList[Reaction](reactions)
		msg.PollVotes = decodeList[PollVote](pollVotes)
		msgs = append(msgs, msg)
	}
	return msgs, rows.Err()
}

// query is queryMessages on the store's database.
func (s *Store) query(query string, args ...any) ([]Message, error) {
	db, err := s.db()
	if err != nil {
		return nil, err
	}
	msgs, err := queryMessages(db, query, args...)
	if err != nil {
		return nil, fmt.Errorf("reading messages: %w", err)
	}
	return msgs, nil
}

// queryOne is query for a SELECT that returns at most one message.
func (s *Store) queryOne(query string, args ...any) (Message, bool, error) {
	msgs, err := s.query(query, args...)
	if err != nil || len(msgs) == 0 {
		return Message{}, false, err
	}
	return msgs[0], true, nil
}

// List returns ALL of chatJID's stored messages, oldest first (matching
// the numbering `wa chat open` shows). For a screenful, use Latest and
// Before instead - a chat's history can be large.
func (s *Store) List(chatJID string) ([]Message, error) {
	return s.query(`SELECT `+columns+` FROM messages
		WHERE chat_jid = ? ORDER BY timestamp, seq`, chatJID)
}

// Get returns the message with the given WhatsApp message id within
// chatJID's history, if present.
func (s *Store) Get(chatJID, id string) (Message, bool, error) {
	return s.queryOne(`SELECT `+columns+` FROM messages
		WHERE chat_jid = ? AND id = ?`, chatJID, id)
}

// Find returns the message with the given ID whichever chat it is in.
// IDs are only unique within a chat; if several chats have one, the
// newest is returned.
func (s *Store) Find(id string) (Message, bool, error) {
	return s.queryOne(`SELECT `+columns+` FROM messages
		WHERE id = ? ORDER BY timestamp DESC, seq DESC LIMIT 1`, id)
}

// Latest returns the newest limit messages of chatJID, oldest first -
// the page a chat shows when it is opened.
func (s *Store) Latest(chatJID string, limit int) ([]Message, error) {
	// Walk the index backwards from the newest message and stop after
	// limit rows, then flip the page into display order.
	msgs, err := s.query(`SELECT `+columns+` FROM messages
		WHERE chat_jid = ? ORDER BY timestamp DESC, seq DESC LIMIT ?`, chatJID, limit)
	slices.Reverse(msgs)
	return msgs, err
}

// Before returns up to limit messages of chatJID that are strictly older
// than the message with the given Timestamp and Seq, oldest first - the
// page shown when scrolling up. Pass the Timestamp and Seq of the oldest
// message you already have; the result ends right before it.
func (s *Store) Before(chatJID string, timestamp, seq int64, limit int) ([]Message, error) {
	// "Older" means an earlier timestamp, or the same timestamp and a
	// smaller seq. It is written as "timestamp <= ? AND (...)" so SQLite
	// can jump straight to the cursor's place in the index instead of
	// walking there from the newest message.
	msgs, err := s.query(`SELECT `+columns+` FROM messages
		WHERE chat_jid = ? AND timestamp <= ? AND (timestamp < ? OR seq < ?)
		ORDER BY timestamp DESC, seq DESC LIMIT ?`, chatJID, timestamp, timestamp, seq, limit)
	slices.Reverse(msgs)
	return msgs, err
}

// Oldest returns chatJID's oldest stored message, if it has any.
func (s *Store) Oldest(chatJID string) (Message, bool, error) {
	return s.queryOne(`SELECT `+columns+` FROM messages
		WHERE chat_jid = ? ORDER BY timestamp, seq LIMIT 1`, chatJID)
}

// Count returns how many messages are stored for chatJID.
func (s *Store) Count(chatJID string) (int, error) {
	db, err := s.db()
	if err != nil {
		return 0, err
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM messages WHERE chat_jid = ?`, chatJID).Scan(&n); err != nil {
		return 0, fmt.Errorf("counting messages: %w", err)
	}
	return n, nil
}

// LastIncoming returns the newest limit messages of chatJID that someone
// else sent (FromMe false), NEWEST first.
func (s *Store) LastIncoming(chatJID string, limit int) ([]Message, error) {
	return s.query(`SELECT `+columns+` FROM messages
		WHERE chat_jid = ? AND from_me = 0
		ORDER BY timestamp DESC, seq DESC LIMIT ?`, chatJID, limit)
}

// Search returns up to limit messages whose Text contains query, ignoring
// upper/lower case, NEWEST first. chatJID "" searches every chat.
//
// There is no text index: a search reads through the chat (or, for every
// chat, the whole table), so run it off the UI goroutine.
func (s *Store) Search(chatJID, query string, limit int) ([]Message, error) {
	// wa_lower is localdb's Go-style lower-casing; instr(a, b) > 0 is
	// SQL for strings.Contains(a, b).
	needle := strings.ToLower(query)
	if chatJID == "" {
		return s.query(`SELECT `+columns+` FROM messages
			WHERE instr(wa_lower(text), ?) > 0
			ORDER BY timestamp DESC, seq DESC LIMIT ?`, needle, limit)
	}
	return s.query(`SELECT `+columns+` FROM messages
		WHERE chat_jid = ? AND instr(wa_lower(text), ?) > 0
		ORDER BY timestamp DESC, seq DESC LIMIT ?`, chatJID, needle, limit)
}

// eachPage is how many messages Each reads at a time.
const eachPage = 200

// Each calls fn for every stored message: chat by chat, each chat oldest
// first. It stops at, and returns, the first error fn returns.
//
// fn may call any Store method, including ones that change or delete the
// message it was given: Each reads a page, lets go of the database, and
// only then calls fn. Chats that first appear while Each is running are
// not visited.
func (s *Store) Each(fn func(Message) error) error {
	chats, err := s.chatJIDs(`SELECT DISTINCT chat_jid FROM messages ORDER BY chat_jid`)
	if err != nil {
		return err
	}
	for _, chatJID := range chats {
		// The place of the last message handed to fn; the next page
		// starts right after it. Remembering a place rather than a count
		// keeps this correct when fn deletes messages as it goes.
		var timestamp, seq int64
		first := true
		for {
			var page []Message
			if first {
				page, err = s.query(`SELECT `+columns+` FROM messages
					WHERE chat_jid = ? ORDER BY timestamp, seq LIMIT ?`, chatJID, eachPage)
				first = false
			} else {
				page, err = s.query(`SELECT `+columns+` FROM messages
					WHERE chat_jid = ? AND timestamp >= ? AND (timestamp > ? OR seq > ?)
					ORDER BY timestamp, seq LIMIT ?`, chatJID, timestamp, timestamp, seq, eachPage)
			}
			if err != nil {
				return err
			}
			for _, msg := range page {
				if err := fn(msg); err != nil {
					return err
				}
			}
			if len(page) < eachPage {
				break // that was the chat's last page
			}
			timestamp, seq = page[len(page)-1].Timestamp, page[len(page)-1].Seq
		}
	}
	return nil
}

// chatJIDs runs a query that selects chat JIDs and returns them.
func (s *Store) chatJIDs(query string, args ...any) ([]string, error) {
	db, err := s.db()
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("listing chats with messages: %w", err)
	}
	defer rows.Close()

	var jids []string
	for rows.Next() {
		var jid string
		if err := rows.Scan(&jid); err != nil {
			return nil, fmt.Errorf("listing chats with messages: %w", err)
		}
		jids = append(jids, jid)
	}
	return jids, rows.Err()
}

// ---- small named values ----

// Meta returns the value saved under key, "" if there is none. The meta
// table is a place for flags that must survive restarts, such as
// "legacy_fold".
func (s *Store) Meta(key string) (string, error) {
	db, err := s.db()
	if err != nil {
		return "", err
	}
	value, err := localdb.Meta(db, key)
	if err != nil {
		return "", fmt.Errorf("reading meta %q: %w", key, err)
	}
	return value, nil
}

// SetMeta saves value under key.
func (s *Store) SetMeta(key, value string) error {
	return s.update(func(tx *sql.Tx) error {
		return localdb.SetMeta(tx, key, value)
	})
}
