package v1

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"cloud.google.com/go/storage"
	"github.com/gin-gonic/gin"
	store "github.com/rayer/llm-wiki-bff/internal/storage"
	"github.com/stretchr/testify/assert"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type adminStatsProjectStore struct {
	prefix          string
	concepts        []store.WikiPage
	sources         []store.WikiPage
	cacheConcepts   []store.WikiPage
	cacheSources    []store.WikiPage
	conceptErr      error
	sourceErr       error
	cacheConceptErr error
	cacheSourceErr  error
	hasManifest     bool
	manifestErr     error
	manifestCalls   int
}

func (s *adminStatsProjectStore) Prefix() string { return s.prefix }

func (s *adminStatsProjectStore) ReadFile(context.Context, string) ([]byte, error) {
	return nil, storage.ErrObjectNotExist
}

func (s *adminStatsProjectStore) WriteBytes(context.Context, []byte, string) (string, error) {
	return "", errors.New("not implemented")
}

func (s *adminStatsProjectStore) WriteBytesAtomic(context.Context, []byte, string, string) (string, error) {
	return "", errors.New("not implemented")
}

func (s *adminStatsProjectStore) ListProjects(context.Context, string) ([]store.Project, error) {
	return nil, errors.New("not implemented")
}

func (s *adminStatsProjectStore) ListConcepts(context.Context, bool) ([]store.WikiPage, error) {
	return s.concepts, s.conceptErr
}

func (s *adminStatsProjectStore) ListSources(context.Context) ([]store.WikiPage, error) {
	return s.sources, s.sourceErr
}

func (s *adminStatsProjectStore) ListConceptsFromCache(context.Context) ([]store.WikiPage, error) {
	return s.cacheConcepts, s.cacheConceptErr
}

func (s *adminStatsProjectStore) ListSourcesFromCache(context.Context) ([]store.WikiPage, error) {
	return s.cacheSources, s.cacheSourceErr
}

func (s *adminStatsProjectStore) GetPage(context.Context, string, string) (*store.WikiPage, []byte, error) {
	return nil, nil, errors.New("not implemented")
}

func (s *adminStatsProjectStore) ListMarkdownFiles(context.Context, string) ([]store.MarkdownFile, error) {
	return nil, errors.New("not implemented")
}

func (s *adminStatsProjectStore) ListRawFiles(context.Context) ([]store.RawFile, error) {
	return nil, errors.New("not implemented")
}

func (s *adminStatsProjectStore) BucketStats(context.Context) (int64, int64, error) {
	return 0, 0, errors.New("not implemented")
}

func (s *adminStatsProjectStore) GetMetaSHA256(context.Context, string) (string, error) {
	return "", errors.New("not implemented")
}

func (s *adminStatsProjectStore) HasCurrentManifest(context.Context) (bool, error) {
	s.manifestCalls++
	return s.hasManifest, s.manifestErr
}

type adminStatsRootStore struct {
	*adminStatsProjectStore
	scoped map[string]*adminStatsProjectStore
}

func (s *adminStatsRootStore) Scope(userID, projectID string) store.Store {
	return s.scoped[userID+"/"+projectID]
}

func TestLoadAdminProjectStatisticsUsesScopedCacheAndFallback(t *testing.T) {
	root := &adminStatsRootStore{
		adminStatsProjectStore: &adminStatsProjectStore{prefix: "root"},
		scoped: map[string]*adminStatsProjectStore{
			"user-a/project-a": {
				prefix:        "users/user-a/projects/project-a",
				cacheConcepts: []store.WikiPage{{Slug: "a-one"}, {Slug: "a-two"}},
				cacheSources:  []store.WikiPage{{Slug: "a-source"}},
			},
			"user-a/project-b": {
				prefix:        "users/user-a/projects/project-b",
				cacheConcepts: []store.WikiPage{{Slug: "b-one"}},
				cacheSources:  []store.WikiPage{{Slug: "b-one"}, {Slug: "b-two"}},
			},
			"user-b/empty": {
				prefix:        "users/user-b/projects/empty",
				cacheConcepts: []store.WikiPage{},
				cacheSources:  []store.WikiPage{},
			},
			"user-b/legacy": {
				prefix:          "users/user-b/projects/legacy",
				concepts:        []store.WikiPage{{Slug: "legacy-one"}, {Slug: "legacy-two"}, {Slug: "legacy-three"}},
				sources:         []store.WikiPage{{Slug: "legacy-source"}},
				cacheConceptErr: storage.ErrObjectNotExist,
				cacheSourceErr:  storage.ErrObjectNotExist,
			},
		},
	}

	got, err := loadAdminProjectStatistics(context.Background(), root, []adminProjectRecord{
		{userID: "user-a", projectID: "project-a"},
		{userID: "user-a", projectID: "project-b"},
		{userID: "user-b", projectID: "empty"},
		{userID: "user-b", projectID: "legacy"},
	})
	if err != nil {
		t.Fatalf("loadAdminProjectStatistics() error = %v", err)
	}

	assert.Equal(t, adminProjectStatistics{conceptCount: 2, sourceCount: 1}, got["user-a/project-a"])
	assert.Equal(t, adminProjectStatistics{conceptCount: 1, sourceCount: 2}, got["user-a/project-b"])
	assert.Equal(t, adminProjectStatistics{conceptCount: 0, sourceCount: 0}, got["user-b/empty"])
	assert.Equal(t, adminProjectStatistics{conceptCount: 3, sourceCount: 1}, got["user-b/legacy"])
}

func TestAdminProjectCountsByUserUsesActualOwnership(t *testing.T) {
	got := adminProjectCountsByUser([]adminProjectRecord{
		{userID: "user-a", projectID: "project-a"},
		{userID: "user-a", projectID: "project-b"},
		{userID: "user-b", projectID: "project-c"},
	})

	assert.Equal(t, 2, got["user-a"])
	assert.Equal(t, 1, got["user-b"])
	assert.Equal(t, 0, got["user-c"])
}

func TestAdminProjectRecordFromFirestoreDocRequiresMatchingStoredProjectID(t *testing.T) {
	project, ok := adminProjectRecordFromFirestoreDoc("user-a_authoritative-project", map[string]interface{}{
		"user_id":    "user-a",
		"project_id": "authoritative-project",
		"name":       "Authoritative Project",
	})
	if !ok {
		t.Fatal("adminProjectRecordFromFirestoreDoc returned ok=false")
	}
	assert.Equal(t, "user-a", project.userID)
	assert.Equal(t, "authoritative-project", project.projectID)
	assert.Equal(t, "Authoritative Project", project.name)
}

func TestAdminProjectRecordFromFirestoreDocRejectsMismatchedStoredProjectID(t *testing.T) {
	if _, ok := adminProjectRecordFromFirestoreDoc("user-a_doc-suffix", map[string]interface{}{
		"user_id":    "user-a",
		"project_id": "authoritative-project",
		"name":       "Mismatched Project",
	}); ok {
		t.Fatal("mismatched project document must not authorize rebuild")
	}
}

type adminDeleteTestBackend struct {
	user                adminDeleteDocument
	projects            []adminDeleteDocument
	userProjects        []adminDeleteDocument
	expectedUserID      string
	listUserProjectsID  string
	deleteUserProjectID string
	deleteUserID        string
	events              *[]string
	listProjectsErr     error
	listUserProjectsErr error
	failProjectID       string
	projectErr          error
	failMarkerID        string
	markerErr           error
	failUserProjectID   string
	userProjectErr      error
}

func (b *adminDeleteTestBackend) getUser(context.Context, string) (adminDeleteDocument, error) {
	return b.user, nil
}

func (b *adminDeleteTestBackend) getProject(_ context.Context, id string) (adminDeleteDocument, error) {
	for _, project := range b.projects {
		if project.id == id {
			return project, nil
		}
	}
	return adminDeleteDocument{}, status.Error(codes.NotFound, "missing")
}

func (b *adminDeleteTestBackend) listProjects(context.Context) ([]adminDeleteDocument, error) {
	return append([]adminDeleteDocument(nil), b.projects...), b.listProjectsErr
}

func (b *adminDeleteTestBackend) listUserProjects(_ context.Context, userID string) ([]adminDeleteDocument, error) {
	b.listUserProjectsID = userID
	if b.expectedUserID != "" && userID != b.expectedUserID {
		return nil, fmt.Errorf("list user projects called for %q, want %q", userID, b.expectedUserID)
	}
	return append([]adminDeleteDocument(nil), b.userProjects...), b.listUserProjectsErr
}

func (b *adminDeleteTestBackend) deleteLock(_ context.Context, userID, projectID string) error {
	*b.events = append(*b.events, "lock:"+userID+"/"+projectID)
	return nil
}

func (b *adminDeleteTestBackend) deleteProject(_ context.Context, id string) error {
	project, ok := b.projectByID(id)
	if !ok {
		return errors.New("missing project")
	}
	*b.events = append(*b.events, "project:"+project.id)
	if project.projectID() == b.failProjectID {
		b.failProjectID = ""
		if b.projectErr != nil {
			return b.projectErr
		}
		return errors.New("injected project failure")
	}
	for i, candidate := range b.projects {
		if candidate.id == id {
			b.projects = append(b.projects[:i], b.projects[i+1:]...)
			break
		}
	}
	return nil
}

