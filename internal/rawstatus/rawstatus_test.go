package rawstatus

import (
	"testing"
	"time"

	store "github.com/rayer/llm-wiki-bff/internal/storage"
)

func TestApplyMarksIngestedWhenHashStatusAndErrorMatch(t *testing.T) {
	files := []store.RawFile{{
		Name:    "seed.md",
		Path:    "raw/seed.md",
		Size:    4,
		Updated: time.Date(2026, 7, 9, 10, 0, 0, 0, time.UTC),
		SHA256:  "abc",
	}}
	artifact := Artifact{Files: map[string]FileStatus{
		"seed.md": {Path: "raw/seed.md", SHA256: "abc", OLWStatus: "ingested", Ingested: true},
	}}

	got := Apply(files, artifact)

	if len(got) != 1 || !got[0].Ingested {
		t.Fatalf("Apply() = %#v, want ingested seed.md", got)
	}
}

func TestApplyReturnsFalseForMissingChangedErrorAndUnsupportedStatus(t *testing.T) {
	files := []store.RawFile{
		{Name: "missing.md", Path: "raw/missing.md", SHA256: "same"},
		{Name: "changed.md", Path: "raw/changed.md", SHA256: "new"},
		{Name: "failed.md", Path: "raw/failed.md", SHA256: "same"},
		{Name: "new.md", Path: "raw/new.md", SHA256: "same"},
	}
	artifact := Artifact{Files: map[string]FileStatus{
		"changed.md": {Path: "raw/changed.md", SHA256: "old", OLWStatus: "ingested", Ingested: true},
		"failed.md":  {Path: "raw/failed.md", SHA256: "same", OLWStatus: "ingested", Ingested: true, Error: "boom"},
		"new.md":     {Path: "raw/new.md", SHA256: "same", OLWStatus: "new", Ingested: true},
	}}

	got := Apply(files, artifact)

	for _, file := range got {
		if file.Ingested {
			t.Fatalf("%s ingested=true, want false: %#v", file.Name, got)
		}
	}
}

func TestDecodeRejectsMalformedJSON(t *testing.T) {
	if _, err := Decode([]byte(`{"files":`)); err == nil {
		t.Fatal("Decode() error = nil, want malformed JSON error")
	}
}

func TestDecodeInfersFileCountFromFilesMap(t *testing.T) {
	artifact, err := Decode([]byte(`{"version":1,"files":{"a.md":{"path":"raw/a.md"},"b.md":{"path":"raw/b.md"}}}`))
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	if artifact.FileCount != 2 {
		t.Fatalf("FileCount = %d, want 2", artifact.FileCount)
	}
	if Count(artifact) != 2 {
		t.Fatalf("Count() = %d, want 2", Count(artifact))
	}
}

func TestCountPrefersExplicitFileCount(t *testing.T) {
	artifact := Artifact{FileCount: 3, Files: map[string]FileStatus{}}
	if Count(artifact) != 3 {
		t.Fatalf("Count() = %d, want 3", Count(artifact))
	}
}
