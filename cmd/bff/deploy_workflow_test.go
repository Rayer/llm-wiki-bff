package main

import (
	"encoding/json"
	"os"
	"os/exec"
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
		"QUERY_STAGE_CONFIG_PATH: /app/configs/query/dev/query-dev-2026-08-21.2.json",
		"QUERY_STAGE_CONFIG_REVISION: query-dev-2026-08-21.2",
		"QUERY_STAGE_CONFIG_DIGEST: sha256:75e4f76de991b496c503b42fd893d34408ddae726fe99003365a5c89b8e46642",
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

func TestReleaseBFFWorkflowPromotesImmutableStageConfig(t *testing.T) {
	data, err := os.ReadFile("../../.github/workflows/release-bff.yml")
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
		"QUERY_STAGE_CONFIG_PATH: /app/configs/query/dev/query-dev-2026-08-21.2.json",
		"QUERY_STAGE_CONFIG_REVISION: query-dev-2026-08-21.2",
		"QUERY_STAGE_CONFIG_DIGEST: sha256:75e4f76de991b496c503b42fd893d34408ddae726fe99003365a5c89b8e46642",
	} {
		if strings.Count(workflow, declaration) != 1 {
			t.Fatalf("production workflow declaration %q count = %d", declaration, strings.Count(workflow, declaration))
		}
	}

	deploy := strings.Index(workflow, "gcloud run deploy")
	quiet := strings.Index(workflow[deploy:], "--quiet")
	if deploy < 0 || quiet < 0 {
		t.Fatal("production deploy command is missing")
	}
	deployCommand := workflow[deploy : deploy+quiet]
	remove := strings.Index(deployCommand, "--remove-env-vars")
	update := strings.Index(deployCommand, "--update-env-vars")
	if remove < 0 || update < 0 || remove >= update {
		t.Fatal("production deploy must remove stale vars before updating env vars")
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
			t.Fatalf("production legacy env %q is not removed-only", name)
		}
	}
	if !strings.Contains(updateBlock, "QUERY_STAGE_CONFIG_PATH=${QUERY_STAGE_CONFIG_PATH}") {
		t.Fatal("production deploy does not set the immutable stage config path")
	}
	if strings.Contains(workflow[deploy:], "set-iam-policy") {
		t.Fatal("production query config wiring must not mutate IAM")
	}

	readback := strings.Index(workflow, "- name: Verify deployed query config readback")
	evidence := strings.Index(workflow, "- name: Render normalized deployment evidence after strict read-back")
	tag := strings.Index(workflow, "- name: Tag promoted production image")
	if readback <= deploy || evidence <= readback || tag <= evidence {
		t.Fatal("production query config readback/evidence/tag order is unsafe")
	}
	readbackBlock := workflow[readback:evidence]
	for _, required := range []string{
		"/api/v1/query/config", "Cache-Control", "no-store", "QUERY_STAGE_CONFIG_REVISION", "QUERY_STAGE_CONFIG_DIGEST",
		"query-retrieval-pipeline-v2", "deepseek-v4-flash", "deepseek-v4-pro", "project_id|generation_id",
		"EXPECTED_COMMIT", "latestReadyRevisionName", "build.commit", "build.revision", "build.service",
	} {
		if !strings.Contains(readbackBlock, required) {
			t.Fatalf("production query config readback missing %q", required)
		}
	}
}