func (b *adminDeleteTestBackend) deleteProjectMetadata(_ context.Context, id string) error {
	*b.events = append(*b.events, "marker:"+id)
	if id == b.failMarkerID {
		b.failMarkerID = ""
		if b.markerErr != nil {
			return b.markerErr
		}
		return errors.New("injected marker failure")
	}
	for i, candidate := range b.projects {
		if candidate.id == id {
			b.projects = append(b.projects[:i], b.projects[i+1:]...)
			break
		}
	}
	return nil
}

func (b *adminDeleteTestBackend) deleteUserProjectMetadata(_ context.Context, userID, id string) error {
	b.deleteUserProjectID = userID
	if b.expectedUserID != "" && userID != b.expectedUserID {
		return fmt.Errorf("delete user project called for %q, want %q", userID, b.expectedUserID)
	}
	*b.events = append(*b.events, "user-project:"+id)
	if id == b.failUserProjectID {
		b.failUserProjectID = ""
		if b.userProjectErr != nil {
			return b.userProjectErr
		}
		return errors.New("injected user project metadata failure")
	}
	for i, candidate := range b.userProjects {
		if candidate.id == id {
			b.userProjects = append(b.userProjects[:i], b.userProjects[i+1:]...)
			break
		}
	}
	return nil
}

func (b *adminDeleteTestBackend) deleteUser(_ context.Context, userID string) error {
	b.deleteUserID = userID
	if b.expectedUserID != "" && userID != b.expectedUserID {
		return fmt.Errorf("delete user called for %q, want %q", userID, b.expectedUserID)
	}
	*b.events = append(*b.events, "user")
	return nil
}

func (b *adminDeleteTestBackend) projectByID(id string) (adminDeleteDocument, bool) {
	for _, project := range b.projects {
		if project.id == id {
			return project, true
		}
	}
	return adminDeleteDocument{}, false
}

func (d adminDeleteDocument) projectID() string {
	projectID, _ := d.data["project_id"].(string)
	return projectID
}

type adminDeleteRecordingRootStore struct {
	*adminStatsRootStore
	events *[]string
}

func (s *adminDeleteRecordingRootStore) DeleteProjectPrefix(_ context.Context, userID, projectID string) (int, error) {
	*s.events = append(*s.events, "gcs:"+userID+"/"+projectID)
	return 0, nil
}

func adminDeleteProjectDoc(id, userID, projectID string) adminDeleteDocument {
	return adminDeleteDocument{id: id, data: map[string]interface{}{
		"user_id":    userID,
		"project_id": projectID,
		"name":       "Authoritative name",
	}}
}

func adminDeleteLegacyProjectDoc(id, projectID string) adminDeleteDocument {
	return adminDeleteDocument{id: id, data: map[string]interface{}{
		"project_id": projectID,
		"name":       "Legacy name",
	}}
}

func adminDeleteMarkerDoc(id, targetProjectID string, includeUserID bool) adminDeleteDocument {
	return adminDeleteMarkerDocWithKey(id, targetProjectID, "idem-key", includeUserID)
}

func adminDeleteMarkerDocWithKey(id, targetProjectID, idempotencyKey string, includeUserID bool) adminDeleteDocument {
	data := map[string]interface{}{
		"project_id":      targetProjectID,
		"idempotency_key": idempotencyKey,
		"name":            "Cached init project",
	}
	if includeUserID {
		data["user_id"] = "user-a"
	}
	return adminDeleteDocument{id: id, data: data}
}

func newAdminDeleteRecordingHandler(backend *adminDeleteTestBackend, events *[]string) *Handler {
	root := &adminDeleteRecordingRootStore{
		adminStatsRootStore: &adminStatsRootStore{adminStatsProjectStore: &adminStatsProjectStore{prefix: "root"}},
		events:              events,
	}
	return &Handler{store: root, adminDeleteBackend: backend}
}

func TestAdminDeleteProjectBindsAllCleanupToAuthoritativeRecord(t *testing.T) {
	events := make([]string, 0, 3)
	backend := &adminDeleteTestBackend{
		projects: []adminDeleteDocument{adminDeleteProjectDoc("user-a_project-a", "user-a", "project-a")},
		events:   &events,
	}
	root := &adminDeleteRecordingRootStore{
		adminStatsRootStore: &adminStatsRootStore{adminStatsProjectStore: &adminStatsProjectStore{prefix: "root"}},
		events:              &events,
	}
	h := &Handler{store: root, adminDeleteBackend: backend}
	recorder := invokeHandlerWithParams(h.AdminDeleteProject, http.MethodDelete, "/admin/projects/user-a_project-a", gin.Params{{Key: "id", Value: "user-a_project-a"}})

	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	assert.Equal(t, []string{"gcs:user-a/project-a", "lock:user-a/project-a", "project:user-a_project-a"}, events)
	assert.Contains(t, recorder.Body.String(), `"name":"Authoritative name"`)
}

func TestAdminDeleteProjectAcceptsLegacyRealProjectWithoutUserID(t *testing.T) {
	events := []string{}
	backend := &adminDeleteTestBackend{
		projects: []adminDeleteDocument{adminDeleteLegacyProjectDoc("user-a_project-a", "project-a")},
		events:   &events,
	}
	h := newAdminDeleteRecordingHandler(backend, &events)
	recorder := invokeHandlerWithParams(h.AdminDeleteProject, http.MethodDelete, "/admin/projects/user-a_project-a", gin.Params{{Key: "id", Value: "user-a_project-a"}})

	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	assert.Equal(t, []string{"gcs:user-a/project-a", "lock:user-a/project-a", "project:user-a_project-a"}, events)
}

func TestAdminDeleteProjectRejectsIdempotencyMarker(t *testing.T) {
	events := []string{}
	backend := &adminDeleteTestBackend{
		projects: []adminDeleteDocument{adminDeleteMarkerDoc("user-a_idem-key", "project-a", true)},
		events:   &events,
	}
	h := newAdminDeleteRecordingHandler(backend, &events)
	recorder := invokeHandlerWithParams(h.AdminDeleteProject, http.MethodDelete, "/admin/projects/user-a_idem-key", gin.Params{{Key: "id", Value: "user-a_idem-key"}})

	if recorder.Code != http.StatusInternalServerError || len(events) != 0 {
		t.Fatalf("status=%d events=%v body=%s; want marker rejection before deletes", recorder.Code, events, recorder.Body.String())
	}
}

func TestAdminDeleteProjectRejectsUnderscoreIdempotencyMarker(t *testing.T) {
	events := []string{}
	backend := &adminDeleteTestBackend{
		projects: []adminDeleteDocument{
			adminDeleteMarkerDocWithKey("user-a_idem_key", "project-a", "idem_key", true),
		},
		events: &events,
	}
	h := newAdminDeleteRecordingHandler(backend, &events)
	recorder := invokeHandlerWithParams(h.AdminDeleteProject, http.MethodDelete, "/admin/projects/user-a_idem_key", gin.Params{{Key: "id", Value: "user-a_idem_key"}})

	if recorder.Code != http.StatusInternalServerError || len(events) != 0 {
		t.Fatalf("status=%d events=%v body=%s; want marker rejection before deletes", recorder.Code, events, recorder.Body.String())
	}
}

func TestAdminDeleteProjectAcceptsRealProjectIDWithUnderscores(t *testing.T) {
	events := []string{}
	backend := &adminDeleteTestBackend{
		projects: []adminDeleteDocument{
			adminDeleteProjectDoc("user-a_project_with_underscores", "user-a", "project_with_underscores"),
		},
		events: &events,
	}
	h := newAdminDeleteRecordingHandler(backend, &events)
	recorder := invokeHandlerWithParams(h.AdminDeleteProject, http.MethodDelete, "/admin/projects/user-a_project_with_underscores", gin.Params{{Key: "id", Value: "user-a_project_with_underscores"}})

	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	assert.Equal(t, []string{
		"gcs:user-a/project_with_underscores", "lock:user-a/project_with_underscores", "project:user-a_project_with_underscores",
	}, events)
}

func TestAdminDeleteProjectRejectsMismatchedStoredIdentityBeforeCleanup(t *testing.T) {
	for _, tc := range []struct {
		name string
		doc  adminDeleteDocument
	}{
		{name: "stored project mismatch", doc: adminDeleteProjectDoc("user-a_key-project", "user-a", "authoritative-project")},
		{name: "stored user mismatch", doc: adminDeleteProjectDoc("user-a_project-a", "user-b", "project-a")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			events := []string{}
			backend := &adminDeleteTestBackend{projects: []adminDeleteDocument{tc.doc}, events: &events}
			root := &adminDeleteRecordingRootStore{
				adminStatsRootStore: &adminStatsRootStore{adminStatsProjectStore: &adminStatsProjectStore{prefix: "root"}},
				events:              &events,
			}
			h := &Handler{store: root, adminDeleteBackend: backend}
			recorder := invokeHandlerWithParams(h.AdminDeleteProject, http.MethodDelete, "/admin/projects/"+tc.doc.id, gin.Params{{Key: "id", Value: tc.doc.id}})
			if recorder.Code != http.StatusInternalServerError || len(events) != 0 {
				t.Fatalf("status=%d events=%v body=%s; want validation failure before deletes", recorder.Code, events, recorder.Body.String())
			}
		})
	}
}

