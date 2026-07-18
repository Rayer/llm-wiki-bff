package sourcestatus

import (
	"fmt"
	"strings"
	"testing"
)

func TestFingerprintVector(t *testing.T) {
	if got := Fingerprint("raw", "annotation"); got != "606e7c8f9230826ff5e1eb1e13d489ec84ea7a2ff5729ed454008528eca3e2e7" {
		t.Fatalf("Fingerprint() = %s", got)
	}
}

func TestDecodeMalformedReceipt(t *testing.T) {
	if _, err := Decode([]byte("{")); err == nil {
		t.Fatal("malformed receipt must fail decoding")
	}
}

func TestDecodeRejectsLogicalEntryOverflow(t *testing.T) {
	var b strings.Builder
	b.WriteString(`{"version":1,"sources":{`)
	for i := 0; i < 10001; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `"source-%d":{"raw_path":"raw/source.md"}`, i)
	}
	b.WriteString("}}")
	if _, err := Decode([]byte(b.String())); err == nil || err.Error() != "generated cache logical entry limit exceeded" {
		t.Fatalf("Decode() error = %v, want fixed logical-entry error", err)
	}
}
