package main

// Cloud publishing intentionally uses objects rather than a mounted filesystem.
// The narrow interface makes the commit protocol executable with an in-memory
// backend in tests and keeps GCS details at this boundary.

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	cloudstorage "cloud.google.com/go/storage"
	"github.com/rayer/llm-wiki-bff/internal/annotation"
	"github.com/rayer/llm-wiki-bff/internal/generation"
	"github.com/rayer/llm-wiki-bff/internal/sourcestatus"
	"github.com/rayer/llm-wiki-bff/internal/storage"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
)

type objectConditions struct {
	DoesNotExist    bool
	GenerationMatch int64
}
type objectAttrs struct {
	Name             string
	Generation, Size int64
	Metadata         map[string]string
}
type objectStore interface {
	Read(context.Context, string, int64, int64) ([]byte, objectAttrs, error)
	List(context.Context, string, int) ([]objectAttrs, error)
	Write(context.Context, string, []byte, map[string]string, objectConditions) (objectAttrs, error)
	Delete(context.Context, string, int64) error
	Close() error
}

var errObjectGenerationConflict = errors.New("object generation conflict")
var errObjectNotFound = errors.New("object not found")

const cloudManifestReadbackTimeout = 3 * time.Second

var errManifestCommitOutcomeUnknown = errors.New("manifest commit outcome unknown")
var errCloudFailureRecording = errors.New("failure state recording failed")
var errCloudPipelineLogRecording = errors.New("pipeline log recording failed")
var errCloudSourceStatusRecording = errors.New("source status recording failed")
var errCloudPipelineExecution = errors.New("pipeline execution failed")
var errCloudPipelinePublish = errors.New("pipeline publish failed")
var errCloudMaterialization = errors.New("pipeline input materialization failed")
var errCloudCommittedReceipt = errors.New("pipeline committed but receipt recording failed")
var errCloudCleanup = errors.New("pipeline cleanup failed")
var errCloudCommittedCleanup = errors.New("pipeline committed but cleanup failed")

const cloudLeaseReleaseAttempts = 3

const (
	cloudPipelineCompletedEvent        = "pipeline completed\n"
	cloudPipelineFailedEvent           = "pipeline failed\n"
	cloudPipelineCommittedCleanupEvent = "pipeline committed cleanup failed\n"
)

type cloudFailureRecordingError struct {
	pipelineLog  bool
	sourceStatus bool
}

func (e cloudFailureRecordingError) Error() string { return errCloudFailureRecording.Error() }
func (e cloudFailureRecordingError) Is(target error) bool {
	return target == errCloudFailureRecording || target == errCloudPipelineLogRecording && e.pipelineLog || target == errCloudSourceStatusRecording && e.sourceStatus
}

func normalizeObjectPrecondition(err error) error {
	if errors.Is(err, cloudstorage.ErrObjectNotExist) {
		return errObjectNotFound
	}
	var apiErr *googleapi.Error
	if errors.As(err, &apiErr) {
		switch apiErr.Code {
		case 404:
			return errObjectNotFound
		case 412:
			return errObjectGenerationConflict
		}
	}
	return err
}

func isObjectNotFound(err error) bool {
	return errors.Is(err, errObjectNotFound) || errors.Is(err, cloudstorage.ErrObjectNotExist)
}

func isObjectGenerationConflict(err error) bool {
	return errors.Is(err, errObjectGenerationConflict)
}

type cloudObjectStore struct {
	bucketName string
	client     *cloudstorage.Client
	bucket     *cloudstorage.BucketHandle
}