func TestAdminDeleteUserValidatesAllOwnedSnapshotsBeforeAnyDelete(t *testing.T) {
	for _, tc := range []struct {
		name     string
		projects []adminDeleteDocument
	}{
		{name: "owned key with foreign stored owner", projects: []adminDeleteDocument{adminDeleteProjectDoc("user-a_project-a", "user-b", "project-a")}},
		{name: "stored owner under foreign key", projects: []adminDeleteDocument{adminDeleteProjectDoc("user-b_project-a", "user-a", "project-a")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			events := []string{}
			backend := &adminDeleteTestBackend{
				user:     adminDeleteDocument{id: "user-a"},
				projects: tc.projects,
				events:   &events,
			}
			root := &adminDeleteRecordingRootStore{
				adminStatsRootStore: &adminStatsRootStore{adminStatsProjectStore: &adminStatsProjectStore{prefix: "root"}},
				events:              &events,
			}
			h := &Handler{store: root, adminDeleteBackend: backend}
			recorder := invokeHandlerWithParams(h.AdminDeleteUser, http.MethodDelete, "/admin/users/user-a", gin.Params{{Key: "id", Value: "user-a"}})
			if recorder.Code != http.StatusInternalServerError || len(events) != 0 {
				t.Fatalf("status=%d events=%v body=%s; want validation failure before deletes", recorder.Code, events, recorder.Body.String())
			}
		})
	}
}

func TestAdminDeleteUserFailsClosedWhenProjectListingFailsAfterPartialResults(t *testing.T) {
	events := []string{}
	project := adminDeleteProjectDoc("user-a_project-a", "user-a", "project-a")
	backend := &adminDeleteTestBackend{
		user:            adminDeleteDocument{id: "user-a"},
		projects:        []adminDeleteDocument{project},
		events:          &events,
		listProjectsErr: status.Error(codes.NotFound, "listing interrupted"),
	}
	h := newAdminDeleteRecordingHandler(backend, &events)

	recorder := invokeHandlerWithParams(h.AdminDeleteUser, http.MethodDelete, "/admin/users/user-a", gin.Params{{Key: "id", Value: "user-a"}})

	assert.Equal(t, http.StatusInternalServerError, recorder.Code)
	assert.Contains(t, recorder.Body.String(), `"error":"user delete unavailable"`)
	assert.Empty(t, events, "a failed project listing must not start cleanup")
	_, projectRemains := backend.projectByID(project.id)
	assert.True(t, projectRemains, "project retry anchor must remain")
	assert.Equal(t, "user-a", backend.user.id, "user retry anchor must remain")
}

func TestAdminDeleteUserDeletesUserScopedProjectMetadataBeforeUser(t *testing.T) {
	events := []string{}
	backend := &adminDeleteTestBackend{
		user:           adminDeleteDocument{id: "user-a"},
		userProjects:   []adminDeleteDocument{{id: "zeta"}, {id: "default"}, {id: "项目-α"}},
		expectedUserID: "user-a",
		events:         &events,
	}
	h := newAdminDeleteRecordingHandler(backend, &events)

	recorder := invokeHandlerWithParams(h.AdminDeleteUser, http.MethodDelete, "/admin/users/user-a", gin.Params{{Key: "id", Value: "user-a"}})

	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	assert.Equal(t, []string{"user-project:default", "user-project:zeta", "user-project:项目-α", "user"}, events)
	assert.Empty(t, backend.userProjects)
	assert.Equal(t, "user-a", backend.listUserProjectsID)
	assert.Equal(t, "user-a", backend.deleteUserProjectID)
}

func TestAdminDeleteUserOrdersAllCleanupPhasesDeterministically(t *testing.T) {
	events := []string{}
	backend := &adminDeleteTestBackend{
		user: adminDeleteDocument{id: "user-a"},
		projects: []adminDeleteDocument{
			adminDeleteMarkerDocWithKey("user-a_idem-z", "project-z", "idem-z", true),
			adminDeleteProjectDoc("user-a_project-z", "user-a", "project-z"),
			adminDeleteMarkerDocWithKey("user-a_idem-a", "project-a", "idem-a", true),
			adminDeleteProjectDoc("user-a_project-a", "user-a", "project-a"),
		},
		userProjects:   []adminDeleteDocument{{id: "zeta"}, {id: "default"}, {id: "项目-α"}},
		expectedUserID: "user-a",
		events:         &events,
	}
	h := newAdminDeleteRecordingHandler(backend, &events)

	recorder := invokeHandlerWithParams(h.AdminDeleteUser, http.MethodDelete, "/admin/users/user-a", gin.Params{{Key: "id", Value: "user-a"}})

	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	assert.Equal(t, []string{
		"gcs:user-a/project-a", "lock:user-a/project-a", "project:user-a_project-a",
		"gcs:user-a/project-z", "lock:user-a/project-z", "project:user-a_project-z",
		"marker:user-a_idem-a", "marker:user-a_idem-z",
		"user-project:default", "user-project:zeta", "user-project:项目-α", "user",
	}, events)
	assert.Equal(t, "user-a", backend.deleteUserProjectID)
	assert.Equal(t, "user-a", backend.deleteUserID)
}

func TestAdminDeleteUserRejectsUnsafeListedUserProjectIDBeforeCleanup(t *testing.T) {
	for _, projectID := range []string{"", ".", "..", "bad/id", `bad\id`, "bad\x00id", "bad\nid", "bad\u0085id", "bad\u202eid", "bad\u2028id", "bad\u2029id"} {
		t.Run(fmt.Sprintf("%q", projectID), func(t *testing.T) {
			events := []string{}
			backend := &adminDeleteTestBackend{
				user:         adminDeleteDocument{id: "user-a"},
				userProjects: []adminDeleteDocument{{id: "valid"}, {id: projectID}},
				events:       &events,
			}
			h := newAdminDeleteRecordingHandler(backend, &events)

			recorder := invokeHandlerWithParams(h.AdminDeleteUser, http.MethodDelete, "/admin/users/user-a", gin.Params{{Key: "id", Value: "user-a"}})

			assert.Equal(t, http.StatusInternalServerError, recorder.Code)
			assert.Empty(t, events, "unsafe listed child ID must fail before cleanup mutations")
			assert.Len(t, backend.userProjects, 2)
		})
	}
}

func TestAdminDeleteUserRetryReenumeratesRemainingUserProjects(t *testing.T) {
	events := []string{}
	backend := &adminDeleteTestBackend{
		user:              adminDeleteDocument{id: "user-a"},
		userProjects:      []adminDeleteDocument{{id: "alpha"}, {id: "beta"}},
		expectedUserID:    "user-a",
		failUserProjectID: "beta",
		userProjectErr:    errors.New("injected user project metadata failure"),
		events:            &events,
	}
	h := newAdminDeleteRecordingHandler(backend, &events)

	first := invokeHandlerWithParams(h.AdminDeleteUser, http.MethodDelete, "/admin/users/user-a", gin.Params{{Key: "id", Value: "user-a"}})
	if first.Code != http.StatusInternalServerError {
		t.Fatalf("first status=%d body=%s", first.Code, first.Body.String())
	}
	assert.Equal(t, []adminDeleteDocument{{id: "beta"}}, backend.userProjects)
	assert.Equal(t, "user-a", backend.user.id)

	second := invokeHandlerWithParams(h.AdminDeleteUser, http.MethodDelete, "/admin/users/user-a", gin.Params{{Key: "id", Value: "user-a"}})
	if second.Code != http.StatusOK {
		t.Fatalf("second status=%d body=%s", second.Code, second.Body.String())
	}
	assert.Empty(t, backend.userProjects)
	assert.Equal(t, "user-a", backend.listUserProjectsID)
	assert.Equal(t, "user-a", backend.deleteUserProjectID)
	assert.Equal(t, "user-a", backend.deleteUserID)
	assert.Equal(t, []string{"user-project:alpha", "user-project:beta", "user-project:beta", "user"}, events)
}

func TestAdminDeleteUserFailsClosedWhenUserProjectListingFails(t *testing.T) {
	events := []string{}
	backend := &adminDeleteTestBackend{
		user:                adminDeleteDocument{id: "user-a"},
		userProjects:        []adminDeleteDocument{{id: "default"}},
		listUserProjectsErr: errors.New("listing interrupted"),
		events:              &events,
	}
	h := newAdminDeleteRecordingHandler(backend, &events)

	recorder := invokeHandlerWithParams(h.AdminDeleteUser, http.MethodDelete, "/admin/users/user-a", gin.Params{{Key: "id", Value: "user-a"}})

	assert.Equal(t, http.StatusInternalServerError, recorder.Code)
	assert.Empty(t, events, "a failed user-project listing must not start cleanup")
	assert.Len(t, backend.userProjects, 1)
}

