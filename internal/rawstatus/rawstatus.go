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
	return artifact, nil
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

func isIngested(raw store.RawFile, status FileStatus) bool {
	if raw.SHA256 == "" || status.SHA256 != raw.SHA256 || status.Error != "" {
		return false
	}
	return status.OLWStatus == "ingested" || status.OLWStatus == "compiled"
}
