package v1

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

type projectRenameFixture struct {
	projects map[string]map[string]string
	calls    []string
}

type transactionalProjectRenameFixture struct {
	snapshot map[string]interface{}
	retry    map[string]interface{}
	calls    int
}

func (f *transactionalProjectRenameFixture) RenameProject(_ context.Context, userID, projectID, name string, authorize func(string, map[string]interface{}) bool) error {
	f.calls++
	docID := projectDocID(userID, projectID)
	if !authorize(docID, f.snapshot) {
		return errProjectNotFound
	}
	if f.retry != nil {
		f.snapshot = f.retry
		f.retry = nil
		f.calls++
		if !authorize(docID, f.snapshot) {
			return errProjectNotFound
		}
	}
	f.snapshot["name"] = name
	return nil
}

func (f *projectRenameFixture) rename(_ context.Context, userID, projectID, name string) error {
	key := userID + "/" + projectID
	project, ok := f.projects[key]
	if !ok {
		return errProjectNotFound
	}
	f.calls = append(f.calls, key+"="+name)
	project["name"] = name
	return nil
}

func TestRenameProjectOwnerSuccessPersistsTrimmedName(t *testing.T) {
	fixture := &projectRenameFixture{projects: map[string]map[string]string{
		"owner/project": {"name": "Before", "project_id": "project", "storage_prefix": "users/owner/projects/project", "generation": "g1"},
	}}
	h := New(nil, nil, nil, nil, nil, nil)
	h.SetProjectRenameFunc(fixture.rename)

	recorder := invokeRenameProject(t, h, "owner", "project", `{"name":"  Renamed  "}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if got := fixture.projects["owner/project"]["name"]; got != "Renamed" {
		t.Fatalf("stored name=%q, want Renamed", got)
	}
	if got := fixture.calls; !reflect.DeepEqual(got, []string{"owner/project=Renamed"}) {
		t.Fatalf("calls=%v", got)
	}
	var response map[string]string
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response["id"] != "project" || response["name"] != "Renamed" {
		t.Fatalf("response=%v", response)
	}
}

func TestRenameProjectNameBoundaries(t *testing.T) {
	tests := []struct {
		name       string
		value      string
		wantStatus int
	}{
		{name: "one Unicode character", value: "界", wantStatus: http.StatusOK},
		{name: "64 Unicode characters", value: strings.Repeat("界", 64), wantStatus: http.StatusOK},
		{name: "empty after trim", value: "   ", wantStatus: http.StatusBadRequest},
		{name: "65 Unicode characters", value: strings.Repeat("界", 65), wantStatus: http.StatusBadRequest},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := &projectRenameFixture{projects: map[string]map[string]string{
				"owner/project": {"name": "Before"},
			}}
			h := New(nil, nil, nil, nil, nil, nil)
			h.SetProjectRenameFunc(fixture.rename)
			recorder := invokeRenameProject(t, h, "owner", "project", mustJSONName(t, tt.value))
			if recorder.Code != tt.wantStatus {
				t.Fatalf("status=%d body=%s, want %d", recorder.Code, recorder.Body.String(), tt.wantStatus)
			}
		})
	}
}

func TestRenameProjectStrictJSON(t *testing.T) {
	for _, body := range []string{`{"name":"Renamed","extra":true}`, `{"name":"Renamed"} trailing`} {
		fixture := &projectRenameFixture{projects: map[string]map[string]string{"owner/project": {"name": "Before"}}}
		h := New(nil, nil, nil, nil, nil, nil)
		h.SetProjectRenameFunc(fixture.rename)
		recorder := invokeRenameProject(t, h, "owner", "project", body)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("body %q status=%d, want 400", body, recorder.Code)
		}
		if !strings.HasPrefix(recorder.Body.String(), `{"error":"invalid JSON:`) {
			t.Fatalf("body=%s, want typed invalid JSON error", recorder.Body.String())
		}
	}
}

func TestRenameProjectRejectsDuplicateNameKeysWithoutUpdate(t *testing.T) {
	for _, body := range []string{`{"name":"First","name":"Second"}`, `{"name":"First","na\u006De":"Second"}`} {
		fixture := &projectRenameFixture{projects: map[string]map[string]string{"owner/project": {"name": "Before"}}}
		h := New(nil, nil, nil, nil, nil, nil)
		h.SetProjectRenameFunc(fixture.rename)
		recorder := invokeRenameProject(t, h, "owner", "project", body)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("body %q status=%d, want 400", body, recorder.Code)
		}
		if fixture.projects["owner/project"]["name"] != "Before" || len(fixture.calls) != 0 {
			t.Fatalf("duplicate key changed project: project=%v calls=%v", fixture.projects, fixture.calls)
		}
	}
}

