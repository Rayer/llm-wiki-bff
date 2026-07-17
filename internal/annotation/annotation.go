package annotation

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"strings"
	"time"
	"unicode/utf8"
)

const PathPrefix = "cache/annotations/"

type Object struct {
	Version   int    `json:"version"`
	SourceID  string `json:"source_id"`
	RawPath   string `json:"raw_path"`
	Body      string `json:"body"`
	SHA256    string `json:"ann_sha256"`
	UpdatedAt string `json:"updated_at"`
	UpdatedBy string `json:"updated_by"`
}

func Normalize(body string) string {
	return strings.ReplaceAll(strings.ReplaceAll(body, "\r\n", "\n"), "\r", "\n")
}
func Digest(body string) string {
	s := sha256.Sum256([]byte(Normalize(body)))
	return hex.EncodeToString(s[:])
}
func Path(sourceID string) string { return PathPrefix + sourceID + ".json" }

// ValidSourceID permits the stable IDs already present in project id maps,
// while ensuring an ID can never alter the annotation object path.
func ValidSourceID(sourceID string) bool {
	return sourceID != "" && sourceID != "." && sourceID != ".." &&
		!strings.ContainsAny(sourceID, "/\\") && path.Clean(sourceID) == sourceID
}

// Validate checks the persisted annotation contract for its expected source.
func (o Object) Validate(sourceID, rawPath string) error {
	if o.Version != 1 || o.SourceID != sourceID || o.RawPath != rawPath {
		return fmt.Errorf("annotation identity is invalid")
	}
	if !utf8.ValidString(o.Body) || Normalize(o.Body) != o.Body {
		return fmt.Errorf("annotation body is invalid")
	}
	if o.SHA256 != Digest(o.Body) {
		return fmt.Errorf("annotation digest is invalid")
	}
	if strings.TrimSpace(o.UpdatedBy) == "" {
		return fmt.Errorf("annotation author is invalid")
	}
	if !strings.HasSuffix(o.UpdatedAt, "Z") {
		return fmt.Errorf("annotation timestamp is invalid")
	}
	if _, err := time.Parse(time.RFC3339, o.UpdatedAt); err != nil {
		return fmt.Errorf("annotation timestamp is invalid: %w", err)
	}
	return nil
}