func TestAdminDeleteUserDoesNotDeleteUserAfterUserProjectMetadataFailure(t *testing.T) {
	events := []string{}
	backend := &adminDeleteTestBackend{
		user:              adminDeleteDocument{id: "user-a"},
		userProjects:      []adminDeleteDocument{{id: "default"}},
		failUserProjectID: "default",
		userProjectErr:    errors.New("metadata delete failed"),
		events:            &events,
	}
	h := newAdminDeleteRecordingHandler(backend, &events)

	recorder := invokeHandlerWithParams(h.AdminDeleteUser, http.MethodDelete, "/admin/users/user-a", gin.Params{{Key: "id", Value: "user-a"}})

	assert.Equal(t, http.StatusInternalServerError, recorder.Code)
	assert.Equal(t, []string{"user-project:default"}, events)
	assert.Len(t, backend.userProjects, 1)
}

func TestAdminDeleteProjectListResultOnlyTreatsIteratorDoneAsExhaustion(t *testing.T) {
	partial := []adminDeleteDocument{{id: "user-a_project-a"}}

	got, err := adminDeleteProjectListResult(partial, iterator.Done)
	assert.NoError(t, err)
	assert.Equal(t, partial, got)

	got, err = adminDeleteProjectListResult(partial, status.Error(codes.NotFound, "listing interrupted"))
	assert.Error(t, err)
	assert.Nil(t, got)
}

func TestAdminDeleteUserSkipsForeignLegacyRealProjectWithShortOwnerPrefix(t *testing.T) {
	events := []string{}
	backend := &adminDeleteTestBackend{
		user: adminDeleteDocument{id: "user"},
		projects: []adminDeleteDocument{
			adminDeleteLegacyProjectDoc("user_target", "target"),
			adminDeleteLegacyProjectDoc("user_x_project", "project"),
		},
		events: &events,
	}
	h := newAdminDeleteRecordingHandler(backend, &events)
	recorder := invokeHandlerWithParams(h.AdminDeleteUser, http.MethodDelete, "/admin/users/user", gin.Params{{Key: "id", Value: "user"}})

	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	assert.Equal(t, []string{
		"gcs:user/target", "lock:user/target", "project:user_target", "user",
	}, events)
}

func TestAdminDeleteUserSkipsForeignLegacyMarkerWithShortOwnerPrefix(t *testing.T) {
	events := []string{}
	backend := &adminDeleteTestBackend{
		user:     adminDeleteDocument{id: "user"},
		projects: []adminDeleteDocument{adminDeleteMarkerDocWithKey("user_x_idem_key", "project", "idem_key", false)},
		events:   &events,
	}
	h := newAdminDeleteRecordingHandler(backend, &events)
	recorder := invokeHandlerWithParams(h.AdminDeleteUser, http.MethodDelete, "/admin/users/user", gin.Params{{Key: "id", Value: "user"}})

	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	assert.Equal(t, []string{"user"}, events)
	assert.Len(t, backend.projects, 1)
}

func TestAdminDeleteUserReversePrefixOwnerDeletesItsLegacyDocuments(t *testing.T) {
	events := []string{}
	backend := &adminDeleteTestBackend{
		user: adminDeleteDocument{id: "user_x"},
		projects: []adminDeleteDocument{
			adminDeleteLegacyProjectDoc("user_x_project", "project"),
			adminDeleteMarkerDocWithKey("user_x_idem_key", "project", "idem_key", false),
		},
		events: &events,
	}
	h := newAdminDeleteRecordingHandler(backend, &events)
	recorder := invokeHandlerWithParams(h.AdminDeleteUser, http.MethodDelete, "/admin/users/user_x", gin.Params{{Key: "id", Value: "user_x"}})

	if recorder.Code != http.StatusOK || len(backend.projects) != 0 {
		t.Fatalf("status=%d remaining=%v body=%s", recorder.Code, backend.projects, recorder.Body.String())
	}
	assert.Equal(t, []string{
		"gcs:user_x/project", "lock:user_x/project", "project:user_x_project",
		"marker:user_x_idem_key", "user",
	}, events)
}

func TestAdminDeleteUserRejectsAmbiguousLegacyRealAndMarkerIdentity(t *testing.T) {
	events := []string{}
	backend := &adminDeleteTestBackend{
		user: adminDeleteDocument{id: "user"},
		projects: []adminDeleteDocument{{
			id: "user_x_project",
			data: map[string]interface{}{
				"project_id":      "project",
				"idempotency_key": "x_project",
			},
		}},
		events: &events,
	}
	h := newAdminDeleteRecordingHandler(backend, &events)
	recorder := invokeHandlerWithParams(h.AdminDeleteUser, http.MethodDelete, "/admin/users/user", gin.Params{{Key: "id", Value: "user"}})

	if recorder.Code != http.StatusInternalServerError || len(events) != 0 {
		t.Fatalf("status=%d events=%v body=%s; want ambiguous identity rejected before mutation", recorder.Code, events, recorder.Body.String())
	}
}

func TestAdminDeleteUserDeletesMarkerMetadataAfterRealProjects(t *testing.T) {
	events := []string{}
	backend := &adminDeleteTestBackend{
		user: adminDeleteDocument{id: "user-a"},
		projects: []adminDeleteDocument{
			adminDeleteProjectDoc("user-a_project-a", "user-a", "project-a"),
			adminDeleteMarkerDoc("user-a_idem-key", "project-a", true),
		},
		events: &events,
	}
	h := newAdminDeleteRecordingHandler(backend, &events)
	recorder := invokeHandlerWithParams(h.AdminDeleteUser, http.MethodDelete, "/admin/users/user-a", gin.Params{{Key: "id", Value: "user-a"}})

	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	assert.Equal(t, []string{
		"gcs:user-a/project-a", "lock:user-a/project-a", "project:user-a_project-a",
		"marker:user-a_idem-key", "user",
	}, events)
}

func TestAdminDeleteUserAcceptsMarkerWithUnderscoreIdempotencyKey(t *testing.T) {
	events := []string{}
	backend := &adminDeleteTestBackend{
		user: adminDeleteDocument{id: "user-a"},
		projects: []adminDeleteDocument{
			adminDeleteProjectDoc("user-a_project-a", "user-a", "project-a"),
			{id: "user-a_idem_key", data: map[string]interface{}{
				"user_id":         "user-a",
				"project_id":      "project-a",
				"idempotency_key": "idem_key",
				"name":            "Cached init project",
			}},
		},
		events: &events,
	}
	h := newAdminDeleteRecordingHandler(backend, &events)
	recorder := invokeHandlerWithParams(h.AdminDeleteUser, http.MethodDelete, "/admin/users/user-a", gin.Params{{Key: "id", Value: "user-a"}})

	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	assert.Equal(t, []string{
		"gcs:user-a/project-a", "lock:user-a/project-a", "project:user-a_project-a",
		"marker:user-a_idem_key", "user",
	}, events)
}

func TestAdminDeleteUserMarkerOnlyWithUnderscoreIdempotencyKeyConverges(t *testing.T) {
	events := []string{}
	backend := &adminDeleteTestBackend{
		user:     adminDeleteDocument{id: "user-a"},
		projects: []adminDeleteDocument{adminDeleteMarkerDocWithKey("user-a_idem_key", "project-a", "idem_key", true)},
		events:   &events,
	}
	h := newAdminDeleteRecordingHandler(backend, &events)
	recorder := invokeHandlerWithParams(h.AdminDeleteUser, http.MethodDelete, "/admin/users/user-a", gin.Params{{Key: "id", Value: "user-a"}})

	if recorder.Code != http.StatusOK || len(backend.projects) != 0 {
		t.Fatalf("status=%d remaining=%v body=%s", recorder.Code, backend.projects, recorder.Body.String())
	}
	assert.Equal(t, []string{"marker:user-a_idem_key", "user"}, events)
}

func TestAdminDeleteUserUnderscoreMarkerRetryPreservesAnchorAndConverges(t *testing.T) {
	events := []string{}
	backend := &adminDeleteTestBackend{
		user: adminDeleteDocument{id: "user-a"},
		projects: []adminDeleteDocument{
			adminDeleteProjectDoc("user-a_project-a", "user-a", "project-a"),
			adminDeleteMarkerDocWithKey("user-a_idem_key", "project_a", "idem_key", true),
		},
		events:       &events,
		failMarkerID: "user-a_idem_key",
		markerErr:    errors.New("injected marker failure"),
	}
	h := newAdminDeleteRecordingHandler(backend, &events)

	first := invokeHandlerWithParams(h.AdminDeleteUser, http.MethodDelete, "/admin/users/user-a", gin.Params{{Key: "id", Value: "user-a"}})
	if first.Code != http.StatusInternalServerError || len(backend.projects) != 1 || backend.projects[0].id != "user-a_idem_key" {
		t.Fatalf("first status=%d remaining=%v body=%s", first.Code, backend.projects, first.Body.String())
	}
	second := invokeHandlerWithParams(h.AdminDeleteUser, http.MethodDelete, "/admin/users/user-a", gin.Params{{Key: "id", Value: "user-a"}})
	if second.Code != http.StatusOK || len(backend.projects) != 0 {
		t.Fatalf("retry status=%d remaining=%v body=%s", second.Code, backend.projects, second.Body.String())
	}
	assert.Equal(t, []string{
		"gcs:user-a/project-a", "lock:user-a/project-a", "project:user-a_project-a",
		"marker:user-a_idem_key", "marker:user-a_idem_key", "user",
	}, events)
}

