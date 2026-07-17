package sourcestatus

import "testing"

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
