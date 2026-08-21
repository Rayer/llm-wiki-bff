package main

import (
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestDeployBFFWorkflowUsesOnlyImmutableStageConfig(t *testing.T) {
	data, err := os.ReadFile("../../.github/workflows/deploy-bff.yml")
	if err != nil {
		t.Fatal(err)
	}
	workflow := string(data)
	var document map[string]any
	if err := yaml.Unmarshal(data, &document); err != nil {
		t.Fatalf("workflow YAML: %v", err)
	}
	if len(document) == 0 {
		t.Fatal("workflow YAML is empty")
	}

	for _, declaration := range []string{
		"QUERY_STAGE_CONFIG_PATH: /app/configs/query/dev/query-dev-2026-08-21.1.json",
		"QUERY_STAGE_CONFIG_REVISION: query-dev-2026-08-21.1",
		"QUERY_STAGE_CONFIG_DIGEST: sha256:a35955fe4a451c740e6252cae8087f114fbac6b4162245d3de7818c1ad37a5c6",
	} {
		if strings.Count(workflow, declaration) != 1 {
			t.Fatalf("workflow declaration %q count = %d", declaration, strings.Count(workflow, declaration))
		}
	}

	gate := strings.Index(workflow, "- name: Verify immutable DEV query stage artifact")
	auth := strings.Index(workflow, "- name: Authenticate to Google Cloud")
	if gate < 0 || auth < 0 || gate >= auth {
		t.Fatal("immutable artifact gate must precede cloud authentication")
	}
	gateBlock := workflow[gate:auth]
	for _, required := range []string{"cmd/query_config_check", "QUERY_STAGE_CONFIG_REVISION", "QUERY_STAGE_CONFIG_DIGEST", "QUERY_STAGE_CONFIG_PATH"} {
		if !strings.Contains(gateBlock, required) {
			t.Fatalf("artifact gate missing %q", required)
		}
	}
	tests := strings.Index(workflow, "- name: Run Go tests")
	if tests < 0 || tests >= gate {
		t.Fatal("Run Go tests step is missing or out of order")
	}
	testsBlock := workflow[tests:gate]
	if !strings.Contains(testsBlock, "QUERY_STAGE_CONFIG_PATH: \"\"") {
		t.Fatal("Run Go tests step must clear deployment query stage config path")
	}

	deploy := strings.Index(workflow, "gcloud run deploy")
	quiet := strings.Index(workflow[deploy:], "--quiet")
	if deploy < 0 || quiet < 0 {
		t.Fatal("deploy command is missing")
	}
	deployCommand := workflow[deploy : deploy+quiet]
	update := strings.Index(deployCommand, "--update-env-vars")
	remove := strings.Index(deployCommand, "--remove-env-vars")
	if remove < 0 || update < 0 || remove >= update {
		t.Fatal("deploy command must remove stale vars before updating env vars")
	}
	removeBlock := deployCommand[remove:update]
	updateBlock := deployCommand[update:]
	legacy := []string{
		"QUERY_EXPANSION_MODEL", "QUERY_EXPANSION_REASONING", "ANSWER_SYNTHESIS_MODEL", "ANSWER_SYNTHESIS_REASONING",
		"QUERY_SELECTION_LIMIT", "QUERY_SELECTION_EXPLORATION_SLOTS", "QUERY_SELECTION_EVIDENCE_THRESHOLD",
		"QUERY_EXPANSION_KEYWORDS_PER_ATTEMPT", "QUERY_EXPANSION_ATTEMPTS", "QUERY_MATCHING_RARE_KEYWORD_MAX_DOCUMENT_FREQUENCY",
	}
	for _, name := range legacy {
		if !strings.Contains(removeBlock, name) || strings.Contains(updateBlock, name) || strings.Count(workflow, name) != 1 {
			t.Fatalf("legacy env %q is not removed-only in deploy command", name)
		}
	}
	if !strings.Contains(updateBlock, "QUERY_STAGE_CONFIG_PATH=${{ env.QUERY_STAGE_CONFIG_PATH }}") {
		t.Fatal("deploy command does not set the immutable stage config path")
	}

	cutover := strings.Index(workflow, "CUTOVER_VERIFIED=1")
	readback := strings.Index(workflow, "- name: Verify deployed query config readback")
	persist := strings.Index(workflow, "- name: Persist build image digest")
	rollback := strings.Index(workflow, "- name: Restore BFF traffic on post-cutover failure")
	if cutover < 0 || readback <= cutover || persist <= readback || rollback <= persist || strings.Count(workflow, "Restore BFF traffic on post-cutover failure") != 1 {
		t.Fatal("readback/rollback workflow order is unsafe")
	}
	for _, required := range []string{"QUERY_STAGE_CONFIG_REVISION", "QUERY_STAGE_CONFIG_DIGEST", "config_revision=", "config_digest="} {
		if !strings.Contains(workflow[readback:persist], required) {
			t.Fatalf("readback step missing %q", required)
		}
	}
}