func TestReleaseBFFWorkflowPollsExactCreatedRevisionAndRollsBackReadbackFailure(t *testing.T) {
	data, err := os.ReadFile("../../.github/workflows/release-bff.yml")
	if err != nil {
		t.Fatal(err)
	}
	workflow := string(data)

	deploy := strings.Index(workflow, "      - name: Deploy existing immutable image to Cloud Run")
	readback := strings.Index(workflow, "      - name: Verify deployed query config readback")
	rollback := strings.Index(workflow, "      - name: Restore frozen production traffic after query-config readback failure")
	if deploy < 0 || readback <= deploy || rollback <= readback {
		t.Fatal("release deploy, readback, and rollback steps are missing or out of order")
	}
	deployBlock := workflow[deploy:readback]
	readbackBlock := workflow[readback:rollback]
	rollbackBlock := workflow[rollback:]

	for _, required := range []string{
		"id: deploy",
		"gcloud run deploy",
		"deploy_started=true",
	} {
		if !strings.Contains(deployBlock, required) {
			t.Errorf("deploy step missing mutation-state contract %q", required)
		}
	}
	for _, required := range []string{
		`FROZEN_READY_REVISION=$(jq -er '.ready_revision | select(type == "string" and length > 0)' "$ROLLBACK_CONTRACT")`,
		"latestCreatedRevisionName",
		"latestReadyRevisionName",
		"READBACK_REVISION_DEADLINE",
		`FROZEN_CREATED_REVISION="${FROZEN_CREATED_REVISION:?}"`,
		`"$CANDIDATE_REVISION" != "$FROZEN_CREATED_REVISION"`,
		".status.latestCreatedRevisionName == $revision",
		".status.latestReadyRevisionName == $revision",
		"EXPECTED_REVISION=\"$CREATED_REVISION\"",
	} {
		if !strings.Contains(readbackBlock, required) {
			t.Errorf("readback step missing exact convergence contract %q", required)
		}
	}
	if strings.Contains(readbackBlock, "EXPECTED_REVISION=$(jq -er '.status.latestReadyRevisionName") {
		t.Fatal("readback must not select the ready revision before exact created-revision convergence")
	}
	validation := strings.Index(workflow, "      - name: Validate frozen rollback traffic before mutation")
	upload := strings.Index(workflow, "      - name: Upload immutable rollback contract")
	if validation < 0 || upload < 0 || validation <= upload || validation >= deploy {
		t.Fatal("frozen rollback traffic validation must follow durable upload and precede deploy")
	}
	for _, required := range []string{
		`FROZEN_READY_REVISION=$(jq -er '.ready_revision | select(type == "string" and length > 0)' "$ROLLBACK_CONTRACT")`,
		"validate_frozen_rollback_traffic()",
		"exactly one canonical rollback target",
		"revision_name == $ready_revision",
		"percent == 100",
		"tag",
		"if: ${{ always() && steps.deploy.outputs.deploy_started == 'true' && (steps.deploy.outcome == 'failure' || steps.query_config_readback.outcome == 'failure') }}",
		"SERVICE_JSON=$(gcloud run services describe",
		"validate_effective_traffic()",
		"live production traffic is already the frozen revision",
		"provider traffic readback is unavailable, unsupported, or ambiguous",
		"gcloud run services update-traffic",
		`--to-revisions "${FROZEN_READY_REVISION}=100"`,
		"latestRevision",
		"RESTORED_EFFECTIVE_REVISION",
		"RESTORED_EFFECTIVE_PERCENT",
		"exit 1",
		"ROLLBACK_READBACK_DEADLINE",
	} {
		if !strings.Contains(rollbackBlock, required) && !strings.Contains(workflow[validation:deploy], required) {
			t.Errorf("rollback step missing exact post-failure contract %q", required)
		}
	}
	if !strings.Contains(rollbackBlock, "keys | sort") || !strings.Contains(rollbackBlock, "latestRevision") {
		t.Fatal("rollback readback must reject unknown provider traffic fields and mutable latest metadata")
	}
	if strings.Contains(rollbackBlock, "ROLLBACK_TRAFFIC_JSON=$(jq -cer '.traffic'") || strings.Contains(rollbackBlock, "--to-latest") {
		t.Fatal("release rollback must use the frozen ready revision with explicit effective traffic")
	}

	marker := strings.Index(deployBlock, `echo "deploy_started=true" >> "$GITHUB_OUTPUT"`)
	command := strings.Index(deployBlock, "gcloud run deploy")
	if marker < 0 || command < 0 || marker > command {
		t.Fatal("deploy_started must be emitted immediately before the deploy command")
	}
	if strings.Contains(workflow, "deploy_attempted") {
		t.Fatal("obsolete deploy_attempted ledger must not remain")
	}

	readLive := strings.Index(rollbackBlock, `SERVICE_JSON=$(gcloud run services describe`)
	validateLive := strings.Index(rollbackBlock, `validate_effective_traffic "$SERVICE_JSON"`)
	mutate := strings.Index(rollbackBlock, "gcloud run services update-traffic")
	if readLive < 0 || validateLive < 0 || mutate < 0 || !(readLive < validateLive && validateLive < mutate) {
		t.Fatal("rollback must strictly read and validate live traffic before any write")
	}
	alreadyFrozen := strings.Index(rollbackBlock, "live production traffic is already the frozen revision")
	alreadyFrozenExit := strings.Index(rollbackBlock[alreadyFrozen:], "exit 1")
	if alreadyFrozen < 0 || alreadyFrozenExit < 0 || strings.Contains(rollbackBlock[alreadyFrozen:alreadyFrozen+alreadyFrozenExit], "update-traffic") {
		t.Fatal("already-frozen traffic path must perform zero writes and preserve failure")
	}
	unknown := strings.Index(rollbackBlock, "provider traffic readback is unavailable, unsupported, or ambiguous")
	unknownExit := strings.Index(rollbackBlock[unknown:], "exit 1")
	if unknown < 0 || unknownExit < 0 || strings.Contains(rollbackBlock[unknown:unknown+unknownExit], "update-traffic") {
		t.Fatal("unknown traffic readback must fail partial/unknown without guessing or writing")
	}
	if !strings.Contains(rollbackBlock, "live production traffic differs from the frozen revision; restoring") || !strings.Contains(rollbackBlock, "validate_restored_effective_traffic") {
		t.Fatal("changed traffic path must restore and verify exact frozen effective routing")
	}
}

