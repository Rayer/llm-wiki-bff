package sourcelifecycle

import (
	"testing"
	"time"

	"github.com/rayer/llm-wiki-bff/internal/annotation"
	"github.com/rayer/llm-wiki-bff/internal/rawstatus"
	"github.com/rayer/llm-wiki-bff/internal/sourcestatus"
	"github.com/rayer/llm-wiki-bff/internal/storage"
)

func TestCalculateUnionAndPrecedence(t *testing.T) {
	rawHash := annotation.Digest("raw")
	raw := storage.RawFile{Name: "one.md", Path: "raw/one.md", SHA256: rawHash}
	ann := annotation.Digest("note")
	base := storage.WikiPage{ID: "id", RawPath: raw.Path}
	receipt := func(rawHash, annHash string) sourcestatus.Receipt {
		return sourcestatus.Receipt{RawPath: raw.Path, LastIngestedRawSHA256: rawHash, LastIngestedAnnSHA256: annHash, LastIngestFingerprint: sourcestatus.Fingerprint(rawHash, annHash), LastSuccessAt: time.Now().UTC().Format(time.RFC3339)}
	}
	for _, tc := range []struct {
		name, want string
		receipt    sourcestatus.Receipt
	}{
		{"error", "error", func() sourcestatus.Receipt {
			r := receipt(annotation.Digest("old"), ann)
			r.FailedFingerprint = sourcestatus.Fingerprint(raw.SHA256, ann)
			r.Error = "worker failed"
			return r
		}()},
		{"raw", "content_pending", receipt(annotation.Digest("old"), ann)},
		{"annotation", "notes_pending", receipt(raw.SHA256, annotation.Digest("old"))},
		{"synced", "synced", receipt(raw.SHA256, ann)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pages, _ := Calculate(Input{Sources: []storage.WikiPage{base}, RawFiles: []storage.RawFile{raw}, Annotations: map[string]storage.ObjectMeta{"id": {SHA256: ann, HasAnnotation: true, Updated: time.Now()}}, Receipts: map[string]sourcestatus.Receipt{"id": tc.receipt}})
			if pages[0].LifecycleStatus != tc.want {
				t.Fatalf("status = %s", pages[0].LifecycleStatus)
			}
		})
	}
	pages, counts := Calculate(Input{Sources: []storage.WikiPage{base}, RawFiles: []storage.RawFile{raw, {Name: "new.md", Path: "raw/new.md", SHA256: "new"}}})
	if len(pages) != 2 || pages[1].ID != "" || pages[1].AnnotationAllowed || counts.NewRaw != 2 {
		t.Fatalf("pages=%+v counts=%+v", pages, counts)
	}
}

func TestInvalidReceiptUsesLegacyStatus(t *testing.T) {
	raw := storage.RawFile{Name: "one.md", Path: "raw/one.md", SHA256: "raw"}
	base := storage.WikiPage{ID: "id", RawPath: raw.Path}
	legacy := map[string]rawstatus.FileStatus{"one.md": {Path: raw.Path, OLWStatus: "compiled", Ingested: true}}
	for name, receipt := range map[string]sourcestatus.Receipt{
		"partial":         {RawPath: raw.Path},
		"wrong path":      {RawPath: "raw/other.md", LastIngestedRawSHA256: "raw", LastIngestedAnnSHA256: annotation.Digest(""), LastIngestFingerprint: "bad", LastSuccessAt: time.Now().UTC().Format(time.RFC3339)},
		"bad fingerprint": {RawPath: raw.Path, LastIngestedRawSHA256: "raw", LastIngestedAnnSHA256: annotation.Digest(""), LastIngestFingerprint: "bad", LastSuccessAt: time.Now().UTC().Format(time.RFC3339)},
		"bad timestamp":   {RawPath: raw.Path, LastIngestedRawSHA256: "raw", LastIngestedAnnSHA256: annotation.Digest(""), LastIngestFingerprint: sourcestatus.Fingerprint("raw", annotation.Digest("")), LastSuccessAt: "bad"},
	} {
		t.Run(name, func(t *testing.T) {
			pages, _ := Calculate(Input{Sources: []storage.WikiPage{base}, RawFiles: []storage.RawFile{raw}, Receipts: map[string]sourcestatus.Receipt{"id": receipt}, Legacy: legacy})
			if pages[0].LifecycleStatus != "synced" {
				t.Fatalf("legacy fallback status = %s", pages[0].LifecycleStatus)
			}
		})
	}
	pages, _ := Calculate(Input{Sources: []storage.WikiPage{base}, RawFiles: []storage.RawFile{raw}, Legacy: map[string]rawstatus.FileStatus{"one.md": {Path: raw.Path, Ingested: true}}})
	if pages[0].LifecycleStatus != "synced" {
		t.Fatalf("legacy ingested boolean status = %+v", pages[0])
	}
	pages, _ = Calculate(Input{Sources: []storage.WikiPage{base}, RawFiles: []storage.RawFile{raw}, Annotations: map[string]storage.ObjectMeta{"id": {SHA256: annotation.Digest("note"), HasAnnotation: true}}, Legacy: legacy})
	if pages[0].LifecycleStatus != "notes_pending" || !pages[0].AnnotationDirty {
		t.Fatalf("legacy annotation status = %+v", pages[0])
	}
	pages, _ = Calculate(Input{Sources: []storage.WikiPage{base}, RawFiles: []storage.RawFile{raw}, Legacy: map[string]rawstatus.FileStatus{"one.md": {Path: raw.Path, Error: "failed"}}})
	if pages[0].LifecycleStatus != "content_pending" || !pages[0].RawDirty {
		t.Fatalf("legacy error status = %+v", pages[0])
	}
}

