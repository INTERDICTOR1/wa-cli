package store

// NOTE: written without the ability to compile/run in the authoring
// sandbox — see the comment at the top of internal/whatsapp/client_test.go
// for why. Please run `go test ./internal/store/...` locally.

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	waLog "go.mau.fi/whatsmeow/util/log"
)

func TestOpen_CreatesDataDirAndDBFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "data")
	log := waLog.Stdout("test", "ERROR", false)

	container, err := Open(context.Background(), dir, log)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if container == nil {
		t.Fatal("expected non-nil container")
	}
	// Windows can't delete an open file when the temp dir is cleaned up.
	t.Cleanup(func() { container.Close() })

	if _, err := os.Stat(dir); err != nil {
		t.Errorf("expected data dir to be created: %v", err)
	}

	dbPath := filepath.Join(dir, DBFileName)
	if _, err := os.Stat(dbPath); err != nil {
		t.Errorf("expected %s to be created: %v", DBFileName, err)
	}
}

func TestOpen_ReopensExistingStore(t *testing.T) {
	dir := t.TempDir()
	log := waLog.Stdout("test", "ERROR", false)

	first, err := Open(context.Background(), dir, log)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	t.Cleanup(func() { first.Close() })
	// Reopening the same dataDir should succeed against the existing
	// session.db rather than erroring out.
	second, err := Open(context.Background(), dir, log)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	t.Cleanup(func() { second.Close() })
}