func TestAdminDeleteUserAcceptsRealProjectIDWithUnderscores(t *testing.T) {
	events := []string{}
	backend := &adminDeleteTestBackend{
		user: adminDeleteDocument{id: "user-a"},
		projects: []adminDeleteDocument{
			adminDeleteProjectDoc("user-a_project_with_underscores", "user-a", "project_with_underscores"),
		},
		events: &events,
	}
	h := newAdminDeleteRecordingHandler(backend, &events)
	recorder := invokeHandlerWithParams(h.AdminDeleteUser, http.MethodDelete, "/admin/users/user-a", gin.Params{{Key: "id", Value: "user-a"}})

	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	assert.Equal(t, []string{
		"gcs:user-a/project_with_underscores", "lock:user-a/project_with_underscores", "project:user-a_project_with_underscores", "user",
	}, events)
}

func TestAdminDeleteUserAcceptsLegacyMarkerWithoutUserID(t *testing.T) {
	events := []string{}
	backend := &adminDeleteTestBackend{
		user: adminDeleteDocument{id: "user-a"},
		projects: []adminDeleteDocument{
			adminDeleteLegacyProjectDoc("user-a_project-a", "project-a"),
			adminDeleteMarkerDoc("user-a_idem-key", "project-a", false),
		},
		events: &events,
	}
	h := newAdminDeleteRecordingHandler(backend, &events)
	recorder := invokeHandlerWithParams(h.AdminDeleteUser, http.MethodDelete, "/admin/users/user-a", gin.Params{{Key: "id", Value: "user-a"}})

	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	assert.Equal(t, []string{
		"gcs:user-a/project-a", "lock:user-a/project-a", "project:user-a_project-a",
		"marker:user-a_idem-key", "user",
	}, events)
}

func TestAdminDeleteUserAcceptsLegacyMarkerWithUnderscoreIdempotencyKey(t *testing.T) {
	events := []string{}
	backend := &adminDeleteTestBackend{
		user:     adminDeleteDocument{id: "user-a"},
		projects: []adminDeleteDocument{adminDeleteMarkerDocWithKey("user-a_idem_key", "project-a", "idem_key", false)},
		events:   &events,
	}
	h := newAdminDeleteRecordingHandler(backend, &events)
	recorder := invokeHandlerWithParams(h.AdminDeleteUser, http.MethodDelete, "/admin/users/user-a", gin.Params{{Key: "id", Value: "user-a"}})

	if recorder.Code != http.StatusOK || len(backend.projects) != 0 {
		t.Fatalf("status=%d remaining=%v body=%s", recorder.Code, backend.projects, recorder.Body.String())
	}
	assert.Equal(t, []string{"marker:user-a_idem_key", "user"}, events)
}

func TestAdminDeleteUserRejectsCorruptMarkerBeforeAnyDelete(t *testing.T) {
	events := []string{}
	backend := &adminDeleteTestBackend{
		user: adminDeleteDocument{id: "user-a"},
		projects: []adminDeleteDocument{{
			id: "user-a_idem-key",
			data: map[string]interface{}{
				"user_id":         "user-a",
				"project_id":      "idem-key",
				"idempotency_key": "idem-key",
			},
		}},
		events: &events,
	}
	h := newAdminDeleteRecordingHandler(backend, &events)
	recorder := invokeHandlerWithParams(h.AdminDeleteUser, http.MethodDelete, "/admin/users/user-a", gin.Params{{Key: "id", Value: "user-a"}})

	if recorder.Code != http.StatusInternalServerError || len(events) != 0 {
		t.Fatalf("status=%d events=%v body=%s; want fail closed before deletes", recorder.Code, events, recorder.Body.String())
	}
}

func TestAdminDeleteUserMarkerFailurePreservesRetryAnchor(t *testing.T) {
	events := []string{}
	backend := &adminDeleteTestBackend{
		user: adminDeleteDocument{id: "user-a"},
		projects: []adminDeleteDocument{
			adminDeleteProjectDoc("user-a_project-a", "user-a", "project-a"),
			adminDeleteMarkerDoc("user-a_idem-key", "project-a", true),
		},
		events:       &events,
		failMarkerID: "user-a_idem-key",
		markerErr:    errors.New("injected marker failure"),
	}
	h := newAdminDeleteRecordingHandler(backend, &events)

	first := invokeHandlerWithParams(h.AdminDeleteUser, http.MethodDelete, "/admin/users/user-a", gin.Params{{Key: "id", Value: "user-a"}})
	if first.Code != http.StatusInternalServerError || len(backend.projects) != 1 || backend.projects[0].id != "user-a_idem-key" {
		t.Fatalf("first status=%d remaining=%v body=%s", first.Code, backend.projects, first.Body.String())
	}
	second := invokeHandlerWithParams(h.AdminDeleteUser, http.MethodDelete, "/admin/users/user-a", gin.Params{{Key: "id", Value: "user-a"}})
	if second.Code != http.StatusOK || len(backend.projects) != 0 {
		t.Fatalf("retry status=%d remaining=%v body=%s", second.Code, backend.projects, second.Body.String())
	}
	assert.Equal(t, []string{
		"gcs:user-a/project-a", "lock:user-a/project-a", "project:user-a_project-a",
		"marker:user-a_idem-key", "marker:user-a_idem-key", "user",
	}, events)
}

func TestAdminDeleteUserRetryReenumeratesRemainingProjects(t *testing.T) {
	events := []string{}
	backend := &adminDeleteTestBackend{
		user: adminDeleteDocument{id: "user-a"},
		projects: []adminDeleteDocument{
			adminDeleteProjectDoc("user-a_project-a", "user-a", "project-a"),
			adminDeleteProjectDoc("user-a_project-b", "user-a", "project-b"),
		},
		events:        &events,
		failProjectID: "project-b",
		projectErr:    errors.New("injected project failure"),
	}
	root := &adminDeleteRecordingRootStore{
		adminStatsRootStore: &adminStatsRootStore{adminStatsProjectStore: &adminStatsProjectStore{prefix: "root"}},
		events:              &events,
	}
	h := &Handler{store: root, adminDeleteBackend: backend}

	first := invokeHandlerWithParams(h.AdminDeleteUser, http.MethodDelete, "/admin/users/user-a", gin.Params{{Key: "id", Value: "user-a"}})
	if first.Code != http.StatusInternalServerError || len(backend.projects) != 1 || backend.projects[0].id != "user-a_project-b" {
		t.Fatalf("first status=%d remaining=%v body=%s", first.Code, backend.projects, first.Body.String())
	}
	second := invokeHandlerWithParams(h.AdminDeleteUser, http.MethodDelete, "/admin/users/user-a", gin.Params{{Key: "id", Value: "user-a"}})
	if second.Code != http.StatusOK || len(backend.projects) != 0 {
		t.Fatalf("retry status=%d remaining=%v body=%s", second.Code, backend.projects, second.Body.String())
	}
	assert.Equal(t, []string{
		"gcs:user-a/project-a", "lock:user-a/project-a", "project:user-a_project-a",
		"gcs:user-a/project-b", "lock:user-a/project-b", "project:user-a_project-b",
		"gcs:user-a/project-b", "lock:user-a/project-b", "project:user-a_project-b", "user",
	}, events)
}

func TestAdminDeleteHandlersReachUnsupportedCapabilityAfterValidation(t *testing.T) {
	backend := &adminDeleteTestBackend{
		user:     adminDeleteDocument{id: "user-a"},
		projects: []adminDeleteDocument{adminDeleteProjectDoc("user-a_project-a", "user-a", "project-a")},
		events:   &[]string{},
	}
	h := &Handler{store: &adminStatsRootStore{adminStatsProjectStore: &adminStatsProjectStore{prefix: "root"}}, adminDeleteBackend: backend}
	project := invokeHandlerWithParams(h.AdminDeleteProject, http.MethodDelete, "/admin/projects/user-a_project-a", gin.Params{{Key: "id", Value: "user-a_project-a"}})
	user := invokeHandlerWithParams(h.AdminDeleteUser, http.MethodDelete, "/admin/users/user-a", gin.Params{{Key: "id", Value: "user-a"}})
	if project.Code != http.StatusInternalServerError || user.Code != http.StatusInternalServerError {
		t.Fatalf("unsupported capability statuses: project=%d user=%d", project.Code, user.Code)
	}
}