func newCloudObjectStore(bucket string) objectStore { return &cloudObjectStore{bucketName: bucket} }
func (s *cloudObjectStore) ensure(ctx context.Context) error {
	if s.bucket != nil {
		return nil
	}
	c, err := cloudstorage.NewClient(ctx)
	if err != nil {
		return err
	}
	s.client = c
	s.bucket = c.Bucket(s.bucketName)
	return nil
}
func (s *cloudObjectStore) Read(ctx context.Context, name string, objectGeneration, limit int64) ([]byte, objectAttrs, error) {
	if err := s.ensure(ctx); err != nil {
		return nil, objectAttrs{}, err
	}
	o := s.bucket.Object(name)
	if objectGeneration > 0 {
		o = o.Generation(objectGeneration)
	}
	r, err := o.NewReader(ctx)
	if err != nil {
		return nil, objectAttrs{}, normalizeObjectPrecondition(err)
	}
	if limit < 0 || r.Attrs.Size < 0 || r.Attrs.Size > limit {
		_ = r.Close()
		return nil, objectAttrs{}, errors.New("object exceeds input limit")
	}
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		_ = r.Close()
		return nil, objectAttrs{}, err
	}
	if err := r.Close(); err != nil {
		return nil, objectAttrs{}, err
	}
	if int64(len(b)) != r.Attrs.Size || int64(len(b)) > limit {
		return nil, objectAttrs{}, errors.New("object exceeds input limit")
	}
	a := r.Attrs
	return b, objectAttrs{Name: name, Generation: a.Generation, Size: a.Size}, nil
}
func (s *cloudObjectStore) List(ctx context.Context, prefix string, max int) ([]objectAttrs, error) {
	if err := s.ensure(ctx); err != nil {
		return nil, err
	}
	if max < 0 {
		return nil, errors.New("invalid object list limit")
	}
	it := s.bucket.Objects(ctx, &cloudstorage.Query{Prefix: prefix})
	out := make([]objectAttrs, 0, min(max, 32))
	var totalSize int64
	for {
		a, err := it.Next()
		if errors.Is(err, iterator.Done) {
			return out, nil
		}
		if err != nil {
			return nil, normalizeObjectPrecondition(err)
		}
		if len(out) == max {
			return nil, errors.New("object list exceeds limit")
		}
		if a.Size < 0 || a.Size > generation.MaxTotalSize || totalSize > generation.MaxTotalSize-a.Size {
			return nil, errors.New("object list exceeds limit")
		}
		out = append(out, objectAttrs{Name: a.Name, Generation: a.Generation, Size: a.Size, Metadata: a.Metadata})
		totalSize += a.Size
	}
}
func (s *cloudObjectStore) Write(ctx context.Context, name string, data []byte, metadata map[string]string, condition objectConditions) (objectAttrs, error) {
	if err := s.ensure(ctx); err != nil {
		return objectAttrs{}, err
	}
	o := s.bucket.Object(name)
	if condition.DoesNotExist {
		o = o.If(cloudstorage.Conditions{DoesNotExist: true})
	} else if condition.GenerationMatch > 0 {
		o = o.If(cloudstorage.Conditions{GenerationMatch: condition.GenerationMatch})
	}
	w := o.NewWriter(ctx)
	w.Metadata = metadata
	if _, err := w.Write(data); err != nil {
		_ = w.Close()
		return objectAttrs{}, normalizeObjectPrecondition(err)
	}
	if err := w.Close(); err != nil {
		return objectAttrs{}, normalizeObjectPrecondition(err)
	}
	a := w.Attrs()
	return objectAttrs{Name: name, Generation: a.Generation, Size: a.Size, Metadata: a.Metadata}, nil
}
func (s *cloudObjectStore) Delete(ctx context.Context, name string, generation int64) error {
	if err := s.ensure(ctx); err != nil {
		return err
	}
	o := s.bucket.Object(name)
	if generation > 0 {
		o = o.If(cloudstorage.Conditions{GenerationMatch: generation})
	}
	return normalizeObjectPrecondition(o.Delete(ctx))
}
func (s *cloudObjectStore) Close() error {
	if s.client != nil {
		return s.client.Close()
	}
	return nil
}

type cloudLease struct {
	store      objectStore
	name       string
	generation int64
}

func acquireCloudLease(ctx context.Context, store objectStore, prefix, execution string) (*cloudLease, error) {
	payload, _ := json.Marshal(map[string]string{"execution": "redacted", "started_at": time.Now().UTC().Format(time.RFC3339)})
	a, err := store.Write(ctx, prefix+generation.LeasePath, payload, nil, objectConditions{DoesNotExist: true})
	if err != nil {
		return nil, errors.New("pipeline publish lease is held")
	}
	return &cloudLease{store: store, name: prefix + generation.LeasePath, generation: a.Generation}, nil
}
func (l *cloudLease) Release(_ context.Context) error {
	if err := storage.RetryGenerationCleanup(l.generation, generation.LeaseReleaseTimeout, cloudLeaseReleaseAttempts, func(ctx context.Context, objectGeneration int64) error {
		return l.store.Delete(ctx, l.name, objectGeneration)
	}); err != nil {
		return errCloudCleanup
	}
	return nil
}

