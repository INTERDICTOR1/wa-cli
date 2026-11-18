package msgstore

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"

	"github.com/codebyoketch/wa-cli/internal/localdb"
)

// legacyFile is where the old store kept every message: one JSON object
// mapping each chat JID to the list of its messages.
const legacyFile = "messages.json"

// importLegacy copies an old messages.json into the database the first
// time the store is used, then renames it to messages.json.migrated.
// localdb.ImportLegacy supplies the guarantees (once only, all or
// nothing, the file is never deleted, a damaged file is reported in the
// meta table instead of breaking the store); this function only knows
// how to read the file.
//
// The old store could hold the same message several times; here the
// first copy wins. Old histories also still contain reactions, edits and
// deletions as separate messages, so after importing anything the meta
// value "legacy_fold" is set to "pending" - the WhatsApp layer then folds
// those into the messages they refer to and sets it to "done".
func importLegacy(db *sql.DB, dataDir string) {
	path := filepath.Join(dataDir, legacyFile)
	localdb.ImportLegacy(db, "messages", path, func(tx *sql.Tx, file io.Reader) error {
		insert, err := tx.Prepare(insertNewSQL)
		if err != nil {
			return err
		}
		defer insert.Close()

		imported := 0
		err = readLegacy(file, func(msg Message) error {
			_, isNew, err := insertNew(insert, msg)
			if isNew {
				imported++
			}
			return err
		})
		if err != nil {
			return fmt.Errorf("reading %s: %w", legacyFile, err)
		}
		if imported == 0 {
			return nil
		}
		return localdb.SetMeta(tx, "legacy_fold", "pending")
	})
}

// readLegacy reads the old file's JSON from r and calls fn for each
// message, in file order.
//
// It reads piece by piece (one "token" - a brace, a key, a value - at a
// time) rather than loading the file into one big map: the file is
// megabytes, and this way only one message is in memory at once.
func readLegacy(r io.Reader, fn func(Message) error) error {
	dec := json.NewDecoder(r)

	// The opening { of the whole object.
	token, err := dec.Token()
	if err == io.EOF {
		return nil // an empty file: no messages
	}
	if err != nil {
		return err
	}
	if token == nil {
		return nil // the file says "null": no messages
	}
	if token != json.Delim('{') {
		return errors.New("expected a JSON object of chats")
	}

	// One "chat JID": [messages...] entry per turn.
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return endedEarly(err)
		}
		chatJID, _ := key.(string) // object keys are always strings

		token, err := dec.Token()
		if err != nil {
			return endedEarly(err)
		}
		if token == nil {
			continue // "chat JID": null
		}
		if token != json.Delim('[') {
			return fmt.Errorf("chat %q: expected a list of messages", chatJID)
		}
		for dec.More() {
			var msg Message
			if err := dec.Decode(&msg); err != nil {
				return endedEarly(err)
			}
			// The key is what the old store filed the message under.
			msg.ChatJID = chatJID
			if err := fn(msg); err != nil {
				return err
			}
		}
		// The list's closing ].
		if _, err := dec.Token(); err != nil {
			return endedEarly(err)
		}
	}

	// The closing } of the whole object.
	_, err = dec.Token()
	return endedEarly(err)
}

// endedEarly gives the bare "EOF" a decoder returns when the file stops
// in the middle a message that says so.
func endedEarly(err error) error {
	if err == io.EOF {
		return io.ErrUnexpectedEOF
	}
	return err
}
