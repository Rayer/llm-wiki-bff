package gcs

import "testing"

func TestNewScopedClientUsesRequestedPrefixWithoutChangingDefault(t *testing.T) {
	defaultClient := &Client{userID: "default-user", projectID: "default-project"}

	scopedClient := defaultClient.NewScopedClient("request-user", "request-project")

	if got := scopedClient.Prefix(); got != "users/request-user/projects/request-project" {
		t.Fatalf("scoped prefix = %q, want %q", got, "users/request-user/projects/request-project")
	}
	if got := defaultClient.Prefix(); got != "users/default-user/projects/default-project" {
		t.Fatalf("default prefix = %q, want %q", got, "users/default-user/projects/default-project")
	}
	if scopedClient.bucket != defaultClient.bucket {
		t.Fatal("scoped client does not share the default client's bucket handle")
	}
}