func runCloudWorkerBatch(ctx context.Context, cfg workerConfig, commands [][]string, objects objectStore) (result error) {
	cfg.cloudMode = true
	if err := validateWorkerInput(cfg, commands); err != nil {
		return errors.New("cloud worker input is invalid")
	}
	if cfg.Bucket == "" || cfg.UserID == "" || cfg.ProjectID == "" || !cfg.Postprocess || !startsWithFullOLWRun(commands) {
		return errors.New("cloud worker configuration is invalid")
	}
	defer objects.Close()
	prefix := fmt.Sprintf("users/%s/projects/%s/", cfg.UserID, cfg.ProjectID)
	lease, err := acquireCloudLease(ctx, objects, prefix, cfg.ExecutionID)
	if err != nil {
		return errors.New("pipeline publish lease unavailable")
	}
	committed := false
	workspace := ""
	defer func() {
		if cleanupErr := lease.Release(ctx); cleanupErr != nil {
			_ = writeCloudPipelineEvent(context.Background(), objects, prefix, cfg, cloudPipelineCommittedCleanupEvent)
			if result == nil {
				if committed {
					result = errCloudCommittedCleanup
				} else {
					result = cleanupErr
				}
			} else {
				result = errors.Join(result, cleanupErr)
			}
		}
	}()
	// Cloud workers are deliberately detached from DATA_DIR, WORKSPACE and any
	// mount. Their private work area is always local /tmp.
	workspace, err = os.MkdirTemp("/tmp", "olw-cloud-")
	if err != nil {
		return errors.New("pipeline workspace unavailable")
	}
	defer os.RemoveAll(workspace)
	snapshots, manifestData, manifestAttrs, err := materializeCloudWorkspace(ctx, objects, prefix, workspace)
	if err != nil {
		if recordErr := writeCloudFailureReceipts(ctx, objects, prefix, workspace, cfg, snapshots); recordErr != nil {
			return errors.Join(errCloudMaterialization, recordErr)
		}
		return errCloudMaterialization
	}
	// Capture concept IDs from the immediately prior committed/materialized
	// workspace id_map before OLW regenerates transient concept identities.
	priorConcepts, err := snapshotConcepts(workspace)
	if err != nil {
		if recordErr := writeCloudFailureReceipts(ctx, objects, prefix, workspace, cfg, snapshots); recordErr != nil {
			return errors.Join(errCloudMaterialization, recordErr)
		}
		return errCloudMaterialization
	}
	if err := materializeSnapshots(workspace, snapshots); err != nil {
		if recordErr := writeCloudFailureReceipts(ctx, objects, prefix, workspace, cfg, snapshots); recordErr != nil {
			return errors.Join(errCloudMaterialization, recordErr)
		}
		return errCloudMaterialization
	}
	cfg.VaultPath = workspace
	cfg.Workspace = false
	cfg.SuppressOutput = true
	if err := runWorkerBatchAtVault(ctx, cfg, commands, workspace); err != nil {
		primary := errCloudPipelineExecution
		if recordErr := writeCloudFailureReceipts(ctx, objects, prefix, workspace, cfg, snapshots); recordErr != nil {
			return errors.Join(primary, recordErr)
		}
		return primary
	}
	if err := reconcileWorkspaceSources(workspace, snapshots); err != nil {
		if recordErr := writeCloudFailureReceipts(ctx, objects, prefix, workspace, cfg, snapshots); recordErr != nil {
			return errors.Join(errCloudPipelinePublish, recordErr)
		}
		return errCloudPipelinePublish
	}
	if err := reconcileWorkspaceConcepts(workspace, priorConcepts, snapshots); err != nil {
		if recordErr := writeCloudFailureReceipts(ctx, objects, prefix, workspace, cfg, snapshots); recordErr != nil {
			return errors.Join(errCloudPipelinePublish, recordErr)
		}
		return errCloudPipelinePublish
	}
	if _, _, err := publishCloudGenerationFromStart(ctx, objects, prefix, workspace, snapshots, manifestData, manifestAttrs, manifestAttrs.Generation > 0); err != nil {
		if errors.Is(err, errManifestCommitOutcomeUnknown) {
			return errManifestCommitOutcomeUnknown
		}
		primary := errCloudPipelinePublish
		if recordErr := writeCloudFailureReceipts(ctx, objects, prefix, workspace, cfg, snapshots); recordErr != nil {
			return errors.Join(primary, recordErr)
		}
		return primary
	}
	committed = true
	if err := writeCloudReceipts(ctx, objects, prefix, workspace, cfg, snapshots); err != nil {
		return errCloudCommittedReceipt
	}
	return nil
}

func materializeCloudWorkspace(ctx context.Context, objects objectStore, prefix, workspace string) ([]sourceSnapshot, []byte, objectAttrs, error) {
	budget := cloudMaterializationBudget{}
	snapshots, err := materializeCanonicalCloudInputs(ctx, objects, prefix, workspace, &budget)
	if err != nil {
		return snapshots, nil, objectAttrs{}, err
	}
	reservations := cloudTombstones(snapshots)
	manifestData, manifestAttrs, err := objects.Read(ctx, prefix+generation.ManifestPath, 0, generation.MaxManifestBytes)
	manifestExists := err == nil
	if err != nil && !isObjectNotFound(err) {
		return snapshots, nil, objectAttrs{}, err
	}
	if manifestExists {
		m, err := generation.Decode(manifestData)
		if err != nil {
			return snapshots, nil, objectAttrs{}, err
		}
		for _, f := range m.Files {
			b, a, err := readCloudMaterializedObject(ctx, objects, prefix+m.ObjectPath(f), f.Generation, f.Size, &budget)
			if err != nil || a.Size != f.Size || digestBytes(b) != f.SHA256 {
				return snapshots, nil, objectAttrs{}, errors.New("generation object fails manifest validation")
			}
			if err := writeCloudFile(workspace, f.Path, b); err != nil {
				return snapshots, nil, objectAttrs{}, err
			}
		}
	} else if err := materializeLegacyCloudOutputs(ctx, objects, prefix, workspace, &budget); err != nil {
		return snapshots, nil, objectAttrs{}, err
	}
	err = nil
	if len(snapshots) == 0 {
		mapped, mapErr := snapshotSources(workspace)
		if mapErr == nil && len(mapped) > 0 {
			snapshots = mapped
		} else if mapErr != nil {
			err = mapErr
		}
	} else if mapped, mapErr := snapshotSources(workspace); mapErr != nil {
		err = mapErr
	} else if len(mapped) > 0 {
		snapshots = appendCloudReservations(mapped, reservations)
	}
	return snapshots, manifestData, manifestAttrs, err
}

