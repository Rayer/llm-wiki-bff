package v1

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"cloud.google.com/go/firestore"
	"cloud.google.com/go/storage"
	fm "github.com/adrg/frontmatter"
	"github.com/rayer/llm-wiki-bff/internal/gcs"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	idMapPath           = "cache/id_map.json"
	idMapTempPath       = "cache/id_map.json.tmp"
	rebuildIndexLockKey = "rebuild_index"
	rebuildIndexTTL     = 60 * time.Second
)

var errRebuildIndexLocked = errors.New("rebuild index already running")

type idMap struct {
	Concept   map[string]string   `json:"concept"`
	Source    map[string]string   `json:"source"`
	Redirects map[string][]string `json:"redirects"`
}

type idMapStore interface {
	ListMarkdownFiles(ctx context.Context, dir string) ([]gcs.MarkdownFile, error)
	ReadFile(ctx context.Context, relPath string) ([]byte, error)
}

type idMapWriter interface {
	WriteBytesAtomic(ctx context.Context, data []byte, tmpPath, finalPath string) (string, error)
}

type idMapReadWriter interface {
	idMapStore
	idMapWriter
}

type markdownMatter struct {
	ID    string `yaml:"id"`
	Title string `yaml:"title"`
}

func buildIDMap(ctx context.Context, store idMapStore) (idMap, error) {
	next := idMap{
		Concept:   map[string]string{},
		Source:    map[string]string{},
		Redirects: map[string][]string{},
	}

	if err := addIDMapEntries(ctx, store, "wiki/", next.Concept); err != nil {
		return next, err
	}
	if err := addIDMapEntries(ctx, store, "wiki/sources/", next.Source); err != nil {
		return next, err
	}

	old, err := readOldIDMap(ctx, store)
	if err != nil {
		return next, err
	}
	next.Redirects = cloneRedirects(old.Redirects)
	appendChangedRedirects(next.Redirects, old.Concept, next.Concept)
	appendChangedRedirects(next.Redirects, old.Source, next.Source)

	return next, nil
}

func rebuildIndex(ctx context.Context, store idMapReadWriter) (idMap, error) {
	next, err := buildIDMap(ctx, store)
	if err != nil {
		return next, err
	}
	if err := writeIDMap(ctx, store, next); err != nil {
		return next, err
	}
	return next, nil
}

func generateID(data []byte) string {
	h := md5.Sum(data)
	return hex.EncodeToString(h[:])[:12]
}

func addIDMapEntries(ctx context.Context, store idMapStore, dir string, entries map[string]string) error {
	files, err := store.ListMarkdownFiles(ctx, dir)
	if err != nil {
		return fmt.Errorf("list %s: %w", dir, err)
	}
	for _, file := range files {
		matter, err := parseIDMapMatter(file.Data)
		if err != nil {
			return fmt.Errorf("parse %s: %w", file.Path, err)
		}
		id := strings.TrimSpace(matter.ID)
		if id == "" {
			id = generateID(file.Data)
		}
		entries[id] = file.Slug
	}
	return nil
}

func parseIDMapMatter(data []byte) (markdownMatter, error) {
	var matter markdownMatter
	if !strings.HasPrefix(string(data), "---") {
		return matter, nil
	}
	_, err := fm.MustParse(strings.NewReader(string(data)), &matter)
	return matter, err
}

func readOldIDMap(ctx context.Context, store idMapStore) (idMap, error) {
	old := idMap{
		Concept:   map[string]string{},
		Source:    map[string]string{},
		Redirects: map[string][]string{},
	}
	data, err := store.ReadFile(ctx, idMapPath)
	if err != nil {
		if errors.Is(err, storage.ErrObjectNotExist) {
			return old, nil
		}
		return old, fmt.Errorf("read old id map: %w", err)
	}
	if len(data) == 0 {
		return old, nil
	}
	if err := json.Unmarshal(data, &old); err != nil {
		return old, fmt.Errorf("decode old id map: %w", err)
	}
	if old.Concept == nil {
		old.Concept = map[string]string{}
	}
	if old.Source == nil {
		old.Source = map[string]string{}
	}
	if old.Redirects == nil {
		old.Redirects = map[string][]string{}
	}
	return old, nil
}

func cloneRedirects(src map[string][]string) map[string][]string {
	dst := make(map[string][]string, len(src))
	for id, redirects := range src {
		dst[id] = append([]string(nil), redirects...)
	}
	return dst
}

func appendChangedRedirects(redirects map[string][]string, oldEntries, newEntries map[string]string) {
	for id, newSlug := range newEntries {
		oldSlug := strings.TrimSpace(oldEntries[id])
		if oldSlug == "" || oldSlug == newSlug || containsString(redirects[id], oldSlug) {
			continue
		}
		redirects[id] = append(redirects[id], oldSlug)
	}
}

func containsString(values []string, needle string) bool {
	for _, value := range values {
		if value == needle {
			return true
		}
	}
	return false
}

func writeIDMap(ctx context.Context, writer idMapWriter, next idMap) error {
	data, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return fmt.Errorf("encode id map: %w", err)
	}
	if _, err := writer.WriteBytesAtomic(ctx, data, idMapTempPath, idMapPath); err != nil {
		return fmt.Errorf("write id map: %w", err)
	}
	return nil
}

func acquireRebuildIndexLock(ctx context.Context, fs *firestore.Client, uid, pid string, now time.Time) error {
	ref := fs.Collection("locks").Doc(fmt.Sprintf("%s__%s", uid, pid))
	expiresAt := now.Add(rebuildIndexTTL)

	return fs.RunTransaction(ctx, func(ctx context.Context, tx *firestore.Transaction) error {
		snap, err := tx.Get(ref)
		if err != nil && status.Code(err) != codes.NotFound {
			return err
		}
		if err == nil {
			if activeLock(snap.Data()[rebuildIndexLockKey], now) {
				return errRebuildIndexLocked
			}
		}
		return tx.Set(ref, map[string]interface{}{
			rebuildIndexLockKey: map[string]interface{}{
				"status":     "active",
				"expires_at": expiresAt,
			},
		}, firestore.MergeAll)
	})
}

func releaseRebuildIndexLock(ctx context.Context, fs *firestore.Client, uid, pid string) error {
	ref := fs.Collection("locks").Doc(fmt.Sprintf("%s__%s", uid, pid))
	return fs.RunTransaction(ctx, func(ctx context.Context, tx *firestore.Transaction) error {
		if _, err := tx.Get(ref); err != nil {
			if status.Code(err) == codes.NotFound {
				return nil
			}
			return err
		}
		return tx.Update(ref, []firestore.Update{{Path: rebuildIndexLockKey, Value: firestore.Delete}})
	})
}

func activeLock(value interface{}, now time.Time) bool {
	lock, ok := value.(map[string]interface{})
	if !ok {
		return false
	}
	statusValue, _ := lock["status"].(string)
	expiresAt, ok := firestoreTimestamp(lock["expires_at"])
	return statusValue == "active" && ok && expiresAt.After(now)
}

func firestoreTimestamp(value interface{}) (time.Time, bool) {
	switch t := value.(type) {
	case time.Time:
		return t, true
	case *timestamppb.Timestamp:
		if t == nil {
			return time.Time{}, false
		}
		return t.AsTime(), true
	case timestamppb.Timestamp:
		return t.AsTime(), true
	default:
		return time.Time{}, false
	}
}
