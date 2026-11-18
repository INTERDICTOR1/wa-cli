package msgstore

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/codebyoketch/wa-cli/internal/localdb"
)

// This file holds the methods that change messages that are already
// stored: delivery status, reactions, poll votes, edits, deletion,
// moving and pruning.

// exec runs one statement that changes data and returns how many rows it
// changed.
func (s *Store) exec(query string, args ...any) (int64, error) {
	db, err := s.db()
	if err != nil {
		return 0, err
	}
	result, err := localdb.Exec(db, query, args...)
	if err != nil {
		return 0, fmt.Errorf("updating messages: %w", err)
	}
	return result.RowsAffected()
}

// update runs fn in one write transaction (see localdb.Update): its
// changes are saved together or not at all, and nothing else - not even
// another wa process - can write in between what fn reads and writes.
func (s *Store) update(fn func(tx *sql.Tx) error) error {
	db, err := s.db()
	if err != nil {
		return err
	}
	if err := localdb.Update(db, fn); err != nil {
		return fmt.Errorf("updating messages: %w", err)
	}
	return nil
}

// ---- delivery status ----

// SetStatus sets the Status of the given messages of chatJID to st, no
// matter what it was. IDs that aren't stored are ignored.
func (s *Store) SetStatus(chatJID string, ids []string, st Status) error {
	if len(ids) == 0 {
		return nil
	}
	return s.update(func(tx *sql.Tx) error {
		for _, id := range ids {
			_, err := tx.Exec(`UPDATE messages SET status = ?
				WHERE chat_jid = ? AND id = ?`, int64(st), chatJID, id)
			if err != nil {
				return err
			}
		}
		return nil
	})
}

