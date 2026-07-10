package rawstatus

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	store "github.com/rayer/llm-wiki-bff/internal/storage"
	_ "modernc.org/sqlite"
)

func TestBuildFromStateDBMarksMatchingIngestedRows(t *testing.T) {
	dbPath := createStateDB(t, `
insert into raw_notes(path, content_hash, status, ingested_at, error) values
('raw/seed.md', 'abc', 'ingested', '2026-06-21T16:25:25Z', ''),
('raw/changed.md', 'old', 'ingested', '2026-06-21T16:25:25Z', ''),
('raw/failed.md', 'same', 'ingested', '2026-06-21T16:25:25Z', 'boom');
`)
	files := []store.RawFile{
		{Name: "seed.md", Path: "raw/seed.md", SHA256: "abc"},
		{Name: "changed.md", Path: "raw/changed.md", SHA256: "new"},
		{Name: "failed.md", Path: "raw/failed.md", SHA256: "same"},
	}

	artifact, err := BuildFromStateDB(context.Background(), dbPath, files, time.Date(2026, 7, 9, 10, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("BuildFromStateDB() error = %v", err)
	}

	if !artifact.Files["seed.md"].Ingested {
		t.Fatalf("seed status = %#v, want ingested", artifact.Files["seed.md"])
	}
	// Hash drift alone no longer blocks "ingested" UI; failed still does via error.
	if !artifact.Files["changed.md"].Ingested {
		t.Fatalf("changed status = %#v, want ingested from OLW status despite hash drift", artifact.Files["changed.md"])
	}
	if artifact.Files["failed.md"].Ingested {
		t.Fatalf("failed status = %#v, want uningested due to error", artifact.Files["failed.md"])
	}
}

func TestBuildFromStateDBMissingDBReturnsEmptyArtifact(t *testing.T) {
	artifact, err := BuildFromStateDB(context.Background(), filepath.Join(t.TempDir(), "missing.db"), nil, time.Date(2026, 7, 9, 10, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("BuildFromStateDB() error = %v", err)
	}
	if len(artifact.Files) != 0 {
		t.Fatalf("files = %#v, want empty", artifact.Files)
	}
}

func TestBuildFromStateDBSchemaMismatchFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`create table raw_notes(path text primary key);`); err != nil {
		t.Fatal(err)
	}

	if _, err := BuildFromStateDB(context.Background(), path, nil, time.Now()); err == nil {
		t.Fatal("BuildFromStateDB() error = nil, want schema mismatch error")
	}
}

func createStateDB(t *testing.T, inserts string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`
create table raw_notes (
	path text primary key,
	content_hash text not null,
	status text not null default 'new',
	ingested_at text,
	error text
);
` + inserts); err != nil {
		t.Fatal(err)
	}
	return path
}
