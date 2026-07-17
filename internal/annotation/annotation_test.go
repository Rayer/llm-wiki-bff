package annotation

import "testing"

func TestNormalizeAndDigest(t *testing.T) {
	if got := Normalize("a\r\nb\rc\n"); got != "a\nb\nc\n" {
		t.Fatalf("Normalize() = %q", got)
	}
	if got := Digest(""); got != "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" {
		t.Fatalf("empty digest = %s", got)
	}
}

func TestObjectValidate(t *testing.T) {
	valid := Object{Version: 1, SourceID: "a..b", RawPath: "raw/a..b.md", Body: "note\n", SHA256: Digest("note\n"), UpdatedAt: "2026-01-02T03:04:05Z", UpdatedBy: "u"}
	if err := valid.Validate("a..b", "raw/a..b.md"); err != nil {
		t.Fatalf("valid object: %v", err)
	}
	for name, mutate := range map[string]func(*Object){
		"version":   func(o *Object) { o.Version = 2 },
		"identity":  func(o *Object) { o.SourceID = "other" },
		"raw path":  func(o *Object) { o.RawPath = "raw/other.md" },
		"body":      func(o *Object) { o.Body = "note\r\n" },
		"digest":    func(o *Object) { o.SHA256 = "bad" },
		"author":    func(o *Object) { o.UpdatedBy = "" },
		"timestamp": func(o *Object) { o.UpdatedAt = "not-a-time" },
	} {
		t.Run(name, func(t *testing.T) {
			object := valid
			mutate(&object)
			if err := object.Validate("a..b", "raw/a..b.md"); err == nil {
				t.Fatal("invalid object passed validation")
			}
		})
	}
	for _, id := range []string{"", ".", "..", "a/b", `a\\b`} {
		if ValidSourceID(id) {
			t.Fatalf("unsafe source ID accepted: %q", id)
		}
	}
	if !ValidSourceID("a..b") {
		t.Fatal("existing safe source ID rejected")
	}
}
