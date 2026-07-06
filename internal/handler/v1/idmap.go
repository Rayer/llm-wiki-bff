package v1

import (
	"context"
	"errors"
	"fmt"
	"time"

	"cloud.google.com/go/firestore"
	"cloud.google.com/go/storage"
	"github.com/rayer/llm-wiki-bff/internal/gcs"
	"github.com/rayer/llm-wiki-bff/internal/wikiindex"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	idMapPath           = wikiindex.IDMapPath
	idMapTempPath       = wikiindex.IDMapTempPath
	conceptsJSONLPath   = wikiindex.ConceptsJSONLPath
	rebuildIndexLockKey = "rebuild_index"
	rebuildIndexTTL     = 60 * time.Second
)

var errRebuildIndexLocked = errors.New("rebuild index already running")

type idMap = wikiindex.IDMap

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

func buildIDMap(ctx context.Context, store idMapStore) (idMap, error) {
	return wikiindex.BuildIDMap(ctx, legacyWikiIndexStore{reader: store})
}

func rebuildIndex(ctx context.Context, store idMapReadWriter) (idMap, error) {
	return wikiindex.Rebuild(ctx, legacyWikiIndexStore{reader: store, writer: store})
}

type legacyWikiIndexStore struct {
	reader idMapStore
	writer idMapWriter
}

func (s legacyWikiIndexStore) ListMarkdownFiles(ctx context.Context, dir string) ([]wikiindex.MarkdownFile, error) {
	files, err := s.reader.ListMarkdownFiles(ctx, dir)
	if err != nil {
		return nil, err
	}
	return convertGCSMarkdownFiles(files), nil
}

func (s legacyWikiIndexStore) ReadFile(ctx context.Context, relPath string) ([]byte, error) {
	data, err := s.reader.ReadFile(ctx, relPath)
	if errors.Is(err, storage.ErrObjectNotExist) {
		return nil, wikiindex.ErrNotFound
	}
	return data, err
}

func (s legacyWikiIndexStore) WriteBytesAtomic(ctx context.Context, data []byte, tmpPath, finalPath string) (string, error) {
	if s.writer == nil {
		return "", fmt.Errorf("wikiindex writer is not configured")
	}
	return s.writer.WriteBytesAtomic(ctx, data, tmpPath, finalPath)
}

type gcsWikiIndexStore struct {
	client *gcs.Client
}

func newGCSWikiIndexStore(client *gcs.Client) gcsWikiIndexStore {
	return gcsWikiIndexStore{client: client}
}

func (s gcsWikiIndexStore) ListMarkdownFiles(ctx context.Context, dir string) ([]wikiindex.MarkdownFile, error) {
	files, err := s.client.ListMarkdownFiles(ctx, dir)
	if err != nil {
		return nil, err
	}
	return convertGCSMarkdownFiles(files), nil
}

func (s gcsWikiIndexStore) ReadFile(ctx context.Context, relPath string) ([]byte, error) {
	data, err := s.client.ReadFile(ctx, relPath)
	if errors.Is(err, storage.ErrObjectNotExist) {
		return nil, wikiindex.ErrNotFound
	}
	return data, err
}

func (s gcsWikiIndexStore) WriteBytesAtomic(ctx context.Context, data []byte, tmpPath, finalPath string) (string, error) {
	return s.client.WriteBytesAtomic(ctx, data, tmpPath, finalPath)
}

func convertGCSMarkdownFiles(files []gcs.MarkdownFile) []wikiindex.MarkdownFile {
	converted := make([]wikiindex.MarkdownFile, len(files))
	for i, file := range files {
		converted[i] = wikiindex.MarkdownFile{
			Slug: file.Slug,
			Path: file.Path,
			Data: file.Data,
		}
	}
	return converted
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