func cloudTombstones(snapshots []sourceSnapshot) []sourceSnapshot {
	reservations := make([]sourceSnapshot, 0)
	for _, snapshot := range snapshots {
		if snapshot.Tombstone {
			reservations = append(reservations, snapshot)
		}
	}
	return reservations
}

func appendCloudReservations(snapshots, reservations []sourceSnapshot) []sourceSnapshot {
	seen := make(map[string]struct{}, len(snapshots)+len(reservations))
	for _, snapshot := range snapshots {
		seen[snapshot.SourceID+"\x00"+snapshot.RawPath] = struct{}{}
	}
	for _, reservation := range reservations {
		key := reservation.SourceID + "\x00" + reservation.RawPath
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		snapshots = append(snapshots, reservation)
	}
	return snapshots
}

type cloudMaterializationBudget struct {
	objects int
	bytes   int64
}

func (b *cloudMaterializationBudget) reserve(size int64) error {
	if size < 0 || size > generation.MaxFileBytes || b.objects >= generation.MaxFiles || b.bytes > generation.MaxTotalSize-size {
		return errors.New("cloud materialization exceeds limit")
	}
	b.objects++
	b.bytes += size
	return nil
}

func readCloudMaterializedObject(ctx context.Context, objects objectStore, name string, generationID, expectedSize int64, budget *cloudMaterializationBudget) ([]byte, objectAttrs, error) {
	if err := budget.reserve(expectedSize); err != nil {
		return nil, objectAttrs{}, err
	}
	limit := expectedSize
	if limit > generation.MaxFileBytes {
		limit = generation.MaxFileBytes
	}
	b, attrs, err := objects.Read(ctx, name, generationID, limit)
	if err != nil || attrs.Size != expectedSize || int64(len(b)) != expectedSize {
		return nil, objectAttrs{}, errors.New("cloud object read failed")
	}
	return b, attrs, nil
}

func materializeCloudObjects(ctx context.Context, objects objectStore, prefix, workspace, objectPrefix string, budget *cloudMaterializationBudget, keep func(string) bool) error {
	remaining := generation.MaxFiles - budget.objects
	attrs, err := objects.List(ctx, prefix+objectPrefix, remaining)
	if err != nil {
		return err
	}
	for _, attr := range attrs {
		rel := strings.TrimPrefix(attr.Name, prefix)
		if !strings.HasPrefix(attr.Name, prefix+objectPrefix) || !keep(rel) {
			continue
		}
		b, _, err := readCloudMaterializedObject(ctx, objects, attr.Name, 0, attr.Size, budget)
		if err != nil {
			return err
		}
		if err := writeCloudFile(workspace, rel, b); err != nil {
			return err
		}
	}
	return nil
}

func materializeCanonicalCloudInputs(ctx context.Context, objects objectStore, prefix, workspace string, budget *cloudMaterializationBudget) ([]sourceSnapshot, error) {
	if err := materializeCloudObjects(ctx, objects, prefix, workspace, "raw/", budget, func(rel string) bool { return storage.SafeRawPath(rel) }); err != nil {
		return nil, err
	}
	if err := materializeCloudObjects(ctx, objects, prefix, workspace, "cache/annotations/", budget, func(rel string) bool {
		name := strings.TrimSuffix(strings.TrimPrefix(rel, "cache/annotations/"), ".json")
		return strings.HasSuffix(rel, ".json") && annotation.ValidSourceID(name)
	}); err != nil {
		return nil, err
	}
	data, attrs, err := objects.Read(ctx, prefix+sourcestatus.Path, 0, generation.MaxFileBytes)
	if isObjectNotFound(err) {
		return []sourceSnapshot{}, nil
	}
	if err != nil {
		return nil, err
	}
	if err := budget.reserve(attrs.Size); err != nil || int64(len(data)) != attrs.Size {
		return nil, errors.New("cloud object read failed")
	}
	if err := writeCloudFile(workspace, sourcestatus.Path, data); err != nil {
		return nil, err
	}
	snapshots, err := snapshotCanonicalCloudSources(workspace, data)
	return snapshots, err
}

