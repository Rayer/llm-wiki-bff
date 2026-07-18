package v1

import (
	"bytes"
	"context"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	conceptcache "github.com/rayer/llm-wiki-bff/internal/cache"
	"github.com/rayer/llm-wiki-bff/internal/localfs"
	"github.com/rayer/llm-wiki-bff/internal/search"
	store "github.com/rayer/llm-wiki-bff/internal/storage"
)

func TestGeneratedReadMapsInternalStorageNotFoundTo404(t *testing.T) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	writeGeneratedReadError(c, store.ErrObjectNotExist, "missing generated data")
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want %d", recorder.Code, http.StatusNotFound)
	}
}

const securitySentinel = "tenant-secret project-secret execution-secret .lwc/publish/generations/generation-secret https://provider.example/secret object-path-secret"

func TestGeneratedPipelineSecurityBoundaryMatrix(t *testing.T) {
	tests := []struct {
		name       string
		invoke     func() *httptest.ResponseRecorder
		wantStatus int
		wantBody   string
	}{
		{
			name: "generated index read",
			invoke: func() *httptest.ResponseRecorder {
				root := generatedIndexErrorRoot{RootStore: localfs.New(t.TempDir()), err: errors.New(securitySentinel)}
				h := New(root, nil, search.NewIndex(), conceptcache.New(), nil, nil)
				return invokeHandler(h.Index, http.MethodGet, "/api/v1/index", map[string]string{"userID": "tenant-secret", "projectID": "project-secret"})
			},
			wantStatus: http.StatusInternalServerError,
			wantBody:   `{"error":"generated data unavailable"}`,
		},
		{
			name: "pipeline run provider failure",
			invoke: func() *httptest.ResponseRecorder {
				client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					switch r.URL.Path {
					case "/token":
						return testHTTPResponse(http.StatusOK, `{"access_token":"test-token"}`), nil
					case "/run":
						return testHTTPResponse(http.StatusForbidden, securitySentinel), nil
					default:
						return testHTTPResponse(http.StatusOK, `{"executions":[]}`), nil
					}
				})}
				h := newPipelineOwnershipHandler(client)
				h.cloudRunJobURL = "https://run.test/run"
				return invokeHandler(h.PipelineRun, http.MethodPost, "/api/v1/pipeline/run", map[string]string{"userID": "tenant-secret", "projectID": "project-secret"})
			},
			wantStatus: http.StatusInternalServerError,
			wantBody:   `{"error":"pipeline unavailable"}`,
		},
		{
			name: "pipeline status provider failure",
			invoke: func() *httptest.ResponseRecorder {
				client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					if r.URL.Path == "/token" {
						return testHTTPResponse(http.StatusOK, `{"access_token":"test-token"}`), nil
					}
					return testHTTPResponse(http.StatusBadGateway, securitySentinel), nil
				})}
				h := newPipelineOwnershipHandler(client)
				return invokeHandler(h.PipelineStatus, http.MethodGet, "/api/v1/pipeline/status", map[string]string{"userID": "tenant-secret", "projectID": "project-secret"})
			},
			wantStatus: http.StatusInternalServerError,
			wantBody:   `{"error":"pipeline status unavailable"}`,
		},
		{
			name: "pipeline log execution lookup",
			invoke: func() *httptest.ResponseRecorder {
				client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					if r.URL.Path == "/token" {
						return testHTTPResponse(http.StatusOK, `{"access_token":"test-token"}`), nil
					}
					return testHTTPResponse(http.StatusBadGateway, securitySentinel), nil
				})}
				h := newPipelineOwnershipHandler(client)
				return invokeHandler(h.PipelineLog, http.MethodGet, "/api/v1/pipeline/log?execution_id=execution-secret", map[string]string{"userID": "tenant-secret", "projectID": "project-secret"})
			},
			wantStatus: http.StatusInternalServerError,
			wantBody:   `{"error":"pipeline execution unavailable"}`,
		},
		{
			name: "rebuild failure",
			invoke: func() *httptest.ResponseRecorder {
				h := &Handler{rebuildIndex: func(context.Context, string, string) (idMap, error) { return idMap{}, errors.New(securitySentinel) }}
				return invokeHandler(h.RebuildIndex, http.MethodPost, "/api/v1/pipeline/rebuild-index", map[string]string{"userID": "tenant-secret", "projectID": "project-secret"})
			},
			wantStatus: http.StatusInternalServerError,
			wantBody:   `{"error":"generated data unavailable"}`,
		},
		{
			name: "admin rebuild failure",
			invoke: func() *httptest.ResponseRecorder {
				h := &Handler{rebuildIndex: func(context.Context, string, string) (idMap, error) { return idMap{}, errors.New(securitySentinel) }}
				return invokeHandlerWithParams(h.AdminRebuildIndex, http.MethodPost, "/api/v1/admin/projects/tenant-secret_project-secret/rebuild-index", ginParams("id", "tenant-secret_project-secret"))
			},
			wantStatus: http.StatusInternalServerError,
			wantBody:   `{"error":"generated data unavailable"}`,
		},
		{
			name: "admin pipeline provider failure",
			invoke: func() *httptest.ResponseRecorder {
				h := &Handler{projectExists: func(context.Context, string) error { return errors.New(securitySentinel) }}
				return invokeHandlerWithParams(h.AdminPipelineTrigger, http.MethodPost, "/api/v1/admin/projects/tenant-secret_project-secret/pipeline", ginParams("id", "tenant-secret_project-secret"))
			},
			wantStatus: http.StatusInternalServerError,
			wantBody:   `{"error":"pipeline unavailable"}`,
		},
		{
			name: "admin generation statistics failure",
			invoke: func() *httptest.ResponseRecorder {
				h := &Handler{
					adminProjectRecordsLoader: func(context.Context) ([]adminProjectRecord, error) {
						return []adminProjectRecord{{userID: "tenant-secret", projectID: "project-secret"}}, nil
					},
					adminProjectStatisticsLoader: func(context.Context, store.RootStore, []adminProjectRecord) (map[string]adminProjectStatistics, error) {
						return nil, errors.New(securitySentinel)
					},
				}
				return invokeHandler(h.AdminProjects, http.MethodGet, "/api/v1/admin/projects", nil)
			},
			wantStatus: http.StatusInternalServerError,
			wantBody:   `{"error":"project statistics unavailable"}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := tt.invoke()
			if recorder.Code != tt.wantStatus || recorder.Body.String() != tt.wantBody {
				t.Fatalf("status=%d body=%q, want status=%d body=%q", recorder.Code, recorder.Body.String(), tt.wantStatus, tt.wantBody)
			}
			assertSecuritySentinelsAbsent(t, recorder.Body.String())
		})
	}
}

func TestAdminPipelineTriggerLogIsSanitized(t *testing.T) {
	var output bytes.Buffer
	previousWriter, previousFlags := log.Writer(), log.Flags()
	log.SetOutput(&output)
	log.SetFlags(0)
	defer func() {
		log.SetOutput(previousWriter)
		log.SetFlags(previousFlags)
	}()

	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/token":
			return testHTTPResponse(http.StatusOK, `{"access_token":"test-token"}`), nil
		case "/run":
			return testHTTPResponse(http.StatusOK, `{"metadata":{"execution":"projects/p/locations/l/jobs/j/executions/execution-secret"}}`), nil
		default:
			return testHTTPResponse(http.StatusOK, `{"executions":[]}`), nil
		}
	})}
	h := &Handler{
		httpClient:       client,
		metadataTokenURL: "http://metadata.test/token",
		cloudRunJobURL:   "https://run.googleapis.com/v2/projects/p/locations/l/jobs/j:run",
		projectExists:    func(context.Context, string) error { return nil },
	}
	h.cloudRunJobURL = "https://run.test/run"
	recorder := invokeHandlerWithParams(h.AdminPipelineTrigger, http.MethodPost, "/api/v1/admin/projects/tenant-secret_project-secret/pipeline", ginParams("id", "tenant-secret_project-secret"))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if got := output.String(); got != "admin pipeline triggered\n" {
		t.Fatalf("log=%q, want fixed event", got)
	}
	assertSecuritySentinelsAbsent(t, output.String())
}

func invokeHandler(handlerFunc func(*gin.Context), method, target string, values map[string]string) *httptest.ResponseRecorder {
	return invokeHandlerWithParams(handlerFunc, method, target, nil, values)
}

func invokeHandlerWithParams(handlerFunc func(*gin.Context), method, target string, params gin.Params, values ...map[string]string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(method, target, nil)
	c.Params = params
	if len(values) > 0 {
		for key, value := range values[0] {
			c.Set(key, value)
		}
	}
	handlerFunc(c)
	return recorder
}

func ginParams(key, value string) gin.Params {
	return gin.Params{{Key: key, Value: value}}
}

func assertSecuritySentinelsAbsent(t *testing.T, value string) {
	t.Helper()
	for _, sentinel := range []string{"tenant-secret", "project-secret", "execution-secret", ".lwc/publish/generations/generation-secret", "https://provider.example/secret", "object-path-secret"} {
		if strings.Contains(value, sentinel) {
			t.Fatalf("security sentinel %q leaked in %q", sentinel, value)
		}
	}
}
