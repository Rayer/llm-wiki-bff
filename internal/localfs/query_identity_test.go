package localfs

import (
	"context"
	"testing"
)

func TestQueryGenerationIdentityIsExplicitLegacyAndDoesNotScan(t *testing.T) {
	identity, err := New(t.TempDir()).WithScope("user", "project").QueryGenerationIdentity(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if identity.ProjectID != "project" || identity.GenerationID != "legacy" || identity.ConceptsDigest != "" {
		t.Fatalf("identity=%+v", identity)
	}
}