func snapshotCanonicalCloudSources(workspace string, data []byte) ([]sourceSnapshot, error) {
	artifact, err := sourcestatus.Decode(data)
	if err != nil || artifact.Version != 1 {
		return nil, errors.New("invalid source status")
	}
	snapshots := make([]sourceSnapshot, 0, len(artifact.Sources))
	for sourceID, receipt := range artifact.Sources {
		if !validCloudReceipt(sourceID, receipt) {
			return nil, errors.New("invalid source status")
		}
		raw, err := readRegularFileWithin(workspace, receipt.RawPath)
		if errors.Is(err, os.ErrNotExist) {
			snapshots = append(snapshots, sourceSnapshot{SourceID: sourceID, RawPath: receipt.RawPath, Tombstone: true})
			continue
		}
		if err != nil {
			return nil, err
		}
		ann, err := readAnnotation(workspace, sourceID, receipt.RawPath)
		if err != nil {
			return nil, err
		}
		rawSum := sha256.Sum256(raw)
		rawSHA := fmt.Sprintf("%x", rawSum[:])
		fingerprint := sourcestatus.Fingerprint(rawSHA, ann.SHA256)
		snapshot := sourceSnapshot{
			SourceID: sourceID, RawPath: receipt.RawPath, RawBytes: raw, RawSHA256: rawSHA,
			AnnotationBody: ann.Body, AnnotationSHA: ann.SHA256, Fingerprint: fingerprint,
			Dirty: !sourcestatus.ValidReceipt(receipt, receipt.RawPath) || receipt.LastIngestFingerprint != fingerprint,
		}
		snapshot.SyntoContentHash = syntoSourceContentHash(snapshot)
		snapshots = append(snapshots, snapshot)
	}
	sort.Slice(snapshots, func(i, j int) bool { return snapshots[i].SourceID < snapshots[j].SourceID })
	return snapshots, nil
}

func materializeLegacyCloudOutputs(ctx context.Context, objects objectStore, prefix, workspace string, budget *cloudMaterializationBudget) error {
	for _, entry := range []struct {
		prefix string
		keep   func(string) bool
	}{
		{"wiki/", generation.GenerationOwned},
		{"cache/", func(rel string) bool { return generation.GenerationOwned(rel) && rel != sourcestatus.Path }},
		{".olw/", generation.GenerationOwned},
		{".synto/", generation.GenerationOwned},
	} {
		if err := materializeCloudObjects(ctx, objects, prefix, workspace, entry.prefix, budget, entry.keep); err != nil {
			return err
		}
	}
	for _, config := range []string{"wiki.toml", "synto.toml"} {
		data, attrs, err := objects.Read(ctx, prefix+config, 0, generation.MaxFileBytes)
		if isObjectNotFound(err) {
			continue
		}
		if err != nil || budget.reserve(attrs.Size) != nil || int64(len(data)) != attrs.Size {
			return errors.New("cloud object read failed")
		}
		if err := writeCloudFile(workspace, config, data); err != nil {
			return err
		}
	}
	return nil
}
func writeCloudFile(root, rel string, b []byte) error {
	if err := safeRelativePath(rel); err != nil {
		return err
	}
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
		return err
	}
	return os.WriteFile(p, b, 0644)
}

func publishCloudGeneration(ctx context.Context, objects objectStore, prefix, workspace string, snapshots []sourceSnapshot) (generation.Manifest, int64, error) {
	files, err := preflightGenerationOutputs(workspace)
	if err != nil {
		return generation.Manifest{}, 0, fmt.Errorf("generation output validation failed: %w", err)
	}
	oldData, oldAttrs, oldErr := objects.Read(ctx, prefix+generation.ManifestPath, 0, generation.MaxManifestBytes)
	if oldErr != nil && !isObjectNotFound(oldErr) {
		return generation.Manifest{}, 0, oldErr
	}
	return publishCloudGenerationWithFiles(ctx, objects, prefix, workspace, snapshots, files, oldData, oldAttrs, oldErr == nil)
}
func publishCloudGenerationFromStart(ctx context.Context, objects objectStore, prefix, workspace string, snapshots []sourceSnapshot, oldData []byte, oldAttrs objectAttrs, oldExists bool) (generation.Manifest, int64, error) {
	files, err := preflightGenerationOutputs(workspace)
	if err != nil {
		return generation.Manifest{}, 0, fmt.Errorf("generation output validation failed: %w", err)
	}
	return publishCloudGenerationWithFiles(ctx, objects, prefix, workspace, snapshots, files, oldData, oldAttrs, oldExists)
}
func publishCloudGenerationWithFiles(ctx context.Context, objects objectStore, prefix, workspace string, snapshots []sourceSnapshot, files []generationOutput, oldData []byte, oldAttrs objectAttrs, oldExists bool) (generation.Manifest, int64, error) {
	var old generation.Manifest
	if oldExists {
		var err error
		old, err = generation.Decode(oldData)
		if err != nil {
			return generation.Manifest{}, 0, err
		}
	}
	id, err := newGenerationID()
	if err != nil {
		return generation.Manifest{}, 0, err
	}
	m := generation.Manifest{Version: generation.Version, GenerationID: id, CreatedAt: time.Now().UTC().Format(time.RFC3339), InputFingerprint: snapshotFingerprint(snapshots)}
	if old.GenerationID != "" {
		m.PreviousGenerationID = old.GenerationID
	}
	for _, file := range files {
		b, err := os.ReadFile(filepath.Join(workspace, filepath.FromSlash(file.path)))
		if err != nil {
			return generation.Manifest{}, 0, errors.New("generation output read failed")
		}
		digest := digestBytes(b)
		if int64(len(b)) != file.size || digest != file.sha256 {
			return generation.Manifest{}, 0, errors.New("generation output changed after validation")
		}
		a, err := objects.Write(ctx, prefix+generation.Prefix+id+"/"+file.path, b, map[string]string{"sha256": digest}, objectConditions{DoesNotExist: true})
		if err != nil || a.Size != file.size || a.Metadata["sha256"] != file.sha256 || a.Generation <= 0 {
			return generation.Manifest{}, 0, errors.New("immutable generation upload failed")
		}
		f := generation.File{Path: file.path, Size: file.size, SHA256: file.sha256, Generation: a.Generation}
		m.Files = append(m.Files, f)
	}
	sort.Slice(m.Files, func(i, j int) bool { return m.Files[i].Path < m.Files[j].Path })
	if err := m.Validate(); err != nil {
		return generation.Manifest{}, 0, err
	}
	data, _ := json.Marshal(m)
	condition := objectConditions{DoesNotExist: true}
	if oldExists {
		condition = objectConditions{GenerationMatch: oldAttrs.Generation}
	}
	a, err := objects.Write(ctx, prefix+generation.ManifestPath, data, map[string]string{"sha256": digestBytes(data)}, condition)
	if err != nil {
		if committed, attrs, outcome := confirmManifestCommit(objects, prefix, data, m); outcome == nil && committed {
			return m, attrs.Generation, nil
		} else if errors.Is(outcome, errManifestCommitOutcomeUnknown) {
			return generation.Manifest{}, 0, errManifestCommitOutcomeUnknown
		}
		return generation.Manifest{}, 0, errors.New("generation manifest commit conflicted")
	}
	return m, a.Generation, nil
}

