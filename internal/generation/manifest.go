// Package generation defines the immutable worker-output manifest contract.
package generation

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"
)

const (
	ManifestPath = ".lwc/publish/current.json"
	LeasePath    = ".lwc/publish/lease.json"
	Prefix       = ".lwc/publish/generations/"
	Version      = 1
	MaxFiles     = 10_000
	MaxFileBytes = 64 << 20
	MaxTotalSize = 512 << 20
	// Manifest fields are bounded by the schema: 10k file rows with a 1024-byte
	// path plus fixed JSON/digest/generation fields and a small header.
	MaxPathBytes        = 1024
	MaxManifestBytes    = 1024 + MaxFiles*(MaxPathBytes+192)
	LeaseReleaseTimeout = 5 * time.Second
)

type File struct {
	Path       string `json:"path"`
	Size       int64  `json:"size"`
	SHA256     string `json:"sha256"`
	Generation int64  `json:"generation"`
}

type Manifest struct {
	Version              int    `json:"version"`
	GenerationID         string `json:"generation_id"`
	PreviousGenerationID string `json:"previous_generation_id,omitempty"`
	CreatedAt            string `json:"created_at"`
	InputFingerprint     string `json:"input_fingerprint"`
	Files                []File `json:"files"`
}

func Decode(data []byte) (Manifest, error) {
	if len(data) > MaxManifestBytes {
		return Manifest{}, errors.New("generation manifest exceeds limit")
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return Manifest{}, errors.New("invalid generation manifest")
	}
	if err := m.Validate(); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

func (m Manifest) Validate() error {
	if m.Version != Version || !safeGenerationID(m.GenerationID) || (m.PreviousGenerationID != "" && !safeGenerationID(m.PreviousGenerationID)) {
		return errors.New("invalid generation manifest")
	}
	if _, err := time.Parse(time.RFC3339, m.CreatedAt); err != nil || strings.TrimSpace(m.InputFingerprint) == "" || len(m.Files) > MaxFiles {
		return errors.New("invalid generation manifest")
	}
	var total int64
	last := ""
	for _, file := range m.Files {
		if !GenerationOwned(file.Path) || file.Path <= last || file.Size < 0 || file.Size > MaxFileBytes || file.Generation <= 0 || !validDigest(file.SHA256) {
			return errors.New("invalid generation manifest")
		}
		if total > MaxTotalSize-file.Size {
			return errors.New("invalid generation manifest")
		}
		total += file.Size
		last = file.Path
	}
	return nil
}

func (m Manifest) ObjectPath(file File) string { return Prefix + m.GenerationID + "/" + file.Path }

func (m Manifest) File(path string) (File, bool) {
	i := sort.Search(len(m.Files), func(i int) bool { return m.Files[i].Path >= path })
	returnFile := File{}
	if i < len(m.Files) && m.Files[i].Path == path {
		returnFile = m.Files[i]
		return returnFile, true
	}
	return returnFile, false
}

func GenerationOwned(rel string) bool {
	if strings.HasPrefix(rel, "wiki/") {
		return safePath(rel)
	}
	switch rel {
	case "wiki.toml", "cache/id_map.json", "cache/concepts.jsonl", "cache/raw_status.json", "cache/suggested_queries.json", ".olw/state.db":
		return true
	default:
		return false
	}
}

// GenerationOwnedWritePath also recognizes the temporary paths used by
// atomic writers for generation-owned outputs.
func GenerationOwnedWritePath(rel string) bool {
	if GenerationOwned(rel) {
		return true
	}
	return strings.HasSuffix(rel, ".tmp") && GenerationOwned(strings.TrimSuffix(rel, ".tmp"))
}

func safePath(p string) bool {
	return p != "" && len(p) <= MaxPathBytes && !strings.HasPrefix(p, "/") && path.Clean(p) == p && !strings.Contains(p, "\\") && !strings.Contains(p, "//") && !strings.Contains(p, "/./") && !strings.Contains(p, "..")
}

func NewFile(path string, data []byte, objectGeneration int64) (File, error) {
	if !GenerationOwned(path) || objectGeneration <= 0 {
		return File{}, fmt.Errorf("invalid generation output")
	}
	sum := sha256.Sum256(data)
	return File{Path: path, Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:]), Generation: objectGeneration}, nil
}

func safeGenerationID(id string) bool {
	if len(id) < 8 || len(id) > 128 {
		return false
	}
	for _, r := range id {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

func validDigest(value string) bool {
	_, err := hex.DecodeString(value)
	return err == nil && len(value) == 64
}