func TestAdminProjectRecordFromFirestoreDocRejectsMalformedProject(t *testing.T) {
	if _, ok := adminProjectRecordFromFirestoreDoc("malformed", map[string]interface{}{
		"name": "Malformed",
	}); ok {
		t.Fatal("malformed project document must not be counted")
	}
}

func TestAdminProjectRecordFromFirestoreDocRejectsUnscopedProjectID(t *testing.T) {
	if _, ok := adminProjectRecordFromFirestoreDoc("malformed", map[string]interface{}{
		"project_id": "authoritative-project",
		"name":       "Malformed",
	}); ok {
		t.Fatal("project document without an owner prefix must not be counted")
	}
}

func TestAdminProjectRecordFromFirestoreDocRejectsUnsafeStorageSegments(t *testing.T) {
	for _, tc := range []struct {
		name  string
		docID string
		data  map[string]interface{}
	}{
		{
			name:  "unsafe owner",
			docID: "user/../other_project",
			data:  map[string]interface{}{"project_id": "project"},
		},
		{
			name:  "unsafe project",
			docID: "user-long_project",
			data:  map[string]interface{}{"project_id": "../other"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := adminProjectRecordFromFirestoreDoc(tc.docID, tc.data); ok {
				t.Fatal("unsafe storage segment must not be counted")
			}
		})
	}
}

func TestAdminStatisticsJSONFieldContract(t *testing.T) {
	projectJSON, err := json.Marshal(adminProjectEntry{
		ID:           "user-a_project-a",
		ConceptCount: 2,
		SourceCount:  1,
	})
	if err != nil {
		t.Fatalf("marshal project entry: %v", err)
	}
	userJSON, err := json.Marshal(adminUserEntry{ID: "user-a", ProjectCount: 2})
	if err != nil {
		t.Fatalf("marshal user entry: %v", err)
	}

	var project map[string]interface{}
	var user map[string]interface{}
	if err := json.Unmarshal(projectJSON, &project); err != nil {
		t.Fatalf("decode project JSON: %v", err)
	}
	if err := json.Unmarshal(userJSON, &user); err != nil {
		t.Fatalf("decode user JSON: %v", err)
	}
	assert.Equal(t, float64(2), project["concept_count"])
	assert.Equal(t, float64(1), project["source_count"])
	assert.Equal(t, float64(2), user["project_count"])
}

// =========================================================================
// AdminRenameProject tests
// =========================================================================

// TestAdminRenameProject_ValidRename_RouteLevel tests that a valid PATCH
// request with a project ID and name body returns 200 at the route level.
// This test uses a nil Handler so the handler returns 500 (firestore not
// configured); it primarily validates Gin routing and content-type handling.
func TestAdminRenameProject_ValidRoute_RouteLevel(t *testing.T) {
	gin.SetMode(gin.TestMode)

	r := gin.New()
	h := &Handler{}
	r.PATCH("/admin/projects/:id", h.AdminRenameProject)

	body := `{"name":"New Project Name"}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("PATCH", "/admin/projects/user-1_proj-123", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	// With nil firestore the handler returns 500, but the route matches
	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

// TestAdminRenameProject_MissingProjectID tests that an empty project ID
// returns 400 Bad Request.
func TestAdminRenameProject_MissingProjectID(t *testing.T) {
	gin.SetMode(gin.TestMode)

	h := &Handler{}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	body := `{"name":"Some Name"}`
	c.Request = httptest.NewRequest(http.MethodPatch, "/admin/projects/", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	h.AdminRenameProject(c)

	assert.Equal(t, http.StatusBadRequest, recorder.Code)

	var resp map[string]string
	err := json.Unmarshal(recorder.Body.Bytes(), &resp)
	assert.NoError(t, err)
	assert.Equal(t, "invalid project doc ID", resp["error"])
}

// TestAdminRenameProject_EmptyName tests that an empty name in the request
// body returns 400 Bad Request with "name is required".
func TestAdminRenameProject_EmptyName(t *testing.T) {
	gin.SetMode(gin.TestMode)

	h := &Handler{}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	body := `{"name":""}`
	c.Request = httptest.NewRequest(http.MethodPatch, "/admin/projects/user-1_proj-123", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Params = gin.Params{{Key: "id", Value: "user-1_proj-123"}}

	h.AdminRenameProject(c)

	assert.Equal(t, http.StatusBadRequest, recorder.Code)

	var resp map[string]string
	err := json.Unmarshal(recorder.Body.Bytes(), &resp)
	assert.NoError(t, err)
	assert.Equal(t, "name is required", resp["error"])
}

// TestAdminRenameProject_InvalidJSON tests that an invalid JSON body returns
// 400 Bad Request with an "invalid JSON" error message.
func TestAdminRenameProject_InvalidJSON(t *testing.T) {
	gin.SetMode(gin.TestMode)

	h := &Handler{}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPatch, "/admin/projects/user-1_proj-123", strings.NewReader(`not json`))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Params = gin.Params{{Key: "id", Value: "user-1_proj-123"}}

	h.AdminRenameProject(c)

	assert.Equal(t, http.StatusBadRequest, recorder.Code)

	var resp map[string]string
	err := json.Unmarshal(recorder.Body.Bytes(), &resp)
	assert.NoError(t, err)
	assert.Contains(t, resp["error"], "invalid JSON")
}

// TestAdminRenameProject_NoFirestore tests that a nil firestore client
// returns 500 Internal Server Error.
func TestAdminRenameProject_NoFirestore(t *testing.T) {
	gin.SetMode(gin.TestMode)

	h := &Handler{} // firestore is nil
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	body := `{"name":"Valid Name"}`
	c.Request = httptest.NewRequest(http.MethodPatch, "/admin/projects/user-1_proj-123", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Params = gin.Params{{Key: "id", Value: "user-1_proj-123"}}

	h.AdminRenameProject(c)

	assert.Equal(t, http.StatusInternalServerError, recorder.Code)

	var resp map[string]string
	err := json.Unmarshal(recorder.Body.Bytes(), &resp)
	assert.NoError(t, err)
	assert.Equal(t, "Firestore client is not configured", resp["error"])
}

// TestAdminRenameProject_MissingProject_Route404 tests that a PATCH request
// without a project ID in the URL returns 404 (Gin route mismatch).
func TestAdminRenameProject_MissingProject_Route404(t *testing.T) {
	gin.SetMode(gin.TestMode)

	r := gin.New()
	h := &Handler{}
	r.PATCH("/admin/projects/:id", h.AdminRenameProject)

	body := `{"name":"Some Name"}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("PATCH", "/admin/projects/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code)
}

// =========================================================================
// AdminDeleteProject tests
// =========================================================================

// TestAdminDeleteProject_MissingProjectID tests that an empty project ID
// returns 400 Bad Request.
func TestAdminDeleteProject_MissingProjectID(t *testing.T) {
	gin.SetMode(gin.TestMode)

	h := &Handler{}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodDelete, "/admin/projects/", nil)

	h.AdminDeleteProject(c)

	assert.Equal(t, http.StatusBadRequest, recorder.Code)

	var resp map[string]string
	err := json.Unmarshal(recorder.Body.Bytes(), &resp)
	assert.NoError(t, err)
	assert.Equal(t, "invalid project doc ID", resp["error"])
}

func TestAdminDeleteProjectRejectsUnsafeIDsBeforeFirestore(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, docID := range []string{
		".._project-123",
		"user-123456_..",
		"user/../tenant_project-123",
		"user\\tenant_project-123",
		"user-123456_project/../other",
		"user-123456_project\x00suffix",
	} {
		t.Run(fmt.Sprintf("%q", docID), func(t *testing.T) {
			h := &Handler{}
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodDelete, "/", nil)
			c.Params = gin.Params{{Key: "id", Value: docID}}

			h.AdminDeleteProject(c)

			assert.Equal(t, http.StatusBadRequest, recorder.Code)
		})
	}
}

// TestAdminDeleteProject_NoFirestore tests that a nil firestore client
// returns 500 Internal Server Error.
func TestAdminDeleteProject_NoFirestore(t *testing.T) {
	gin.SetMode(gin.TestMode)

	h := &Handler{} // firestore is nil
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodDelete, "/admin/projects/user-1_proj-123", nil)
	c.Params = gin.Params{{Key: "id", Value: "user-1_proj-123"}}

	h.AdminDeleteProject(c)

	assert.Equal(t, http.StatusInternalServerError, recorder.Code)

	var resp map[string]string
	err := json.Unmarshal(recorder.Body.Bytes(), &resp)
	assert.NoError(t, err)
	assert.Equal(t, "Firestore client is not configured", resp["error"])
}