func confirmManifestCommit(objects objectStore, prefix string, proposed []byte, manifest generation.Manifest) (bool, objectAttrs, error) {
	ctx, cancel := context.WithTimeout(context.Background(), cloudManifestReadbackTimeout)
	defer cancel()
	data, attrs, err := objects.Read(ctx, prefix+generation.ManifestPath, 0, generation.MaxManifestBytes)
	if err != nil {
		if isObjectNotFound(err) {
			return false, objectAttrs{}, nil
		}
		return false, objectAttrs{}, errManifestCommitOutcomeUnknown
	}
	if bytes.Equal(data, proposed) {
		return true, attrs, nil
	}
	readback, err := generation.Decode(data)
	if err != nil || !reflect.DeepEqual(readback, manifest) {
		return false, objectAttrs{}, nil
	}
	return true, attrs, nil
}

type generationOutput struct {
	path   string
	size   int64
	sha256 string
}

var walkGenerationDir = filepath.WalkDir

type generationTraversalLimits struct {
	entries, directories, depth, symlinks, nonRegular int
	bytes                                             int64
}

const (
	maxGenerationWorkspaceEntries     = generation.MaxFiles + 128
	maxGenerationWorkspaceDirectories = generation.MaxFiles
	maxGenerationWorkspaceDepth       = 64
)

var defaultGenerationTraversalLimits = generationTraversalLimits{
	entries:     maxGenerationWorkspaceEntries,
	directories: maxGenerationWorkspaceDirectories,
	depth:       maxGenerationWorkspaceDepth,
	bytes:       generation.MaxTotalSize,
}

// preflightGenerationOutputs creates the complete bounded file table before
// the first immutable object is written. It deliberately never includes raw,
// annotations, status or diagnostics in the generation.
func preflightGenerationOutputs(root string) ([]generationOutput, error) {
	return preflightGenerationOutputsWithLimits(root, defaultGenerationTraversalLimits)
}

