package localdb

import (
	"database/sql"
	"io"
	"os"
)

// ImportLegacy moves the contents of one of the old JSON state files
// (chats.json, messages.json) into the database, once.
//
// name is "chats" or "messages"; path is the JSON file; load reads the
// file and inserts its rows using tx.
//
// What it guarantees:
//
//   - Once only: success is recorded in the meta table as
//     "legacy_<name>_imported" = "1", in the same transaction as the rows.
//     That flag is checked while holding the write lock, so two processes
//     starting at the same moment can't both import.
//   - All or nothing: if load fails (say the file is damaged halfway
//     through), none of its rows are kept.
//   - The user's file is never deleted: after a successful import it is
//     renamed to "<path>.migrated"; after a failed one it is left exactly
//     where it was.
//   - A bad file never stops the app: the reason is written to
//     "legacy_<name>_error" and the store carries on without the old data.
//     The import is tried again the next time the program starts.
//
// There is deliberately no error result - nothing here should make the
// caller's own operation fail.
func ImportLegacy(db *sql.DB, name, path string, load func(tx *sql.Tx, file io.Reader) error) {
	if _, err := os.Stat(path); err != nil {
		return // no old file: the normal case
	}
	importedKey := "legacy_" + name + "_imported"
	errorKey := "legacy_" + name + "_error"

	// A cheap look before queueing for the write lock.
	imported, _ := Meta(db, importedKey)

	if imported == "" {
		err := Update(db, func(tx *sql.Tx) error {
			// The real check, now that nobody else can be importing.
			done, err := Meta(tx, importedKey)
			if err != nil {
				return err
			}
			if done != "" {
				return nil
			}

			file, err := os.Open(path)
			if os.IsNotExist(err) {
				return nil // removed since we looked: nothing to import
			}
			if err != nil {
				return err
			}
			// Closed before Update returns - Windows can't rename an
			// open file.
			defer file.Close()

			if err := load(tx, file); err != nil {
				return err
			}
			if _, err := tx.Exec(`DELETE FROM meta WHERE key = ?`, errorKey); err != nil {
				return err
			}
			return SetMeta(tx, importedKey, "1")
		})
		if err != nil {
			recordImportError(db, importedKey, errorKey, err)
			return
		}
	}

	// Imported (just now, or by an earlier run that was interrupted before
	// this step): move the file out of the way. If a .migrated file is
	// already there, leave both alone rather than overwrite a backup.
	// Failing to rename is harmless - the flag already prevents a second
	// import - so the error is ignored and it is tried again next start.
	migrated := path + ".migrated"
	if _, err := os.Lstat(migrated); os.IsNotExist(err) {
		_ = os.Rename(path, migrated)
	}
}

// recordImportError notes why an import failed, unless another process
// managed the import in the meantime (then there is nothing to report).
// Best effort: if even this write fails there is nowhere left to put it.
func recordImportError(db *sql.DB, importedKey, errorKey string, cause error) {
	_ = Update(db, func(tx *sql.Tx) error {
		done, err := Meta(tx, importedKey)
		if err != nil || done != "" {
			return err
		}
		return SetMeta(tx, errorKey, cause.Error())
	})
}