// TestAdminDeleteProject_MissingProject_Route404 tests that a DELETE request
// without a project ID in the URL returns 404 (Gin route mismatch).
func TestAdminDeleteProject_MissingProject_Route404(t *testing.T) {
	gin.SetMode(gin.TestMode)

	r := gin.New()
	h := &Handler{}
	r.DELETE("/admin/projects/:id", h.AdminDeleteProject)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("DELETE", "/admin/projects/", nil)
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestAdminDeleteUserRejectsUnsafeIDBeforeFirestore(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, userID := range []string{"", ".", "..", "user/tenant", `user\tenant`, "user\x00tenant", "user/../tenant"} {
		t.Run(fmt.Sprintf("%q", userID), func(t *testing.T) {
			h := &Handler{}
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodDelete, "/", nil)
			c.Params = gin.Params{{Key: "id", Value: userID}}

			h.AdminDeleteUser(c)

			assert.Equal(t, http.StatusBadRequest, recorder.Code)
		})
	}
}

func TestDeleteAdminProjectResourcesFailsClosedAtEachStage(t *testing.T) {
	stageErr := errors.New("injected cleanup failure")
	for _, tc := range []struct {
		name       string
		failStage  int
		wantCalled int
	}{
		{name: "gcs", failStage: 0, wantCalled: 1},
		{name: "lock", failStage: 1, wantCalled: 2},
		{name: "project", failStage: 2, wantCalled: 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			cleanup := func(context.Context) error {
				stage := calls
				calls++
				if stage == tc.failStage {
					return stageErr
				}
				return nil
			}

			if err := deleteAdminProjectResources(context.Background(), cleanup, cleanup, cleanup); err == nil {
				t.Fatal("deleteAdminProjectResources() error = nil, want failure")
			}
			if calls != tc.wantCalled {
				t.Fatalf("cleanup calls = %d, want %d", calls, tc.wantCalled)
			}
		})
	}
}

func TestDeleteAdminProjectResourcesRetriesAfterPartialProgress(t *testing.T) {
	projectDeleteCalls := 0
	cleanup := func(context.Context) error { return nil }
	deleteProject := func(context.Context) error {
		projectDeleteCalls++
		if projectDeleteCalls == 1 {
			return errors.New("injected project delete failure")
		}
		return nil
	}

	if err := deleteAdminProjectResources(context.Background(), cleanup, cleanup, deleteProject); err == nil {
		t.Fatal("first cleanup error = nil, want failure")
	}
	// GCS and lock cleanup are intentionally safe to repeat after the first
	// attempt made progress. A retry must be able to finish the project.
	if err := deleteAdminProjectResources(context.Background(), cleanup, cleanup, deleteProject); err != nil {
		t.Fatalf("retry cleanup error = %v, want nil", err)
	}
}

func TestDeleteAdminUserResourcesDoesNotDeleteUserAfterProjectFailure(t *testing.T) {
	userDeleteCalls := 0
	projectCalls := 0
	projectErr := errors.New("injected project cleanup failure")
	cleanupProject := func(context.Context, string) error {
		projectCalls++
		if projectCalls == 2 {
			return projectErr
		}
		return nil
	}
	deleteUser := func(context.Context) error {
		userDeleteCalls++
		return nil
	}

	if err := deleteAdminUserResources(context.Background(), []string{"project-1", "project-2"}, cleanupProject, deleteUser); err == nil {
		t.Fatal("user cleanup error = nil, want failure")
	}
	if userDeleteCalls != 0 {
		t.Fatalf("user delete calls = %d, want 0", userDeleteCalls)
	}

	if err := deleteAdminUserResources(context.Background(), []string{"project-2"}, func(context.Context, string) error { return nil }, deleteUser); err != nil {
		t.Fatalf("user cleanup retry error = %v, want nil", err)
	}
	if userDeleteCalls != 1 {
		t.Fatalf("user delete calls after retry = %d, want 1", userDeleteCalls)
	}
}

func TestDeleteGCSProjectPrefixFailsClosedForUnsupportedStore(t *testing.T) {
	root := &adminStatsRootStore{adminStatsProjectStore: &adminStatsProjectStore{prefix: "root"}}
	if err := deleteGCSProjectPrefix(context.Background(), root, "u", "p"); err == nil {
		t.Fatal("deleteGCSProjectPrefix() error = nil, want unsupported capability failure")
	}
}

func TestDeleteGCSProjectPrefixTreatsAlreadyMissingAsSuccess(t *testing.T) {
	root := &adminDeleteProjectPrefixRootStore{
		adminStatsRootStore: &adminStatsRootStore{adminStatsProjectStore: &adminStatsProjectStore{prefix: "root"}},
		err:                 storage.ErrObjectNotExist,
	}
	if err := deleteGCSProjectPrefix(context.Background(), root, "u", "p"); err != nil {
		t.Fatalf("deleteGCSProjectPrefix() error = %v, want nil for missing prefix", err)
	}
}

func TestDeleteGCSProjectPrefixRejectsUnsafeIDsBeforeProviderCall(t *testing.T) {
	for _, tc := range []struct {
		name string
		uid  string
		pid  string
	}{
		{name: "empty user", uid: "", pid: "project"},
		{name: "dot user", uid: ".", pid: "project"},
		{name: "dotdot user", uid: "..", pid: "project"},
		{name: "slash user", uid: "user/tenant", pid: "project"},
		{name: "backslash user", uid: `user\tenant`, pid: "project"},
		{name: "nul user", uid: "user\x00tenant", pid: "project"},
		{name: "normalization user", uid: "user/../tenant", pid: "project"},
		{name: "empty project", uid: "user", pid: ""},
		{name: "dot project", uid: "user", pid: "."},
		{name: "dotdot project", uid: "user", pid: ".."},
		{name: "slash project", uid: "user", pid: "project/child"},
		{name: "backslash project", uid: "user", pid: `project\child`},
		{name: "nul project", uid: "user", pid: "project\x00child"},
		{name: "normalization project", uid: "user", pid: "project/../other"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := &adminCountingProjectPrefixRootStore{}
			if err := deleteGCSProjectPrefix(context.Background(), root, tc.uid, tc.pid); err == nil {
				t.Fatal("deleteGCSProjectPrefix() error = nil, want invalid ID failure")
			}
			if root.calls != 0 {
				t.Fatalf("provider calls = %d, want 0", root.calls)
			}
		})
	}
}

type adminDeleteProjectPrefixRootStore struct {
	*adminStatsRootStore
	err error
}

func (s *adminDeleteProjectPrefixRootStore) DeleteProjectPrefix(context.Context, string, string) (int, error) {
	return 0, s.err
}

type adminCountingProjectPrefixRootStore struct {
	calls int
}

func (s *adminCountingProjectPrefixRootStore) DeleteProjectPrefix(context.Context, string, string) (int, error) {
	s.calls++
	return 0, nil
}

// =========================================================================
// AdminRebuildIndex tests
// =========================================================================

// TestAdminRebuildIndex_MissingProjectID tests that an empty project ID
// returns 400 Bad Request.
func TestAdminRebuildIndex_MissingProjectID(t *testing.T) {
	gin.SetMode(gin.TestMode)

	h := &Handler{}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/admin/projects//rebuild-index", nil)

	h.AdminRebuildIndex(c)

	assert.Equal(t, http.StatusBadRequest, recorder.Code)

	var resp map[string]string
	err := json.Unmarshal(recorder.Body.Bytes(), &resp)
	assert.NoError(t, err)
	assert.Equal(t, "invalid project doc ID", resp["error"])
}

// TestAdminRebuildIndex_NoFirestore tests that a nil firestore client
// returns 500 Internal Server Error.
func TestAdminRebuildIndex_NoFirestore(t *testing.T) {
	gin.SetMode(gin.TestMode)

	h := &Handler{} // firestore is nil
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/admin/projects/user-1_proj-123/rebuild-index", nil)
	c.Params = gin.Params{{Key: "id", Value: "user-1_proj-123"}}

	h.AdminRebuildIndex(c)

	assert.Equal(t, http.StatusInternalServerError, recorder.Code)

	var resp map[string]string
	err := json.Unmarshal(recorder.Body.Bytes(), &resp)
	assert.NoError(t, err)
	assert.Equal(t, "Firestore client is not configured", resp["error"])
}