func preflightGenerationOutputsWithLimits(root string, limits generationTraversalLimits) ([]generationOutput, error) {
	var files []generationOutput
	seen := map[string]bool{}
	var total, traversedBytes int64
	entries, directories, symlinks, nonRegular := 0, 0, 0, 0
	err := walkGenerationDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == root {
			return nil
		}
		entries++
		if entries > limits.entries {
			return errors.New("workspace traversal exceeds entry limit")
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		if depth := len(strings.Split(rel, "/")); depth > limits.depth {
			return errors.New("workspace traversal exceeds depth limit")
		}
		if err := safeRelativePath(rel); err != nil || !generation.GenerationOwned(rel) && !d.IsDir() && d.Type()&os.ModeSymlink == 0 {
			// Non-owned canonical inputs are permitted in the private workspace;
			// unsafe entries are not.
			if err != nil {
				return err
			}
		}
		if d.Type()&os.ModeSymlink != 0 {
			symlinks++
			if symlinks > limits.symlinks {
				return errors.New("workspace traversal contains too many symlinks")
			}
			return errors.New("generation contains symlink")
		}
		if d.IsDir() {
			directories++
			if directories > limits.directories {
				return errors.New("workspace traversal exceeds directory limit")
			}
			return nil
		}
		if !d.Type().IsRegular() {
			nonRegular++
			if nonRegular > limits.nonRegular {
				return errors.New("workspace traversal contains too many non-regular entries")
			}
			return errors.New("generation contains special file")
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Size() < 0 || traversedBytes > limits.bytes-info.Size() {
			return errors.New("workspace traversal exceeds byte limit")
		}
		traversedBytes += info.Size()
		if !generation.GenerationOwned(rel) {
			return nil
		}
		if info.Size() > generation.MaxFileBytes {
			return errors.New("generation output too large")
		}
		// Stop at MaxFiles+1 before hashing or appending another output; a child
		// can otherwise force an unbounded in-memory file table.
		if len(files) >= generation.MaxFiles {
			return errors.New("too many generation files")
		}
		total += info.Size()
		if total > generation.MaxTotalSize {
			return errors.New("generation output too large")
		}
		digest, err := digestGenerationFile(p, info.Size())
		if err != nil {
			return err
		}
		files = append(files, generationOutput{path: rel, size: info.Size(), sha256: digest})
		seen[rel] = true
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, required := range []string{"synto.toml", "cache/id_map.json", "cache/concepts.jsonl", "cache/dormant_concepts.jsonl", "cache/raw_status.json", "cache/suggested_queries.json", ".synto/state.db", ".synto/INDEX.json"} {
		if !seen[required] {
			return nil, errors.New("generation output is incomplete")
		}
	}
	legacyConfig, legacyState := seen["wiki.toml"], seen[".olw/state.db"]
	if legacyConfig != legacyState {
		return nil, errors.New("migrated generation rollback artifacts are incomplete")
	}
	for _, state := range []string{".synto/state.db", ".olw/state.db"} {
		if seen[state] {
			if err := validateSQLiteArtifact(root, state); err != nil {
				return nil, fmt.Errorf("invalid %s: %w", state, err)
			}
		}
	}
	if _, err := readSyntoIndexTruth(root); err != nil {
		return nil, fmt.Errorf("invalid .synto/INDEX.json: %w", err)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].path < files[j].path })
	return files, nil
}
func generationOutputFiles(root string) ([]string, error) {
	files, err := preflightGenerationOutputs(root)
	if err != nil {
		return nil, err
	}
	paths := make([]string, len(files))
	for i, f := range files {
		paths[i] = f.path
	}
	return paths, nil
}
func digestGenerationFile(path string, size int64) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil || n != size {
		return "", errors.New("generation output digest failed")
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
func newGenerationID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "g_" + hex.EncodeToString(b), nil
}
func digestBytes(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }
func snapshotFingerprint(s []sourceSnapshot) string {
	h := sha256.New()
	for _, x := range s {
		if x.Tombstone {
			continue
		}
		_, _ = io.WriteString(h, x.SourceID+"\x00"+x.Fingerprint+"\n")
	}
	return hex.EncodeToString(h.Sum(nil))
}
func writeCloudReceipts(ctx context.Context, objects objectStore, prefix, workspace string, cfg workerConfig, snapshots []sourceSnapshot) error {
	if err := writeCloudPipelineLog(ctx, objects, prefix, workspace, cfg); err != nil {
		return err
	}
	return mergeCloudSuccess(ctx, objects, prefix, snapshots)
}
func writeCloudFailureReceipts(ctx context.Context, objects objectStore, prefix, workspace string, cfg workerConfig, snapshots []sourceSnapshot) error {
	logErr := writeCloudPipelineEvent(ctx, objects, prefix, cfg, cloudPipelineFailedEvent)
	statusErr := mergeCloudFailure(ctx, objects, prefix, snapshots)
	if logErr == nil && statusErr == nil {
		return nil
	}
	return cloudFailureRecordingError{pipelineLog: logErr != nil, sourceStatus: statusErr != nil}
}
func writeCloudPipelineLog(ctx context.Context, objects objectStore, prefix, workspace string, cfg workerConfig) error {
	return writeCloudPipelineEvent(ctx, objects, prefix, cfg, cloudPipelineCompletedEvent)
}

func writeCloudPipelineEvent(ctx context.Context, objects objectStore, prefix string, cfg workerConfig, event string) error {
	if !validPipelineExecutionID(cfg.ExecutionID) {
		return nil
	}
	if event != cloudPipelineCompletedEvent && event != cloudPipelineFailedEvent && event != cloudPipelineCommittedCleanupEvent {
		return errors.New("invalid pipeline event")
	}
	_, err := objects.Write(ctx, prefix+"cache/pipeline-"+cfg.ExecutionID+".log", []byte(event), nil, objectConditions{})
	return err
}
func mergeCloudSuccess(ctx context.Context, objects objectStore, prefix string, snapshots []sourceSnapshot) error {
	return mergeCloudReceipts(ctx, objects, prefix, func(artifact *sourcestatus.Artifact) {
		for _, snapshot := range snapshots {
			if snapshot.Tombstone {
				continue
			}
			if !cloudSnapshotCurrent(ctx, objects, prefix, snapshot) {
				continue
			}
			artifact.Sources[snapshot.SourceID] = sourcestatus.Receipt{RawPath: snapshot.RawPath, LastIngestedRawSHA256: snapshot.RawSHA256, LastIngestedAnnSHA256: snapshot.AnnotationSHA, LastIngestFingerprint: snapshot.Fingerprint, LastSuccessAt: time.Now().UTC().Format(time.RFC3339)}
		}
	})
}

const cloudReceiptCASAttempts = 3