func TestReleaseBFFPreMutationTrafficGuard(t *testing.T) {
	data, err := os.ReadFile("../../.github/workflows/release-bff.yml")
	if err != nil {
		t.Fatal(err)
	}
	workflow := string(data)
	start := strings.Index(workflow, "      - name: Validate frozen rollback traffic before mutation")
	end := strings.Index(workflow[start:], "      - name: Deploy existing immutable image to Cloud Run")
	if start < 0 || end < 0 {
		t.Fatal("pre-mutation validation block is missing")
	}
	preflight := workflow[start : start+end]
	filter := `
      .status.latestCreatedRevisionName == $ready_revision
      and .status.latestReadyRevisionName == $ready_revision
      and (.status.traffic | type == "array" and length == 1)
      and (.status.traffic[0] | type == "object")
      and ((.status.traffic[0] | keys | sort) == ["latestRevision", "percent", "revisionName"] or
           (.status.traffic[0] | keys | sort) == ["percent", "revisionName"])
      and .status.traffic[0].revisionName == $ready_revision
      and .status.traffic[0].percent == 100
      and ((.status.traffic[0] | has("latestRevision") | not) or
           (.status.traffic[0].latestRevision | type == "boolean" and . == true))
      and (.status.traffic[0].tag? == null)
    `
	if !strings.Contains(preflight, `(.status.traffic[0] | has("latestRevision") | not) or`) {
		t.Fatal("pre-mutation live traffic must require absent or boolean-true latestRevision")
	}
	if strings.Contains(preflight, `((.status.traffic[0].latestRevision? // false) == false)`) {
		t.Fatal("pre-mutation live traffic must not require latestRevision false")
	}

	rollback := workflow[strings.Index(workflow, "      - name: Restore frozen production traffic after query-config readback failure"):]
	for _, validator := range []string{"validate_restored_effective_traffic()", "validate_effective_traffic()"} {
		if !strings.Contains(rollback, validator) {
			t.Fatalf("post-failure validator %q must remain present", validator)
		}
	}
	if !strings.Contains(rollback, `((.status.traffic[0].latestRevision? // false) == false)`) {
		t.Fatal("post-failure validation must continue requiring explicit effective traffic")
	}
	if !strings.Contains(rollback, `(.status.traffic[0] | keys | sort) == ["percent", "revisionName"]`) {
		t.Fatal("post-failure validation must retain explicit old-route keys")
	}

	base := map[string]any{"status": map[string]any{
		"latestCreatedRevisionName": "rev-1", "latestReadyRevisionName": "rev-1",
		"traffic": []any{map[string]any{"revisionName": "rev-1", "percent": 100}},
	}}
	cases := []struct {
		name   string
		mutate func(map[string]any)
		want   bool
	}{
		{"absent", func(map[string]any) {}, true},
		{"true", func(m map[string]any) {
			m["status"].(map[string]any)["traffic"].([]any)[0].(map[string]any)["latestRevision"] = true
		}, true},
		{"null", func(m map[string]any) {
			m["status"].(map[string]any)["traffic"].([]any)[0].(map[string]any)["latestRevision"] = nil
		}, false},
		{"false", func(m map[string]any) {
			m["status"].(map[string]any)["traffic"].([]any)[0].(map[string]any)["latestRevision"] = false
		}, false},
		{"string", func(m map[string]any) {
			m["status"].(map[string]any)["traffic"].([]any)[0].(map[string]any)["latestRevision"] = "true"
		}, false},
		{"number", func(m map[string]any) {
			m["status"].(map[string]any)["traffic"].([]any)[0].(map[string]any)["latestRevision"] = 1
		}, false},
		{"object", func(m map[string]any) {
			x := m["status"].(map[string]any)["traffic"].([]any)[0].(map[string]any)
			x["latestRevision"] = map[string]any{}
		}, false},
		{"array", func(m map[string]any) {
			m["status"].(map[string]any)["traffic"].([]any)[0].(map[string]any)["latestRevision"] = []any{}
		}, false},
		{"missing revisionName", func(m map[string]any) {
			delete(m["status"].(map[string]any)["traffic"].([]any)[0].(map[string]any), "revisionName")
		}, false},
		{"unknown key", func(m map[string]any) {
			m["status"].(map[string]any)["traffic"].([]any)[0].(map[string]any)["extra"] = true
		}, false},
		{"tag", func(m map[string]any) {
			m["status"].(map[string]any)["traffic"].([]any)[0].(map[string]any)["tag"] = "stable"
		}, false},
		{"split traffic", func(m map[string]any) {
			m["status"].(map[string]any)["traffic"] = []any{m["status"].(map[string]any)["traffic"].([]any)[0], map[string]any{"revisionName": "rev-2", "percent": 0}}
		}, false},
		{"wrong revision", func(m map[string]any) {
			m["status"].(map[string]any)["traffic"].([]any)[0].(map[string]any)["revisionName"] = "rev-2"
		}, false},
		{"wrong percent", func(m map[string]any) {
			m["status"].(map[string]any)["traffic"].([]any)[0].(map[string]any)["percent"] = 99
		}, false},
		{"latestCreated mismatch", func(m map[string]any) { m["status"].(map[string]any)["latestCreatedRevisionName"] = "rev-2" }, false},
		{"latestReady mismatch", func(m map[string]any) { m["status"].(map[string]any)["latestReadyRevisionName"] = "rev-2" }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			value := jsonClone(base)
			tc.mutate(value)
			encoded, _ := json.Marshal(value)
			cmd := exec.Command("jq", "-e", "--arg", "ready_revision", "rev-1", filter)
			cmd.Stdin = strings.NewReader(string(encoded))
			err := cmd.Run()
			if (err == nil) != tc.want {
				t.Fatalf("jq result error=%v, want pass=%v", err, tc.want)
			}
		})
	}
}

func jsonClone(value map[string]any) map[string]any {
	encoded, _ := json.Marshal(value)
	var clone map[string]any
	_ = json.Unmarshal(encoded, &clone)
	return clone
}