// RaiseStatus moves the given messages of chatJID up to st, and returns
// the IDs it actually changed. Only messages you sent (FromMe) whose
// Status is below st are touched, so a late "delivered" receipt can't
// undo a "read" one, and receipts never mark incoming messages.
func (s *Store) RaiseStatus(chatJID string, ids []string, st Status) (changed []string, err error) {
	if len(ids) == 0 {
		return nil, nil
	}
	err = s.update(func(tx *sql.Tx) error {
		for _, id := range ids {
			result, err := tx.Exec(`UPDATE messages SET status = ?
				WHERE chat_jid = ? AND id = ? AND from_me = 1 AND status < ?`, int64(st), chatJID, id, int64(st))
			if err != nil {
				return err
			}
			if n, _ := result.RowsAffected(); n > 0 {
				changed = append(changed, id)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return changed, nil
}

// ---- reactions and poll votes ----

// SetReaction records r as r.SenderJID's reaction to the message
// targetID of chatJID, replacing that person's earlier reaction. An empty
// r.Emoji removes it (that is how WhatsApp sends "reaction removed").
//
// found is false, with no error, when the target message isn't stored.
func (s *Store) SetReaction(chatJID, targetID string, r Reaction) (found bool, err error) {
	err = s.update(func(tx *sql.Tx) error {
		var stored string
		err := tx.QueryRow(`SELECT reactions FROM messages
			WHERE chat_jid = ? AND id = ?`, chatJID, targetID).Scan(&stored)
		if err == sql.ErrNoRows {
			return nil
		}
		if err != nil {
			return err
		}
		found = true

		// Everyone else's reactions, then this person's new one.
		var reactions []Reaction
		for _, old := range decodeList[Reaction](stored) {
			if old.SenderJID != r.SenderJID {
				reactions = append(reactions, old)
			}
		}
		if r.Emoji != "" {
			reactions = append(reactions, r)
		}

		_, err = tx.Exec(`UPDATE messages SET reactions = ?
			WHERE chat_jid = ? AND id = ?`, encodeList(reactions), chatJID, targetID)
		return err
	})
	return found, err
}

// SetPollVote records v as v.VoterJID's vote on the poll message targetID
// of chatJID, replacing that person's earlier vote. A vote with no
// Options removes it (the voter unticked everything).
//
// found is false, with no error, when the poll message isn't stored.
func (s *Store) SetPollVote(chatJID, targetID string, v PollVote) (found bool, err error) {
	err = s.update(func(tx *sql.Tx) error {
		var stored string
		err := tx.QueryRow(`SELECT poll_votes FROM messages
			WHERE chat_jid = ? AND id = ?`, chatJID, targetID).Scan(&stored)
		if err == sql.ErrNoRows {
			return nil
		}
		if err != nil {
			return err
		}
		found = true

		var votes []PollVote
		for _, old := range decodeList[PollVote](stored) {
			if old.VoterJID != v.VoterJID {
				votes = append(votes, old)
			}
		}
		if len(v.Options) > 0 {
			votes = append(votes, v)
		}

		_, err = tx.Exec(`UPDATE messages SET poll_votes = ?
			WHERE chat_jid = ? AND id = ?`, encodeList(votes), chatJID, targetID)
		return err
	})
	return found, err
}

// ---- edits, deletion, content swaps ----

// ApplyEdit replaces the text and content (rawProto, base64 as in
// Message.RawProto) of the message targetID of chatJID and marks it
// Edited. found is false, with no error, when it isn't stored.
func (s *Store) ApplyEdit(chatJID, targetID, text, rawProto string) (found bool, err error) {
	n, err := s.exec(`UPDATE messages SET text = ?, raw_proto = ?, edited = 1
		WHERE chat_jid = ? AND id = ?`, text, rawProtoValue(rawProto), chatJID, targetID)
	return n > 0, err
}

// MarkDeleted marks the message targetID of chatJID as deleted for
// everyone: Deleted is set and its text, content, media type and
// reactions are cleared. The row itself stays, so the chat can show
// "this message was deleted" in its place. found is false, with no
// error, when it isn't stored.
func (s *Store) MarkDeleted(chatJID, targetID string) (found bool, err error) {
	n, err := s.exec(`UPDATE messages
		SET deleted = 1, text = '', raw_proto = NULL, media_type = '', reactions = ''
		WHERE chat_jid = ? AND id = ?`, chatJID, targetID)
	return n > 0, err
}

// ReplaceContent swaps the content of the message targetID of chatJID -
// used when the HD version of an image arrives after the standard one.
// The media type, content (rawProto) and HD flag are replaced; the text
// is replaced only if text isn't empty. found is false, with no error,
// when it isn't stored.
func (s *Store) ReplaceContent(chatJID, targetID, mediaType, text, rawProto string, hd bool) (found bool, err error) {
	n, err := s.exec(`UPDATE messages
		SET media_type = ?, raw_proto = ?, hd = ?,
		    text = CASE WHEN ? = '' THEN text ELSE ? END
		WHERE chat_jid = ? AND id = ?`,
		mediaType, rawProtoValue(rawProto), hd, text, text, chatJID, targetID)
	return n > 0, err
}

// ---- removing and moving ----

// Delete removes one message from this store (only locally - nothing is
// sent to WhatsApp). Deleting a message that isn't stored is not an
// error.
func (s *Store) Delete(chatJID, id string) error {
	_, err := s.exec(`DELETE FROM messages WHERE chat_jid = ? AND id = ?`, chatJID, id)
	return err
}

// DeleteChat removes every stored message of chatJID.
func (s *Store) DeleteChat(chatJID string) error {
	_, err := s.exec(`DELETE FROM messages WHERE chat_jid = ?`, chatJID)
	return err
}

// MoveChat puts all of fromJID's messages under toJID - for when the
// same conversation turns out to be known under two JIDs. If both have a
// message with the same ID, the one already under toJID is kept and
// fromJID's copy is dropped. Afterwards fromJID has no messages.
func (s *Store) MoveChat(fromJID, toJID string) error {
	if fromJID == toJID {
		return nil
	}
	return s.update(func(tx *sql.Tx) error {
		// OR IGNORE skips the rows that would clash with one toJID
		// already has, leaving them under fromJID...
		_, err := tx.Exec(`UPDATE OR IGNORE messages SET chat_jid = ?
			WHERE chat_jid = ?`, toJID, fromJID)
		if err != nil {
			return err
		}
		// ...where this removes them.
		_, err = tx.Exec(`DELETE FROM messages WHERE chat_jid = ?`, fromJID)
		return err
	})
}

// Prune deletes messages older than before, but never a chat's newest
// keepPerChat messages, however old they are. It returns how many it
// deleted.
//
// The file doesn't shrink: SQLite keeps the freed space and reuses it for
// new messages.
func (s *Store) Prune(before time.Time, keepPerChat int) (deleted int, err error) {
	if keepPerChat < 0 {
		keepPerChat = 0
	}
	cutoff := before.UnixMilli()

	chats, err := s.chatJIDs(`SELECT DISTINCT chat_jid FROM messages WHERE timestamp < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	// One chat per statement, so the write lock is held only briefly at
	// a time and incoming messages can be saved in between.
	for _, chatJID := range chats {
		// The sub-select lists the chat's newest keepPerChat messages -
		// the ones to spare.
		n, err := s.exec(`DELETE FROM messages
			WHERE chat_jid = ? AND timestamp < ?
			  AND seq NOT IN (
				SELECT seq FROM messages WHERE chat_jid = ?
				ORDER BY timestamp DESC, seq DESC LIMIT ?)`,
			chatJID, cutoff, chatJID, keepPerChat)
		deleted += int(n)
		if err != nil {
			return deleted, err
		}
	}
	return deleted, nil
}