func mergeCloudFailure(ctx context.Context, objects objectStore, prefix string, snapshots []sourceSnapshot) error {
	return mergeCloudReceipts(ctx, objects, prefix, func(artifact *sourcestatus.Artifact) {
		for _, s := range snapshots {
			if s.Tombstone {
				continue
			}
			if !cloudSnapshotCurrent(ctx, objects, prefix, s) {
				continue
			}
			r := artifact.Sources[s.SourceID]
			r.RawPath, r.FailedFingerprint, r.Error = s.RawPath, s.Fingerprint, "pipeline failed"
			artifact.Sources[s.SourceID] = r
		}
	})
}

func mergeCloudReceipts(ctx context.Context, objects objectStore, prefix string, merge func(*sourcestatus.Artifact)) error {
	for attempt := 0; attempt < cloudReceiptCASAttempts; attempt++ {
		data, attrs, err := objects.Read(ctx, prefix+sourcestatus.Path, 0, generation.MaxFileBytes)
		artifact := sourcestatus.Artifact{Version: 1, Sources: map[string]sourcestatus.Receipt{}}
		if err == nil {
			artifact, err = sourcestatus.Decode(data)
			if err != nil {
				return errors.New("invalid source receipt")
			}
			if err := normalizeCloudReceipts(&artifact); err != nil {
				return errors.New("invalid source receipt")
			}
		} else if !isObjectNotFound(err) {
			return errors.New("source receipt read failed")
		}
		merge(&artifact)
		out, _ := json.Marshal(artifact)
		condition := objectConditions{DoesNotExist: true}
		if err == nil {
			condition = objectConditions{GenerationMatch: attrs.Generation}
		}
		if _, err = objects.Write(ctx, prefix+sourcestatus.Path, out, nil, condition); err == nil {
			return nil
		} else if !isObjectGenerationConflict(err) {
			return errors.New("source receipt write failed")
		}
	}
	return errors.New("source receipt conflict")
}

func normalizeCloudReceipts(artifact *sourcestatus.Artifact) error {
	if artifact.Version != 1 {
		return errors.New("invalid source receipt")
	}
	if artifact.Sources == nil {
		artifact.Sources = map[string]sourcestatus.Receipt{}
	}
	seenRaw := make(map[string]string, len(artifact.Sources))
	for sourceID, receipt := range artifact.Sources {
		if !validCloudReceipt(sourceID, receipt) {
			return errors.New("invalid source receipt")
		}
		if receipt.RawPath != "" {
			if prior, exists := seenRaw[receipt.RawPath]; exists && prior != sourceID {
				return errors.New("invalid source receipt")
			}
			seenRaw[receipt.RawPath] = sourceID
		}
	}
	return nil
}

func validCloudReceipt(sourceID string, receipt sourcestatus.Receipt) bool {
	if !annotation.ValidSourceID(sourceID) || len(sourceID) > maxWorkerIDBytes || !safeMappedRawPath(receipt.RawPath) {
		return false
	}
	for _, digest := range []string{receipt.LastIngestedRawSHA256, receipt.LastIngestedAnnSHA256, receipt.LastIngestFingerprint, receipt.FailedFingerprint} {
		if digest != "" && normalizeCloudDigest(digest) == "" {
			return false
		}
	}
	if receipt.LastSuccessAt != "" && normalizeCloudTimestamp(receipt.LastSuccessAt) == "" {
		return false
	}
	if sourcestatus.ValidReceipt(receipt, receipt.RawPath) {
		return receipt.Error == "" || receipt.Error == "pipeline failed"
	}
	return receipt.LastIngestedRawSHA256 == "" && receipt.LastIngestedAnnSHA256 == "" &&
		receipt.LastIngestFingerprint == "" && receipt.LastSuccessAt == "" &&
		receipt.Error == "pipeline failed" && normalizeCloudDigest(receipt.FailedFingerprint) != ""
}

func normalizeCloudRawPath(value string) string {
	if len(value) > generation.MaxPathBytes || !safeMappedRawPath(value) {
		return ""
	}
	return value
}

func normalizeCloudDigest(value string) string {
	if value == "" || len(value) != 64 {
		return ""
	}
	if _, err := hex.DecodeString(value); err != nil {
		return ""
	}
	return strings.ToLower(value)
}

func normalizeCloudTimestamp(value string) string {
	if value == "" || len(value) > 64 {
		return ""
	}
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func cloudSnapshotCurrent(ctx context.Context, objects objectStore, prefix string, s sourceSnapshot) bool {
	raw, _, err := objects.Read(ctx, prefix+s.RawPath, 0, generation.MaxFileBytes)
	if err != nil || digestBytes(raw) != s.RawSHA256 {
		return false
	}
	b, _, err := objects.Read(ctx, prefix+annotation.Path(s.SourceID), 0, generation.MaxFileBytes)
	if isObjectNotFound(err) {
		return s.AnnotationSHA == annotation.Digest("")
	}
	if err != nil {
		return false
	}
	var a annotation.Object
	return json.Unmarshal(b, &a) == nil && a.Validate(s.SourceID, s.RawPath) == nil && a.SHA256 == s.AnnotationSHA
}
