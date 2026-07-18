package v1

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rayer/llm-wiki-bff/internal/gcs"
	store "github.com/rayer/llm-wiki-bff/internal/storage"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestFinishRebuildObservesAllCleanupErrorsAfterPrimary(t *testing.T) {
	primary := errors.New("primary rebuild failure")
	called := 0
	err := finishRebuild(primary,
		func() error { called++; return errors.New("lease provider detail") },
		func() error { called++; return errors.New("lock provider detail") },
	)
	if called != 2 || !errors.Is(err, primary) || !errors.Is(err, store.ErrLeaseCleanup) || strings.Contains(err.Error(), "provider") {
		t.Fatalf("finishRebuild error=%v called=%d", err, called)
	}
}

type fakeIDMapStore struct {
	files        map[string][]gcs.MarkdownFile
	reads        map[string][]byte
	atomicWrites map[string][]byte
	tmpWrites    []string
}

func (s fakeIDMapStore) ListMarkdownFiles(_ context.Context, dir string) ([]gcs.MarkdownFile, error) {
	return s.files[dir], nil
}

func (s fakeIDMapStore) ReadFile(_ context.Context, relPath string) ([]byte, error) {
	return s.reads[relPath], nil
}

func (s *fakeIDMapStore) WriteBytesAtomic(_ context.Context, data []byte, tmpPath, finalPath string) (string, error) {
	if s.atomicWrites == nil {
		s.atomicWrites = map[string][]byte{}
	}
	s.tmpWrites = append(s.tmpWrites, tmpPath)
	s.atomicWrites[finalPath] = append([]byte(nil), data...)
	return "digest", nil
}

func TestBuildIDMapParsesIDsAndCarriesRedirects(t *testing.T) {
	oldMap := idMap{
		Concept: map[string]string{"a3f7b2c01d9d": "old-slug"},
		Source:  map[string]string{"c5d9e3f1a028": "same-source"},
		Redirects: map[string][]string{
			"a3f7b2c01d9d": {"legacy-slug"},
		},
	}
	oldJSON, err := json.Marshal(oldMap)
	if err != nil {
		t.Fatalf("marshal old map: %v", err)
	}

	store := fakeIDMapStore{
		files: map[string][]gcs.MarkdownFile{
			"wiki/": {
				{
					Slug: "canonical-slug",
					Data: []byte("---\nid: a3f7b2c01d9d\ntitle: Canonical Title\n---\nBody"),
				},
				{
					Slug: "missing-id",
					Data: []byte("---\ntitle: Missing ID\n---\nBody"),
				},
			},
			"wiki/sources/": {
				{
					Slug: "same-source",
					Data: []byte("---\nid: c5d9e3f1a028\ntitle: Source Title\n---\nBody"),
				},
			},
		},
		reads: map[string][]byte{"cache/id_map.json": oldJSON},
	}

	got, err := buildIDMap(context.Background(), store)
	if err != nil {
		t.Fatalf("build id map: %v", err)
	}

	if got.Concept["a3f7b2c01d9d"] != "canonical-slug" {
		t.Fatalf("concept canonical slug = %q", got.Concept["a3f7b2c01d9d"])
	}
	if _, ok := got.Concept[""]; ok {
		t.Fatal("concept with empty id was included")
	}
	if got.Source["c5d9e3f1a028"] != "same-source" {
		t.Fatalf("source canonical slug = %q", got.Source["c5d9e3f1a028"])
	}
	wantRedirects := []string{"legacy-slug", "old-slug"}
	if len(got.Redirects["a3f7b2c01d9d"]) != len(wantRedirects) {
		t.Fatalf("redirects = %#v, want %#v", got.Redirects["a3f7b2c01d9d"], wantRedirects)
	}
	for i, want := range wantRedirects {
		if got.Redirects["a3f7b2c01d9d"][i] != want {
			t.Fatalf("redirects = %#v, want %#v", got.Redirects["a3f7b2c01d9d"], wantRedirects)
		}
	}
}

func TestActiveLockAcceptsProtoTimestamp(t *testing.T) {
	now := time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)
	lock := map[string]interface{}{
		"status":     "active",
		"expires_at": timestamppb.New(now.Add(time.Minute)),
	}

	if !activeLock(lock, now) {
		t.Fatal("activeLock returned false for a non-expired proto timestamp")
	}
}
