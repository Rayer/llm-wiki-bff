package sourcestatus

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/rayer/llm-wiki-bff/internal/generation"
)

const Path = "cache/source_status.json"

type Receipt struct {
	RawPath               string `json:"raw_path"`
	LastIngestedRawSHA256 string `json:"last_ingested_raw_sha256"`
	LastIngestedAnnSHA256 string `json:"last_ingested_ann_sha256"`
	LastIngestFingerprint string `json:"last_ingest_fingerprint"`
	LastSuccessAt         string `json:"last_success_at"`
	FailedFingerprint     string `json:"failed_fingerprint,omitempty"`
	Error                 string `json:"error,omitempty"`
}

type Artifact struct {
	Version int                `json:"version"`
	Sources map[string]Receipt `json:"sources"`
}

func Decode(data []byte) (Artifact, error) {
	var a Artifact
	dec := json.NewDecoder(bytes.NewReader(data))
	token, err := dec.Token()
	if err == nil {
		var ok bool
		var delim json.Delim
		delim, ok = token.(json.Delim)
		if !ok || delim != '{' {
			err = fmt.Errorf("expected JSON object")
		}
	}
	for err == nil && dec.More() {
		var key interface{}
		key, err = dec.Token()
		name, ok := key.(string)
		if err == nil && !ok {
			err = fmt.Errorf("expected JSON object key")
		}
		if err != nil {
			break
		}
		switch name {
		case "version":
			err = dec.Decode(&a.Version)
		case "sources":
			a.Sources, err = generation.DecodeBoundedMap[Receipt](dec)
		default:
			var ignored json.RawMessage
			err = dec.Decode(&ignored)
		}
	}
	if err == nil {
		_, err = dec.Token()
	}
	if err == nil {
		err = generation.EnsureJSONEOF(dec)
	}
	if a.Sources == nil {
		a.Sources = map[string]Receipt{}
	}
	return a, err
}

func Fingerprint(raw, annotation string) string {
	sum := sha256.Sum256([]byte("lwc-ingest-v1\n" + raw + "\n" + annotation + "\n"))
	return hex.EncodeToString(sum[:])
}

// ValidReceipt verifies that a worker receipt is complete enough to describe
// a successful ingestion. Invalid receipts must fall back to legacy status.
func ValidReceipt(receipt Receipt, rawPath string) bool {
	if receipt.RawPath != rawPath || strings.TrimSpace(receipt.LastIngestedRawSHA256) == "" ||
		strings.TrimSpace(receipt.LastIngestedAnnSHA256) == "" || strings.TrimSpace(receipt.LastIngestFingerprint) == "" {
		return false
	}
	if _, err := time.Parse(time.RFC3339, receipt.LastSuccessAt); err != nil {
		return false
	}
	return receipt.LastIngestFingerprint == Fingerprint(receipt.LastIngestedRawSHA256, receipt.LastIngestedAnnSHA256)
}

// ValidCurrentFailure verifies the independent failure receipt shape. A first
// ingestion can fail before a successful receipt exists, but it must still be
// visible as an error for the exact current input fingerprint.
func ValidCurrentFailure(receipt Receipt, rawPath, fingerprint string) bool {
	return receipt.RawPath == rawPath && strings.TrimSpace(receipt.Error) != "" &&
		receipt.FailedFingerprint == fingerprint
}
