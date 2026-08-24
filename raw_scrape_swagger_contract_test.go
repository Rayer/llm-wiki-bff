package main

import (
	"encoding/json"
	"os"
	"sort"
	"testing"

	docs "github.com/rayer/llm-wiki-bff/docs"
	"gopkg.in/yaml.v3"
)

type swaggerArtifact struct {
	Paths       map[string]map[string]any `json:"paths" yaml:"paths"`
	Definitions map[string]struct {
		Properties map[string]any `json:"properties" yaml:"properties"`
	} `json:"definitions" yaml:"definitions"`
}

type rawScrapeOperation struct {
	Security   []map[string][]string `json:"security" yaml:"security"`
	Consumes   []string              `json:"consumes" yaml:"consumes"`
	Produces   []string              `json:"produces" yaml:"produces"`
	Parameters []struct {
		In       string `json:"in" yaml:"in"`
		Name     string `json:"name" yaml:"name"`
		Required bool   `json:"required" yaml:"required"`
		Schema   struct {
			Ref string `json:"$ref" yaml:"$ref"`
		} `json:"schema" yaml:"schema"`
	} `json:"parameters" yaml:"parameters"`
	Responses map[string]struct {
		Schema struct {
			Ref string `json:"$ref" yaml:"$ref"`
		} `json:"schema" yaml:"schema"`
	} `json:"responses" yaml:"responses"`
}

type adminPipelineOperation struct {
	Summary     string `json:"summary" yaml:"summary"`
	Description string `json:"description" yaml:"description"`
	Parameters  []struct {
		In       string `json:"in" yaml:"in"`
		Name     string `json:"name" yaml:"name"`
		Required bool   `json:"required" yaml:"required"`
		Schema   struct {
			Ref string `json:"$ref" yaml:"$ref"`
		} `json:"schema" yaml:"schema"`
	} `json:"parameters" yaml:"parameters"`
	Responses map[string]struct {
		Schema struct {
			Ref string `json:"$ref" yaml:"$ref"`
		} `json:"schema" yaml:"schema"`
	} `json:"responses" yaml:"responses"`
}

func TestSwaggerRawScrapeRouteContract(t *testing.T) {
	t.Run("json", func(t *testing.T) {
		d := readSwaggerJSON(t, "docs/swagger.json")
		assertRawScrapeRouteContract(t, d)
	})

	t.Run("yaml", func(t *testing.T) {
		d := readSwaggerYAML(t, "docs/swagger.yaml")
		assertRawScrapeRouteContract(t, d)
	})

	t.Run("docs", func(t *testing.T) {
		d := readSwaggerFromDocsTemplate(t)
		assertRawScrapeRouteContract(t, d)
	})
}

func TestSwaggerAdminPipelineContract_NoUnrelatedDrift(t *testing.T) {
	t.Run("json", func(t *testing.T) {
		d := readSwaggerJSON(t, "docs/swagger.json")
		assertAdminPipelineOperation(t, d, "json")
	})

	t.Run("yaml", func(t *testing.T) {
		d := readSwaggerYAML(t, "docs/swagger.yaml")
		assertAdminPipelineOperation(t, d, "yaml")
	})

	t.Run("docs", func(t *testing.T) {
		d := readSwaggerFromDocsTemplate(t)
		assertAdminPipelineOperation(t, d, "docs")
	})
}

