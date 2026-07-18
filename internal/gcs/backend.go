package gcs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"cloud.google.com/go/storage"
	"github.com/rayer/llm-wiki-bff/internal/generation"
	store "github.com/rayer/llm-wiki-bff/internal/storage"
	"google.golang.org/api/iterator"
)

// clientBackend is a deliberately small, per-Client seam used by adapter tests.
// Production clients leave it nil and use the GCS bucket directly.
type clientBackend interface {
	Read(context.Context, string, int64, int64) (backendObject, error)
	Attrs(context.Context, string, int64) (backendObject, error)
	List(context.Context, string, bool, func(backendObject) error) error
	Write(context.Context, string, []byte, string, map[string]string, writeCondition) (backendObject, error)
	Delete(context.Context, string, int64) error
}

// writeCondition is intentionally distinct for create-only and generation CAS:
// GCS does not treat GenerationMatch(0) as DoesNotExist.
type writeCondition struct {
	DoesNotExist    bool
	GenerationMatch *int64
}

func createOrGenerationCondition(expected int64) writeCondition {
	if expected == 0 {
		return writeCondition{DoesNotExist: true}
	}
	return writeCondition{GenerationMatch: &expected}
}

func gcsConditions(condition writeCondition) storage.Conditions {
	if condition.DoesNotExist {
		return storage.Conditions{DoesNotExist: true}
	}
	if condition.GenerationMatch != nil {
		return storage.Conditions{GenerationMatch: *condition.GenerationMatch}
	}
	return storage.Conditions{}
}

type backendObject struct {
	Name       string
	Data       []byte
	Generation int64
	Size       int64
	Metadata   map[string]string
	Updated    time.Time
}

func (c *Client) readObject(ctx context.Context, name string, generation, limit int64) (backendObject, error) {
	if limit < 0 {
		return backendObject{}, errors.New("object exceeds input limit")
	}
	if c.backend != nil {
		object, err := c.backend.Read(ctx, name, generation, limit)
		if err != nil || object.Size < 0 || object.Size > limit || int64(len(object.Data)) != object.Size || int64(len(object.Data)) > limit {
			if err != nil {
				return backendObject{}, err
			}
			return backendObject{}, errors.New("object size does not match attributes")
		}
		return object, nil
	}
	obj := c.bucket.Object(name)
	if generation > 0 {
		obj = obj.Generation(generation)
	}
	r, err := obj.NewReader(ctx)
	if err != nil {
		return backendObject{}, err
	}
	defer r.Close()
	if r.Attrs.Size < 0 || r.Attrs.Size > limit {
		return backendObject{}, errors.New("object size does not match attributes")
	}
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return backendObject{}, err
	}
	if int64(len(data)) != r.Attrs.Size || int64(len(data)) > limit {
		return backendObject{}, errors.New("object exceeds input limit")
	}
	return backendObject{Name: name, Data: data, Generation: r.Attrs.Generation, Size: r.Attrs.Size}, nil
}

func (c *Client) objectAttrs(ctx context.Context, name string, generation int64) (backendObject, error) {
	if c.backend != nil {
		return c.backend.Attrs(ctx, name, generation)
	}
	obj := c.bucket.Object(name)
	if generation > 0 {
		obj = obj.Generation(generation)
	}
	attrs, err := obj.Attrs(ctx)
	if err != nil {
		return backendObject{}, err
	}
	return backendObject{Name: attrs.Name, Generation: attrs.Generation, Size: attrs.Size, Metadata: attrs.Metadata, Updated: attrs.Updated.UTC()}, nil
}

func (c *Client) listObjects(ctx context.Context, prefix string) ([]backendObject, error) {
	objects := make([]backendObject, 0)
	err := c.visitObjects(ctx, prefix, func(object backendObject) error {
		objects = append(objects, object)
		return nil
	})
	return objects, err
}

func (c *Client) visitObjects(ctx context.Context, prefix string, visit func(backendObject) error) error {
	return c.visitObjectsWithDepth(ctx, prefix, false, visit)
}

func (c *Client) visitDirectObjects(ctx context.Context, prefix string, visit func(backendObject) error) error {
	return c.visitObjectsWithDepth(ctx, prefix, true, visit)
}

func (c *Client) visitObjectsWithDepth(ctx context.Context, prefix string, directOnly bool, visit func(backendObject) error) error {
	return c.visitObjectsWithBudget(ctx, prefix, directOnly, generation.MaxFiles, generation.MaxTotalSize, errors.New("object list exceeds limit"), errors.New("object list exceeds limit"), visit)
}

func (c *Client) visitObjectsWithBudget(ctx context.Context, prefix string, directOnly bool, maxObjects int, maxBytes int64, countErr, bytesErr error, visit func(backendObject) error) error {
	count := 0
	var total int64
	boundedVisit := func(object backendObject) error {
		if object.Size < 0 || object.Size > maxBytes || total > maxBytes-object.Size {
			return bytesErr
		}
		if count >= maxObjects {
			return countErr
		}
		count++
		total += object.Size
		return visit(object)
	}
	return c.visitObjectsRaw(ctx, prefix, directOnly, boundedVisit)
}

func (c *Client) visitObjectsRaw(ctx context.Context, prefix string, directOnly bool, visit func(backendObject) error) error {
	if c.backend != nil {
		return c.backend.List(ctx, prefix, directOnly, visit)
	}
	query := &storage.Query{Prefix: prefix}
	if directOnly {
		query.Delimiter = "/"
	}
	it := c.bucket.Objects(ctx, query)
	for {
		attrs, err := it.Next()
		if errors.Is(err, iterator.Done) {
			return nil
		}
		if err != nil {
			return err
		}
		if attrs.Name == "" {
			continue
		}
		if err := visit(backendObject{Name: attrs.Name, Generation: attrs.Generation, Size: attrs.Size, Metadata: attrs.Metadata, Updated: attrs.Updated.UTC()}); err != nil {
			return err
		}
	}
}

func (c *Client) writeObject(ctx context.Context, name string, data []byte, contentType string, metadata map[string]string, condition writeCondition) (backendObject, error) {
	if c.backend != nil {
		return c.backend.Write(ctx, name, data, contentType, metadata, condition)
	}
	obj := c.bucket.Object(name)
	if condition.DoesNotExist || condition.GenerationMatch != nil {
		obj = obj.If(gcsConditions(condition))
	}
	w := obj.NewWriter(ctx)
	w.ContentType = contentType
	w.Metadata = metadata
	if _, err := w.Write(data); err != nil {
		closeErr := w.Close()
		if errors.Is(conditionalWriteError(err), store.ErrGenerationMismatch) || errors.Is(conditionalWriteError(closeErr), store.ErrGenerationMismatch) {
			return backendObject{}, store.ErrGenerationMismatch
		}
		return backendObject{}, err
	}
	if err := w.Close(); err != nil {
		return backendObject{}, conditionalWriteError(err)
	}
	attrs := w.Attrs()
	if attrs == nil {
		return backendObject{}, fmt.Errorf("write %s: missing object attributes", name)
	}
	return backendObject{Name: attrs.Name, Generation: attrs.Generation, Size: attrs.Size, Metadata: attrs.Metadata, Updated: attrs.Updated.UTC()}, nil
}

func (c *Client) deleteObject(ctx context.Context, name string, objectGeneration int64) error {
	if c.backend != nil {
		return c.backend.Delete(ctx, name, objectGeneration)
	}
	object := c.bucket.Object(name)
	if objectGeneration > 0 {
		object = object.If(storage.Conditions{GenerationMatch: objectGeneration})
	}
	return conditionalWriteError(object.Delete(ctx))
}
