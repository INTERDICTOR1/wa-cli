package chatstore

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"

	"github.com/codebyoketch/wa-cli/internal/localdb"
)

// legacyFile is where the old store kept every chat: one JSON object
// mapping each chat's JID to its Chat.
const legacyFile = "chats.json"

// importLegacy copies an old chats.json into the database the first time
// the store is used, then renames it to chats.json.migrated.
// localdb.ImportLegacy supplies the guarantees (once only, all or
// nothing, the file is never deleted, a damaged file is reported in the
// meta table instead of breaking the store); this function only knows
// how to read the file.
func importLegacy(db *sql.DB, dataDir string) {
	path := filepath.Join(dataDir, legacyFile)
	localdb.ImportLegacy(db, "chats", path, func(tx *sql.Tx, file io.Reader) error {
		// The chat list is small (a few hundred KB at most), so unlike
		// the message history it is simply read in one go.
		var chats map[string]Chat
		if err := json.NewDecoder(file).Decode(&chats); err != nil && err != io.EOF {
			return fmt.Errorf("reading %s: %w", legacyFile, err)
		}

		// DO NOTHING: should the database already know a chat, its row is
		// newer than the file and wins.
		insert, err := tx.Prepare(`INSERT INTO chats (` + columns + `)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (jid) DO NOTHING`)
		if err != nil {
			return err
		}
		defer insert.Close()

		for jid, c := range chats {
			// The key is what the old store looked chats up by.
			c.JID = jid
			if _, err := insert.Exec(chatArgs(c)...); err != nil {
				return err
			}
		}
		return nil
	})
}