func TestSwaggerRawScrapeRouteContractConsistency(t *testing.T) {
	jsonArtifact := readSwaggerJSON(t, "docs/swagger.json")
	yamlArtifact := readSwaggerYAML(t, "docs/swagger.yaml")
	docArtifact := readSwaggerFromDocsTemplate(t)

	jsonOp := rawScrapeOperationForPath(t, jsonArtifact, "json")
	yamlOp := rawScrapeOperationForPath(t, yamlArtifact, "yaml")
	docOp := rawScrapeOperationForPath(t, docArtifact, "docs")

	if jsonOp.Security == nil || yamlOp.Security == nil || docOp.Security == nil {
		t.Fatal("expected security declarations in all artifacts")
	}
	assertExactRawSecurity(t, jsonOp.Security, "json")
	assertExactRawSecurity(t, yamlOp.Security, "yaml")
	assertExactRawSecurity(t, docOp.Security, "docs")

	if len(jsonOp.Responses) != len(yamlOp.Responses) || len(jsonOp.Responses) != len(docOp.Responses) {
		t.Fatalf("response count drift: json=%d yaml=%d docs=%d", len(jsonOp.Responses), len(yamlOp.Responses), len(docOp.Responses))
	}
	if jsonOp.Responses["200"].Schema.Ref != "#/definitions/handler.ScrapeResponse" ||
		yamlOp.Responses["200"].Schema.Ref != "#/definitions/handler.ScrapeResponse" ||
		docOp.Responses["200"].Schema.Ref != "#/definitions/handler.ScrapeResponse" {
		t.Fatalf("inconsistent success response schema: json=%q yaml=%q docs=%q", jsonOp.Responses["200"].Schema.Ref, yamlOp.Responses["200"].Schema.Ref, docOp.Responses["200"].Schema.Ref)
	}

	wantBody := "#/definitions/handler.ScrapeRequest"
	if got := rawScrapeRequestBodyRef(jsonOp); got != wantBody {
		t.Fatalf("json body ref = %q, want %q", got, wantBody)
	}
	if got := rawScrapeRequestBodyRef(yamlOp); got != wantBody {
		t.Fatalf("yaml body ref = %q, want %q", got, wantBody)
	}
	if got := rawScrapeRequestBodyRef(docOp); got != wantBody {
		t.Fatalf("docs body ref = %q, want %q", got, wantBody)
	}
}

func TestQueryRequestContract_NoProjectFieldAcrossSwaggerArtifacts(t *testing.T) {
	t.Run("json", func(t *testing.T) {
		d := readSwaggerJSON(t, "docs/swagger.json")
		assertQueryRequestFields(t, d)
	})
	t.Run("yaml", func(t *testing.T) {
		d := readSwaggerYAML(t, "docs/swagger.yaml")
		assertQueryRequestFields(t, d)
	})
	t.Run("docs", func(t *testing.T) {
		d := readSwaggerFromDocsTemplate(t)
		assertQueryRequestFields(t, d)
	})
}

func assertRawScrapeRouteContract(t *testing.T, artifact swaggerArtifact) {
	operations, ok := artifact.Paths["/api/v1/raw/scrape"]
	if !ok {
		t.Fatal("swagger contract missing POST route /api/v1/raw/scrape")
	}
	if len(operations) != 1 {
		t.Fatalf("route /api/v1/raw/scrape has unexpected methods: %v", sortedKeys(operations))
	}
	post, ok := operations["post"]
	if !ok {
		t.Fatalf("route /api/v1/raw/scrape has unexpected methods: %v", sortedKeys(operations))
	}
	postBytes, err := json.Marshal(post)
	if err != nil {
		t.Fatalf("marshal POST operation for /api/v1/raw/scrape: %v", err)
	}
	var op rawScrapeOperation
	if err := json.Unmarshal(postBytes, &op); err != nil {
		t.Fatalf("decode raw scrape operation: %v", err)
	}

	bodyRef, ok := rawScrapeBodyRef(op)
	if !ok {
		t.Fatal("missing required body parameter for /api/v1/raw/scrape")
	}
	if bodyRef != "#/definitions/handler.ScrapeRequest" {
		t.Fatalf("body schema = %q, want %q", bodyRef, "#/definitions/handler.ScrapeRequest")
	}

	if got := op.Responses["200"].Schema.Ref; got != "#/definitions/handler.ScrapeResponse" {
		t.Fatalf("200 response schema = %q, want %q", got, "#/definitions/handler.ScrapeResponse")
	}
	for _, status := range []string{"400", "401", "500", "503"} {
		if got := op.Responses[status].Schema.Ref; got != "#/definitions/handler.ErrorResponse" {
			t.Fatalf("%s response schema = %q, want %q", status, got, "#/definitions/handler.ErrorResponse")
		}
	}

	requiresJSON(op.Consumes, "application/json", t)
	requiresJSON(op.Produces, "application/json", t)
	assertExactRawSecurity(t, op.Security, "route contract")
}

