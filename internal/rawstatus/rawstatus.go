package rawstatus

import (
	"encoding/json"
	"time"

	store "github.com/rayer/llm-wiki-bff/internal/storage"
)

const Path = "cache/raw_status.json"

type Artifact struct {
	Version     int                   `json:"version"`
	GeneratedAt string                `json:"generated_at"`
	FileCount   int                   `json:"file_count"`
	Files       map[string]FileStatus `json:"files"`
}

type FileStatus struct {
	Path       string `json:"path"`
	SHA256     string `json:"sha256"`
	OLWStatus  string `json:"olw_status"`
	Ingested   bool   `json:"ingested"`
	IngestedAt string `json:"ingested_at,omitempty"`
	Error      string `json:"error,omitempty"`
}

type File struct {
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	Updated  string `json:"updated"`
	SHA256   string `json:"sha256"`
	Ingested bool   `json:"ingested"`
}

func EmptyArtifact(now time.Time) Artifact {
	return Artifact{
		Version:     1,
		GeneratedAt: now.UTC().Format(time.RFC3339),
		FileCount:   0,
		Files:       map[string]FileStatus{},
	}
}

func Decode(data []byte) (Artifact, error) {
	var artifact Artifact
	if err := json.Unmarshal(data, &artifact); err != nil {
		return Artifact{}, err
	}
	if artifact.Files == nil {
		artifact.Files = map[string]FileStatus{}
	}
	// Older artifacts omit file_count; fall back to map size.
	if artifact.FileCount == 0 && len(artifact.Files) > 0 {
		artifact.FileCount = len(artifact.Files)
	}
	return artifact, nil
}

// Count returns the number of raw files recorded on the artifact.
func Count(artifact Artifact) int {
	if artifact.FileCount > 0 {
		return artifact.FileCount
	}
	return len(artifact.Files)
}

func Apply(files []store.RawFile, artifact Artifact) []File {
	out := make([]File, 0, len(files))
	for _, raw := range files {
		status := artifact.Files[raw.Name]
		out = append(out, File{
			Name:     raw.Name,
			Size:     raw.Size,
			Updated:  raw.Updated.UTC().Format(time.RFC3339),
			SHA256:   raw.SHA256,
			Ingested: isIngested(raw, status),
		})
	}
	return out
}

func isIngested(_ store.RawFile, status FileStatus) bool {
	// Trust OLW state.db status for the UI badge.
	// Do NOT require list-time SHA256 == OLW content_hash:
	// - GCS ListRawFiles often has empty metadata sha256
	// - OLW content_hash may not equal raw file sha256 / upload metadata
	// Those mismatches previously marked pipeline-complete files as pending.
	if status.Error != "" {
		return false
	}
	return status.OLWStatus == "ingested" || status.OLWStatus == "compiled"
}
