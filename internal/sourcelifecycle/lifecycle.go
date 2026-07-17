// Package sourcelifecycle calculates the worker-facing state of sources.
package sourcelifecycle

import (
	"strings"
	"time"

	"github.com/rayer/llm-wiki-bff/internal/annotation"
	"github.com/rayer/llm-wiki-bff/internal/rawstatus"
	"github.com/rayer/llm-wiki-bff/internal/sourcestatus"
	"github.com/rayer/llm-wiki-bff/internal/storage"
)

type Input struct {
	Sources           []storage.WikiPage
	RawFiles          []storage.RawFile
	Annotations       map[string]storage.ObjectMeta
	Receipts          map[string]sourcestatus.Receipt
	Legacy            map[string]rawstatus.FileStatus
	LegacyGeneratedAt time.Time
}

type Counts struct {
	NewRaw          int
	RawDirty        int
	AnnotationDirty int
}

// Calculate decorates compiled sources and appends unmapped current raw files.
// Invalid mappings remain visible but cannot be annotated or counted as work.
func Calculate(in Input) ([]storage.WikiPage, Counts) {
	raws := make(map[string]storage.RawFile, len(in.RawFiles))
	for _, raw := range in.RawFiles {
		raws[raw.Path] = raw
	}
	mapped := make(map[string]bool, len(in.Sources))
	pages := append([]storage.WikiPage(nil), in.Sources...)
	var counts Counts
	for i := range pages {
		page := &pages[i]
		page.RawPath = strings.TrimSpace(page.RawPath)
		page.AnnotationAllowed = storage.SafeRawPath(page.RawPath)
		page.HasAnnotation = false
		page.AnnotationDirty = false
		page.RawDirty = false
		page.Dirty = false
		page.AnnUpdatedAt = ""
		if !page.AnnotationAllowed {
			page.LifecycleStatus = "new"
			continue
		}
		mapped[page.RawPath] = true
		ann := annotation.Digest("")
		if meta, ok := in.Annotations[page.ID]; ok {
			if meta.SHA256 != "" {
				ann = meta.SHA256
			}
			page.HasAnnotation = meta.HasAnnotation
			if !meta.Updated.IsZero() {
				page.AnnUpdatedAt = meta.Updated.UTC().Format("2006-01-02T15:04:05Z07:00")
			}
		}
		raw, rawExists := raws[page.RawPath]
		receipt, hasReceipt := in.Receipts[page.ID]
		fingerprint := sourcestatus.Fingerprint(raw.SHA256, ann)
		if rawExists && hasReceipt && sourcestatus.ValidCurrentFailure(receipt, page.RawPath, fingerprint) {
			markFailure(page, &counts, receipt, raw, ann, in.Legacy)
			continue
		}
		if !hasReceipt || !sourcestatus.ValidReceipt(receipt, page.RawPath) {
			if legacy, ok := legacyStatus(in.Legacy, raw); ok {
				if (!in.LegacyGeneratedAt.IsZero() && raw.Updated.After(in.LegacyGeneratedAt)) || legacy.Error != "" || (!legacy.Ingested && legacy.OLWStatus != "ingested" && legacy.OLWStatus != "compiled") {
					page.RawDirty = true
					page.Dirty = true
					page.LifecycleStatus = "content_pending"
					counts.RawDirty++
				} else if ann != annotation.Digest("") {
					page.AnnotationDirty = true
					page.Dirty = true
					page.LifecycleStatus = "notes_pending"
					counts.AnnotationDirty++
				} else {
					page.LifecycleStatus = "synced"
				}
				continue
			}
			page.LifecycleStatus = "new"
			if rawExists {
				counts.NewRaw++
			}
			continue
		}
		if !rawExists || receipt.LastIngestedRawSHA256 != raw.SHA256 {
			page.RawDirty = true
			page.Dirty = true
			page.LifecycleStatus = "content_pending"
			counts.RawDirty++
		} else if receipt.LastIngestedAnnSHA256 != ann {
			page.AnnotationDirty = true
			page.Dirty = true
			page.LifecycleStatus = "notes_pending"
			counts.AnnotationDirty++
		} else if fingerprint == receipt.LastIngestFingerprint {
			page.LifecycleStatus = "synced"
		} else {
			page.LifecycleStatus = "new"
			if rawExists {
				counts.NewRaw++
			}
		}
	}
	for _, raw := range in.RawFiles {
		if mapped[raw.Path] {
			continue
		}
		pages = append(pages, storage.WikiPage{Slug: raw.Path, Title: raw.Name, RawPath: raw.Path, LifecycleStatus: "new"})
		counts.NewRaw++
	}
	return pages, counts
}

func markFailure(page *storage.WikiPage, counts *Counts, receipt sourcestatus.Receipt, raw storage.RawFile, ann string, legacy map[string]rawstatus.FileStatus) {
	page.LifecycleStatus = "error"
	if sourcestatus.ValidReceipt(receipt, page.RawPath) {
		if receipt.LastIngestedRawSHA256 == raw.SHA256 && receipt.LastIngestedAnnSHA256 != ann {
			page.AnnotationDirty = true
			page.Dirty = true
			counts.AnnotationDirty++
			return
		}
		// A failed retry of an otherwise current successful input still needs a
		// runnable quota signal, so classify it as raw work.
		page.RawDirty = true
		page.Dirty = true
		counts.RawDirty++
		return
	}
	if prior, ok := legacyStatus(legacy, raw); ok && (prior.Ingested || prior.OLWStatus == "ingested" || prior.OLWStatus == "compiled") && ann != annotation.Digest("") {
		page.AnnotationDirty = true
		page.Dirty = true
		counts.AnnotationDirty++
		return
	}
	page.RawDirty = true
	page.Dirty = true
	counts.RawDirty++
}

func SafeRawPath(raw string) bool {
	return storage.SafeRawPath(raw)
}

func legacyStatus(statuses map[string]rawstatus.FileStatus, raw storage.RawFile) (rawstatus.FileStatus, bool) {
	if !storage.SafeRawPath(raw.Path) {
		return rawstatus.FileStatus{}, false
	}
	status, ok := statuses[raw.Name]
	if !ok {
		status, ok = statuses[raw.Path]
	}
	if !ok || (status.Path != "" && status.Path != raw.Path) {
		return rawstatus.FileStatus{}, false
	}
	return status, true
}