func TestRenameProjectRejectsInvalidUTF8WithoutUpdate(t *testing.T) {
	fixture := &projectRenameFixture{projects: map[string]map[string]string{"owner/project": {"name": "Before"}}}
	h := New(nil, nil, nil, nil, nil, nil)
	h.SetProjectRenameFunc(fixture.rename)

	body := string(append([]byte(`{"name":"`), append([]byte{0xff}, []byte(`"}`)...)...))
	recorder := invokeRenameProject(t, h, "owner", "project", body)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s, want 400", recorder.Code, recorder.Body.String())
	}
	if fixture.projects["owner/project"]["name"] != "Before" || len(fixture.calls) != 0 {
		t.Fatalf("invalid UTF-8 changed project: project=%v calls=%v", fixture.projects, fixture.calls)
	}
}

func TestRenameProjectAcceptsExplicitReplacementRune(t *testing.T) {
	fixture := &projectRenameFixture{projects: map[string]map[string]string{"owner/project": {"name": "Before"}}}
	h := New(nil, nil, nil, nil, nil, nil)
	h.SetProjectRenameFunc(fixture.rename)

	recorder := invokeRenameProject(t, h, "owner", "project", `{"name":"�"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200", recorder.Code, recorder.Body.String())
	}
	if got := fixture.projects["owner/project"]["name"]; got != "�" {
		t.Fatalf("stored name=%q, want explicit replacement rune", got)
	}
}

func TestRenameProjectRejectsOversizedBodyBeforeUpdate(t *testing.T) {
	fixture := &projectRenameFixture{projects: map[string]map[string]string{"owner/project": {"name": "Before"}}}
	h := New(nil, nil, nil, nil, nil, nil)
	h.SetProjectRenameFunc(fixture.rename)
	body := `{"name":"` + strings.Repeat("x", 2000) + `"}`
	recorder := invokeRenameProject(t, h, "owner", "project", body)
	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status=%d body=%s, want 413", recorder.Code, recorder.Body.String())
	}
	if fixture.projects["owner/project"]["name"] != "Before" || len(fixture.calls) != 0 {
		t.Fatalf("oversized body updated project: project=%v calls=%v", fixture.projects, fixture.calls)
	}
}

func TestRenameProjectTransactionReclassifiesReplacementBeforeMutation(t *testing.T) {
	fixture := &transactionalProjectRenameFixture{
		snapshot: map[string]interface{}{"user_id": "owner", "project_id": "project", "name": "Before"},
		retry:    map[string]interface{}{"user_id": "foreign", "project_id": "project", "name": "Foreign"},
	}
	h := New(nil, nil, nil, nil, nil, nil)
	h.projectRenameTransaction = fixture

	err := h.renameOwnedProject(context.Background(), "owner", "project", "After")
	if !errors.Is(err, errProjectNotFound) {
		t.Fatalf("rename error=%v, want project not found", err)
	}
	if fixture.calls != 2 {
		t.Fatalf("transaction attempts=%d, want 2", fixture.calls)
	}
	if got := fixture.snapshot["name"]; got != "Foreign" {
		t.Fatalf("foreign replacement mutated: name=%v", got)
	}
}

func TestClassifyAdminProjectDocForOwnerRejectsConcatenationCollision(t *testing.T) {
	const docID = "a_b_c"
	tests := []struct {
		name          string
		storedUserID  string
		storedProject string
		requestedUser string
		requestedProj string
	}{
		{name: "stored a_b/c requested a/b_c", storedUserID: "a_b", storedProject: "c", requestedUser: "a", requestedProj: "b_c"},
		{name: "stored a/b_c requested a_b/c", storedUserID: "a", storedProject: "b_c", requestedUser: "a_b", requestedProj: "c"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := &projectRenameFixture{projects: map[string]map[string]string{}}
			h := New(nil, nil, nil, nil, nil, nil)
			h.SetProjectRenameFunc(func(_ context.Context, userID, projectID, name string) error {
				_, record, ok := classifyAdminProjectDocForOwner(docID, map[string]interface{}{
					"user_id":    tt.storedUserID,
					"project_id": tt.storedProject,
					"name":       "Before",
				}, userID)
				if !ok || record.projectID != projectID {
					return errProjectNotFound
				}
				fixture.calls = append(fixture.calls, userID+"/"+projectID+"="+name)
				return nil
			})
			recorder := invokeRenameProject(t, h, tt.requestedUser, tt.requestedProj, `{"name":"After"}`)
			if recorder.Code != http.StatusNotFound {
				t.Fatalf("status=%d body=%s, want 404", recorder.Code, recorder.Body.String())
			}
			if len(fixture.calls) != 0 {
				t.Fatalf("collision caused rename calls=%v", fixture.calls)
			}
			_, _, ok := classifyAdminProjectDocForOwner(docID, map[string]interface{}{
				"user_id":    tt.storedUserID,
				"project_id": tt.storedProject,
				"name":       "Before",
			}, tt.requestedUser)
			if ok {
				t.Fatalf("classifier accepted foreign owner %q for stored pair (%q, %q)", tt.requestedUser, tt.storedUserID, tt.storedProject)
			}
		})
	}
}

func TestRenameProjectMissingAndCrossOwnerAreNotFound(t *testing.T) {
	fixture := &projectRenameFixture{projects: map[string]map[string]string{
		"other/project": {"name": "Foreign"},
	}}
	h := New(nil, nil, nil, nil, nil, nil)
	h.SetProjectRenameFunc(fixture.rename)

	for _, userID := range []string{"owner", "other"} {
		recorder := invokeRenameProject(t, h, userID, "missing", `{"name":"Renamed"}`)
		if recorder.Code != http.StatusNotFound {
			t.Fatalf("user=%s status=%d body=%s, want 404", userID, recorder.Code, recorder.Body.String())
		}
	}
	if recorder := invokeRenameProject(t, h, "owner", "project", `{"name":"Renamed"}`); recorder.Code != http.StatusNotFound {
		t.Fatalf("cross-owner status=%d body=%s, want 404", recorder.Code, recorder.Body.String())
	}
}

func TestRenameProjectDoesNotChangeIdentityStorageOrGenerationFields(t *testing.T) {
	fixture := &projectRenameFixture{projects: map[string]map[string]string{
		"owner/project": {
			"name":             "Before",
			"project_id":       "project",
			"user_id":          "owner",
			"storage_prefix":   "users/owner/projects/project",
			"generation":       "generation-one",
			"artifact_pointer": "generation-one/current.json",
		},
	}}
	before := cloneStringMap(fixture.projects["owner/project"])
	h := New(nil, nil, nil, nil, nil, nil)
	h.SetProjectRenameFunc(fixture.rename)

	if recorder := invokeRenameProject(t, h, "owner", "project", `{"name":"After"}`); recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	after := cloneStringMap(fixture.projects["owner/project"])
	before["name"] = "After"
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("project changed beyond name: before=%v after=%v", before, after)
	}
}

func TestRenameProjectAllowsDuplicateDisplayNames(t *testing.T) {
	fixture := &projectRenameFixture{projects: map[string]map[string]string{
		"owner/one": {"name": "One"},
		"owner/two": {"name": "Two"},
	}}
	h := New(nil, nil, nil, nil, nil, nil)
	h.SetProjectRenameFunc(fixture.rename)

	for _, projectID := range []string{"one", "two"} {
		if recorder := invokeRenameProject(t, h, "owner", projectID, `{"name":"Same"}`); recorder.Code != http.StatusOK {
			t.Fatalf("project=%s status=%d body=%s", projectID, recorder.Code, recorder.Body.String())
		}
	}
	if fixture.projects["owner/one"]["name"] != "Same" || fixture.projects["owner/two"]["name"] != "Same" {
		t.Fatalf("duplicate names not persisted: %v", fixture.projects)
	}
}

func TestProjectListReflectsRenamedFirestoreName(t *testing.T) {
	project, userID, ok := projectResponseFromFirestoreDoc("owner_project", map[string]interface{}{
		"user_id":    "owner",
		"project_id": "project",
		"name":       "After",
	})
	if !ok || userID != "owner" || project.ID != "project" || project.Name != "After" {
		t.Fatalf("project=%+v user=%q ok=%v", project, userID, ok)
	}
}

func invokeRenameProject(t *testing.T, h *Handler, userID, projectID, body string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPatch, "/api/v1/projects/"+projectID, strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Params = gin.Params{{Key: "projectID", Value: projectID}}
	c.Set("userID", userID)
	h.RenameProject(c)
	return recorder
}

func mustJSONName(t *testing.T, name string) string {
	t.Helper()
	data, err := json.Marshal(map[string]string{"name": name})
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func cloneStringMap(src map[string]string) map[string]string {
	dst := make(map[string]string, len(src))
	for key, value := range src {
		dst[key] = value
	}
	return dst
}