func TestFailureOnlyReceiptIsErrorAndRunnable(t *testing.T) {
	raw := storage.RawFile{Name: "one.md", Path: "raw/one.md", SHA256: "raw"}
	ann := annotation.Digest("note")
	failure := sourcestatus.Receipt{RawPath: raw.Path, FailedFingerprint: sourcestatus.Fingerprint(raw.SHA256, ann), Error: "worker failed"}
	pages, counts := Calculate(Input{Sources: []storage.WikiPage{{ID: "id", RawPath: raw.Path}}, RawFiles: []storage.RawFile{raw}, Annotations: map[string]storage.ObjectMeta{"id": {SHA256: ann, HasAnnotation: true}}, Receipts: map[string]sourcestatus.Receipt{"id": failure}, Legacy: map[string]rawstatus.FileStatus{"one.md": {Path: raw.Path, OLWStatus: "compiled"}}})
	if pages[0].LifecycleStatus != "error" || !pages[0].AnnotationDirty || counts.AnnotationDirty != 1 || !pages[0].Dirty {
		t.Fatalf("failure-only page=%+v counts=%+v", pages[0], counts)
	}
	// A retry remains eligible even when the failed input was also previously
	// successful: the error must not erase all pending-work counts.
	failure.LastIngestedRawSHA256 = raw.SHA256
	failure.LastIngestedAnnSHA256 = ann
	failure.LastIngestFingerprint = sourcestatus.Fingerprint(raw.SHA256, ann)
	failure.LastSuccessAt = time.Now().UTC().Format(time.RFC3339)
	pages, counts = Calculate(Input{Sources: []storage.WikiPage{{ID: "id", RawPath: raw.Path}}, RawFiles: []storage.RawFile{raw}, Annotations: map[string]storage.ObjectMeta{"id": {SHA256: ann, HasAnnotation: true}}, Receipts: map[string]sourcestatus.Receipt{"id": failure}})
	if pages[0].LifecycleStatus != "error" || !pages[0].RawDirty || counts.RawDirty != 1 {
		t.Fatalf("retry page=%+v counts=%+v", pages[0], counts)
	}
}

func TestLegacyCompiledRawUpdatedAfterStatusIsDirty(t *testing.T) {
	generated := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	raw := storage.RawFile{Name: "one.md", Path: "raw/one.md", SHA256: "new", Updated: generated.Add(time.Second)}
	pages, counts := Calculate(Input{Sources: []storage.WikiPage{{ID: "id", RawPath: raw.Path}}, RawFiles: []storage.RawFile{raw}, Legacy: map[string]rawstatus.FileStatus{"one.md": {Path: raw.Path, OLWStatus: "compiled"}}, LegacyGeneratedAt: generated})
	if pages[0].LifecycleStatus != "content_pending" || !pages[0].RawDirty || counts.RawDirty != 1 {
		t.Fatalf("page=%+v counts=%+v", pages[0], counts)
	}
}