func assertQueryRequestFields(t *testing.T, artifact swaggerArtifact) {
	definition, ok := artifact.Definitions["handler.QueryRequest"]
	if !ok {
		t.Fatal("swagger definition missing handler.QueryRequest")
	}
	if _, ok := definition.Properties["project"]; ok {
		t.Fatal(`swagger definition handler.QueryRequest exposes forbidden field "project"`)
	}

	keys := make([]string, 0, len(definition.Properties))
	for key := range definition.Properties {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if got := len(keys); got != 2 {
		t.Fatalf("handler.QueryRequest has %d properties, want 2 (q, mode)", got)
	}
	if keys[0] != "mode" || keys[1] != "q" {
		t.Fatalf("handler.QueryRequest properties = %q, want [mode q]", keys)
	}
}

func rawScrapeOperationForPath(t *testing.T, artifact swaggerArtifact, artifactName string) rawScrapeOperation {
	t.Helper()
	raw, ok := artifact.Paths["/api/v1/raw/scrape"]
	if !ok {
		t.Fatalf("artifact %s missing /api/v1/raw/scrape path", artifactName)
	}
	post, ok := raw["post"]
	if !ok {
		t.Fatalf("artifact %s missing POST for /api/v1/raw/scrape", artifactName)
	}
	postBytes, err := json.Marshal(post)
	if err != nil {
		t.Fatalf("artifact %s marshal POST operation: %v", artifactName, err)
	}
	var op rawScrapeOperation
	if err := json.Unmarshal(postBytes, &op); err != nil {
		t.Fatalf("artifact %s decode raw scrape operation: %v", artifactName, err)
	}
	if _, ok := rawScrapeBodyRef(op); !ok {
		t.Fatalf("artifact %s missing required body ref for /api/v1/raw/scrape", artifactName)
	}
	return op
}

func assertAdminPipelineOperation(t *testing.T, artifact swaggerArtifact, artifactName string) {
	t.Helper()
	const (
		wantSummary     = "Trigger pipeline for a project (admin)"
		wantDescription = "Invokes the Cloud Run worker job for the specified project. Optional clean_rebuild/stage overrides may be provided in the JSON request body."
	)

	admin, ok := artifact.Paths["/api/v1/admin/projects/{id}/pipeline"]
	if !ok {
		t.Fatalf("artifact %s missing /api/v1/admin/projects/{id}/pipeline path", artifactName)
	}
	post, ok := admin["post"]
	if !ok {
		t.Fatalf("artifact %s missing POST /api/v1/admin/projects/{id}/pipeline", artifactName)
	}

	postBytes, err := json.Marshal(post)
	if err != nil {
		t.Fatalf("artifact %s marshal admin pipeline operation: %v", artifactName, err)
	}
	var op adminPipelineOperation
	if err := json.Unmarshal(postBytes, &op); err != nil {
		t.Fatalf("artifact %s decode admin pipeline operation: %v", artifactName, err)
	}

	if op.Summary != wantSummary {
		t.Fatalf("artifact %s admin pipeline summary = %q, want %q", artifactName, op.Summary, wantSummary)
	}
	if op.Description != wantDescription {
		t.Fatalf("artifact %s admin pipeline description = %q, want %q", artifactName, op.Description, wantDescription)
	}
	if len(op.Parameters) != 2 {
		t.Fatalf("artifact %s admin pipeline parameters = %d, want path plus optional body", artifactName, len(op.Parameters))
	}
	if op.Parameters[0].In != "path" || op.Parameters[0].Name != "id" || !op.Parameters[0].Required {
		t.Fatalf("artifact %s admin pipeline path parameter = {in=%q name=%q required=%v}, want {in=path name=id required=true}", artifactName, op.Parameters[0].In, op.Parameters[0].Name, op.Parameters[0].Required)
	}
	if op.Parameters[1].In != "body" || op.Parameters[1].Name != "body" || op.Parameters[1].Required || op.Parameters[1].Schema.Ref != "#/definitions/v1.adminPipelineTriggerRequest" {
		t.Fatalf("artifact %s admin pipeline body parameter = %+v, want optional v1.adminPipelineTriggerRequest", artifactName, op.Parameters[1])
	}
	if op.Responses["202"].Schema.Ref != "#/definitions/v1.adminPipelineTriggerResponse" {
		t.Fatalf("artifact %s admin pipeline 202 schema = %q, want named response", artifactName, op.Responses["202"].Schema.Ref)
	}
	request := artifact.Definitions["v1.adminPipelineTriggerRequest"].Properties
	for _, field := range []string{"stage", "clean_rebuild"} {
		if _, ok := request[field]; !ok {
			t.Fatalf("artifact %s trigger request is missing %q", artifactName, field)
		}
	}
	response := artifact.Definitions["v1.adminPipelineTriggerResponse"].Properties
	for _, field := range []string{"status", "execution_id", "stage", "clean_rebuild"} {
		if _, ok := response[field]; !ok {
			t.Fatalf("artifact %s trigger response is missing %q", artifactName, field)
		}
	}
}

func TestSwaggerAdminPipelineStatusContract(t *testing.T) {
	for _, artifactName := range []string{"json", "yaml", "docs"} {
		t.Run(artifactName, func(t *testing.T) {
			var artifact swaggerArtifact
			switch artifactName {
			case "json":
				artifact = readSwaggerJSON(t, "docs/swagger.json")
			case "yaml":
				artifact = readSwaggerYAML(t, "docs/swagger.yaml")
			default:
				artifact = readSwaggerFromDocsTemplate(t)
			}
			get := artifact.Paths["/api/v1/admin/projects/{id}/pipeline/status"]["get"]
			bytes, err := json.Marshal(get)
			if err != nil {
				t.Fatal(err)
			}
			var op adminPipelineOperation
			if err := json.Unmarshal(bytes, &op); err != nil {
				t.Fatal(err)
			}
			if op.Responses["200"].Schema.Ref != "#/definitions/v1.adminPipelineStatusResponse" {
				t.Fatalf("200 schema = %q, want named status response", op.Responses["200"].Schema.Ref)
			}
			response := artifact.Definitions["v1.adminPipelineStatusResponse"].Properties
			for _, field := range []string{"project_id", "last_execution"} {
				if _, ok := response[field]; !ok {
					t.Fatalf("artifact %s status response is missing %q", artifactName, field)
				}
			}
		})
	}
}

func rawScrapeBodyRef(op rawScrapeOperation) (string, bool) {
	for _, param := range op.Parameters {
		if param.In == "body" && param.Required {
			return param.Schema.Ref, true
		}
	}
	return "", false
}

func rawScrapeRequestBodyRef(op rawScrapeOperation) string {
	for _, param := range op.Parameters {
		if param.In == "body" {
			if param.Schema.Ref != "" {
				return param.Schema.Ref
			}
		}
	}
	return ""
}

func requiresJSON(values []string, want string, t *testing.T) {
	t.Helper()
	for _, value := range values {
		if value == want {
			return
		}
	}
	t.Fatalf("expected %q, got %q", want, values)
}

func assertExactRawSecurity(t *testing.T, security []map[string][]string, artifactName string) {
	t.Helper()
	if len(security) != 1 {
		t.Fatalf("%s raw security = %v, want exactly one security requirement object", artifactName, security)
	}
	requirement := security[0]
	if _, ok := requirement["BearerAuth"]; !ok {
		t.Fatalf("%s raw security = %v, missing BearerAuth", artifactName, security)
	}
	if _, ok := requirement["ProjectHeader"]; !ok {
		t.Fatalf("%s raw security = %v, missing ProjectHeader", artifactName, security)
	}
	if len(requirement) != 2 {
		t.Fatalf("%s raw security = %v, expected exactly BearerAuth and ProjectHeader", artifactName, security)
	}
	if _, ok := requirement["DevUserAuth"]; ok {
		t.Fatalf("%s raw security = %v, should not include DevUserAuth", artifactName, security)
	}
	for key := range requirement {
		if key != "BearerAuth" && key != "ProjectHeader" {
			t.Fatalf("%s raw security = %v, unexpected key %q", artifactName, security, key)
		}
	}
}

func readSwaggerJSON(t *testing.T, path string) swaggerArtifact {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read Swagger JSON %s: %v", path, err)
	}
	var artifact swaggerArtifact
	if err := json.Unmarshal(data, &artifact); err != nil {
		t.Fatalf("decode Swagger JSON %s: %v", path, err)
	}
	return artifact
}

func readSwaggerYAML(t *testing.T, path string) swaggerArtifact {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read Swagger YAML %s: %v", path, err)
	}
	var artifact swaggerArtifact
	if err := yaml.Unmarshal(data, &artifact); err != nil {
		t.Fatalf("decode Swagger YAML %s: %v", path, err)
	}
	return artifact
}

func readSwaggerFromDocsTemplate(t *testing.T) swaggerArtifact {
	t.Helper()
	raw := docs.SwaggerInfo.ReadDoc()
	var artifact swaggerArtifact
	if err := json.Unmarshal([]byte(raw), &artifact); err != nil {
		t.Fatalf("decode docs.SwaggerInfo template output: %v", err)
	}
	return artifact
}

func sortedKeys[M any](m map[string]M) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
