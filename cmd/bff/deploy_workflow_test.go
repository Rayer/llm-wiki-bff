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
		"QUERY_STAGE_CONFIG_PATH: /app/configs/query/dev/query-dev-2026-08-22.1.json",
		"QUERY_STAGE_CONFIG_REVISION: query-dev-2026-08-22.1",
		"QUERY_STAGE_CONFIG_DIGEST: sha256:04dd36a4446043f225f651caaae1bf73c605106b99a23acbbc98cac09c6c4942",
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
	ci := strings.Index(workflow, "- name: Require exact canonical CI evidence")
	if ci < 0 || ci >= gate {
		t.Fatal("canonical CI evidence step is missing or out of order")
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
		"QUERY_STAGE_CONFIG_PATH: /app/configs/query/dev/query-dev-2026-08-22.1.json",
		"QUERY_STAGE_CONFIG_REVISION: query-dev-2026-08-22.1",
		"QUERY_STAGE_CONFIG_DIGEST: sha256:04dd36a4446043f225f651caaae1bf73c605106b99a23acbbc98cac09c6c4942",
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
	if readback <= deploy || evidence <= readback {
		t.Fatal("production query config readback/evidence order is unsafe")
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
		"scripts/validate_bff_promotion_contract.py validate-traffic",
		"--traffic-path status.traffic",
		"--recognized-revision \"$FROZEN_READY_REVISION\"",
		"if: ${{ always() && steps.deploy.outputs.deploy_started == 'true' && (steps.deploy.outcome == 'failure' || steps.query_config_readback.outcome == 'failure') }}",
		"SERVICE_JSON=$(gcloud run services describe",
		"validate_effective_traffic()",
		"live production traffic is already the frozen revision",
		"provider traffic readback is unavailable, unsupported, or ambiguous",
		"gcloud run services update-traffic",
		`--to-revisions "${FROZEN_READY_REVISION}=100"`,
		"RESTORED_EFFECTIVE_REVISION",
		"RESTORED_EFFECTIVE_PERCENT",
		"exit 1",
		"ROLLBACK_READBACK_DEADLINE",
	} {
		if !strings.Contains(rollbackBlock, required) && !strings.Contains(workflow[validation:deploy], required) {
			t.Errorf("rollback step missing exact post-failure contract %q", required)
		}
	}
	if strings.Contains(rollbackBlock, "keys | sort") {
		t.Fatal("rollback traffic semantics must not be duplicated in shell jq")
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
	for _, want := range []string{
		"scripts/validate_bff_promotion_contract.py validate-traffic",
		"--traffic-path status.traffic",
		"--recognized-revision \"$FROZEN_READY_REVISION\"",
	} {
		if !strings.Contains(preflight, want) {
			t.Fatalf("pre-mutation validation must use shared traffic contract %q", want)
		}
	}
	if strings.Contains(preflight, "gcloud run services update-traffic") || strings.Contains(preflight, "gcloud run deploy") {
		t.Fatal("pre-mutation validation must not mutate provider state")
	}

	rollback := workflow[strings.Index(workflow, "      - name: Restore frozen production traffic after query-config readback failure"):]
	for _, validator := range []string{"validate_restored_effective_traffic()", "validate_effective_traffic()"} {
		if !strings.Contains(rollback, validator) {
			t.Fatalf("post-failure validator %q must remain present", validator)
		}
	}
	for _, want := range []string{
		"scripts/validate_bff_promotion_contract.py validate-traffic",
		"--traffic-path status.traffic",
		"--recognized-revision \"$FROZEN_READY_REVISION\"",
	} {
		if !strings.Contains(rollback, want) {
			t.Fatalf("post-failure validation must use shared traffic contract %q", want)
		}
	}
}