func TestGenerationProjectsRejectManualRebuildWithoutCallingWriter(t *testing.T) {
	gin.SetMode(gin.TestMode)
	project := &adminStatsProjectStore{hasManifest: true}
	root := &adminStatsRootStore{adminStatsProjectStore: project, scoped: map[string]*adminStatsProjectStore{"request-user/demo": project}}
	calls := 0
	h := &Handler{store: root, adminProjectRecordLoader: func(context.Context, string) (adminProjectRecord, error) {
		return adminProjectRecord{id: "request-user_demo", userID: "request-user", projectID: "demo"}, nil
	}, rebuildIndex: func(context.Context, string, string) (idMap, error) {
		calls++
		return idMap{}, nil
	}}

	request := func(admin bool) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		if admin {
			c.Request = httptest.NewRequest(http.MethodPost, "/admin/projects/request-user_demo/rebuild-index", nil)
			c.Params = gin.Params{{Key: "id", Value: "request-user_demo"}}
			h.AdminRebuildIndex(c)
		} else {
			c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/pipeline/rebuild-index", nil)
			c.Set("userID", "request-user")
			c.Set("projectID", "demo")
			h.RebuildIndex(c)
		}
		return recorder
	}
	for _, recorder := range []*httptest.ResponseRecorder{request(false), request(true)} {
		if recorder.Code != http.StatusConflict {
			t.Fatalf("manual rebuild status = %d, want 409; body = %s", recorder.Code, recorder.Body.String())
		}
		if !strings.Contains(recorder.Body.String(), "run the pipeline") {
			t.Fatalf("manual rebuild body = %s", recorder.Body.String())
		}
	}
	if calls != 0 {
		t.Fatalf("rebuild invoked %d times for generation project", calls)
	}

	project.hasManifest = false
	if recorder := request(false); recorder.Code != http.StatusOK || calls != 1 {
		t.Fatalf("legacy rebuild = %d, calls = %d; want 200 and 1", recorder.Code, calls)
	}
	project.hasManifest = true
	if recorder := request(false); recorder.Code != http.StatusConflict || calls != 1 {
		t.Fatalf("malformed manifest rebuild = %d, calls = %d; want 409 and 1", recorder.Code, calls)
	}
}

type adminGenerationRebuilderStore struct {
	*adminStatsProjectStore
	result store.GenerationRebuildResult
	err    error
	called bool
}

type adminGenerationRootStore struct{ *adminGenerationRebuilderStore }

func (s *adminGenerationRootStore) Scope(string, string) store.Store {
	return s.adminGenerationRebuilderStore
}

func (s *adminGenerationRebuilderStore) RebuildIndexGeneration(context.Context, store.GenerationRebuildPlanner) (store.GenerationRebuildResult, error) {
	s.called = true
	if s.err != nil {
		return store.GenerationRebuildResult{}, s.err
	}
	return s.result, nil
}

func TestAdminRebuildIndexUsesGenerationRebuilderAfterAuthorization(t *testing.T) {
	project := &adminGenerationRebuilderStore{
		adminStatsProjectStore: &adminStatsProjectStore{prefix: "users/user/project", hasManifest: true},
		result: store.GenerationRebuildResult{
			Status: "ok", OldGeneration: "g_old", NewGeneration: "g_new", ConceptCount: 2, SourceCount: 1,
		},
	}
	root := &adminGenerationRootStore{adminGenerationRebuilderStore: project}
	h := &Handler{store: root, adminProjectRecordLoader: func(context.Context, string) (adminProjectRecord, error) {
		return adminProjectRecord{id: "user-account_project-account", userID: "user-account", projectID: "project-account"}, nil
	}}

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/admin/projects/user-account_project-account/rebuild-index", nil)
	c.Params = gin.Params{{Key: "id", Value: "user-account_project-account"}}
	h.AdminRebuildIndex(c)

	if recorder.Code != http.StatusOK || !project.called {
		t.Fatalf("generation rebuild status=%d called=%v body=%s", recorder.Code, project.called, recorder.Body.String())
	}
	var response store.GenerationRebuildResult
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.NewGeneration != "g_new" || response.ConceptCount != 2 {
		t.Fatalf("response=%+v", response)
	}
}

func TestAdminRebuildIndexClassifiesOnlyGenerationMismatchAsConflict(t *testing.T) {
	for _, tc := range []struct {
		name       string
		err        error
		wantStatus int
	}{
		{name: "provider error containing cas label", err: errors.New("cas_conflict: permission denied"), wantStatus: http.StatusInternalServerError},
		{name: "generation mismatch", err: store.ErrGenerationMismatch, wantStatus: http.StatusConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			project := &adminGenerationRebuilderStore{
				adminStatsProjectStore: &adminStatsProjectStore{prefix: "users/user/project", hasManifest: true},
				err:                    tc.err,
			}
			root := &adminGenerationRootStore{adminGenerationRebuilderStore: project}
			h := &Handler{store: root, adminProjectRecordLoader: func(context.Context, string) (adminProjectRecord, error) {
				return adminProjectRecord{id: "user-account_project-account", userID: "user-account", projectID: "project-account"}, nil
			}}
			recorder := invokeHandlerWithParams(h.AdminRebuildIndex, http.MethodPost, "/admin/projects/user-account_project-account/rebuild-index", gin.Params{{Key: "id", Value: "user-account_project-account"}})
			if recorder.Code != tc.wantStatus {
				t.Fatalf("status=%d body=%s, want %d", recorder.Code, recorder.Body.String(), tc.wantStatus)
			}
		})
	}
}

func TestAdminRebuildIndexUsesAuthoritativeRecordAndFailsClosedOnMismatch(t *testing.T) {
	project := &adminGenerationRebuilderStore{
		adminStatsProjectStore: &adminStatsProjectStore{prefix: "users/authoritative/project", hasManifest: true},
	}
	root := &adminGenerationRootStore{adminGenerationRebuilderStore: project}
	loaderCalls := 0
	h := &Handler{
		store: root,
		adminProjectRecordLoader: func(context.Context, string) (adminProjectRecord, error) {
			loaderCalls++
			return adminProjectRecord{id: "caller_user_caller_project", userID: "authoritative-user", projectID: "authoritative-project"}, nil
		},
	}
	recorder := invokeHandlerWithParams(h.AdminRebuildIndex, http.MethodPost, "/admin/projects/caller_user_caller_project/rebuild-index", gin.Params{{Key: "id", Value: "caller_user_caller_project"}})
	if recorder.Code != http.StatusInternalServerError || loaderCalls != 1 || project.called {
		t.Fatalf("mismatched admin rebuild status=%d loaderCalls=%d writerCalled=%v body=%s", recorder.Code, loaderCalls, project.called, recorder.Body.String())
	}

	project.called = false
	h.adminProjectRecordLoader = func(context.Context, string) (adminProjectRecord, error) {
		return adminProjectRecord{id: "authoritative-user_authoritative-project", userID: "authoritative-user", projectID: "authoritative-project"}, nil
	}
	recorder = invokeHandlerWithParams(h.AdminRebuildIndex, http.MethodPost, "/admin/projects/authoritative-user_authoritative-project/rebuild-index", gin.Params{{Key: "id", Value: "authoritative-user_authoritative-project"}})
	if recorder.Code != http.StatusOK || !project.called {
		t.Fatalf("matching admin rebuild status=%d writerCalled=%v body=%s", recorder.Code, project.called, recorder.Body.String())
	}
}

func TestPublicRebuildNeverInvokesGenerationRebuilder(t *testing.T) {
	project := &adminGenerationRebuilderStore{
		adminStatsProjectStore: &adminStatsProjectStore{prefix: "users/user/project", hasManifest: true},
	}
	root := &adminGenerationRootStore{adminGenerationRebuilderStore: project}
	h := &Handler{store: root}

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/pipeline/rebuild-index", nil)
	c.Request.Header.Set("X-User-ID", "spoofed-user")
	c.Request.Header.Set("X-Project-ID", "spoofed-project")
	h.RebuildIndex(c)

	if recorder.Code != http.StatusConflict || project.called {
		t.Fatalf("public rebuild status=%d called=%v body=%s, want managed 409 and zero writer calls", recorder.Code, project.called, recorder.Body.String())
	}
}

func TestRebuildIndexAuthenticatesBeforeGenerationProbeAndSanitizesProbeErrors(t *testing.T) {
	gin.SetMode(gin.TestMode)
	project := &adminStatsProjectStore{manifestErr: errors.New("sentinel-provider-path/users/tenant")}
	root := &adminStatsRootStore{adminStatsProjectStore: project, scoped: map[string]*adminStatsProjectStore{"user/project": project}}
	h := &Handler{store: root}

	unauthenticated := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(unauthenticated)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/pipeline/rebuild-index", nil)
	h.RebuildIndex(c)
	if unauthenticated.Code != http.StatusUnauthorized || project.manifestCalls != 0 {
		t.Fatalf("unauthenticated rebuild = %d probes=%d, want 401 and 0", unauthenticated.Code, project.manifestCalls)
	}

	authenticated := httptest.NewRecorder()
	c, _ = gin.CreateTestContext(authenticated)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/pipeline/rebuild-index", nil)
	c.Set("userID", "user")
	c.Set("projectID", "project")
	h.RebuildIndex(c)
	if authenticated.Code != http.StatusInternalServerError || strings.Contains(authenticated.Body.String(), "sentinel-provider-path") {
		t.Fatalf("authenticated rebuild = %d body=%s", authenticated.Code, authenticated.Body.String())
	}
}

// TestAdminRebuildIndex_MissingProject_Route404 tests that a POST request
// without a project ID in the URL returns 404 (Gin route mismatch).
func TestAdminRebuildIndex_MissingProject_Route404(t *testing.T) {
	gin.SetMode(gin.TestMode)

	r := gin.New()
	h := &Handler{}
	r.POST("/admin/projects/:id/rebuild-index", h.AdminRebuildIndex)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/admin/projects/", nil)
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code)
}
