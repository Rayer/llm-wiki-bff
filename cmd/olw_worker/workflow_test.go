package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// This keeps the deployment boundary deterministic without requiring cloud
// credentials: the checked-in workflow must retain the fail-closed contract.
func TestDeployWorkerWorkflowContract(t *testing.T) {
	data, err := os.ReadFile("../../.github/workflows/deploy-worker.yml")
	if err != nil {
		t.Fatal(err)
	}
	workflow := string(data)
	for _, want := range []string{
		"branches: [develop/1.0, main]",
		"workflow_dispatch:",
		"group: deploy-olw-pipeline-dev",
		"cancel-in-progress: true",
		"actions/setup-go@v5",
		"go test ./... -count=1",
		"go vet ./...",
		"go build ./...",
		"${SHA}-${GITHUB_RUN_ID}-${GITHUB_RUN_ATTEMPT}",
		"git rev-parse \"origin/${SOURCE_BRANCH}\"",
		"--image \"$IMMUTABLE_IMAGE\"",
		"gcloud run jobs describe \"${JOB_NAME}\"",
		"BUCKET: llm-wiki-data-dev",
		"--update-env-vars \"BUCKET=${BUCKET}\"",
		"--remove-env-vars \"DATA_DIR,WORKSPACE,VAULT_PATH,WORKSPACE_DIR\"",
		"--args \"^@^run@[[\\\"run\\\",\\\"--auto-approve\\\"]]\"",
		"--clear-volume-mounts",
		"--clear-volumes",
		"worker must not retain GCSFuse volumes",
		"worker args do not match the cloud worker contract",
		"${DEPLOYED}\" != \"${IMMUTABLE_IMAGE}",
	} {
		if !strings.Contains(workflow, want) {
			t.Fatalf("workflow missing %q", want)
		}
	}
	if !strings.Contains(workflow, `select(.name == "DATA_DIR" or .name == "WORKSPACE" or .name == "VAULT_PATH" or .name == "WORKSPACE_DIR")`) {
		t.Fatal("workflow does not read back removed legacy env")
	}
	if strings.Index(workflow, "go test ./... -count=1") > strings.Index(workflow, "Authenticate to Google Cloud") {
		t.Fatal("source verification must run before cloud authentication")
	}
	if strings.Count(workflow, "git rev-parse \"origin/${SOURCE_BRANCH}\"") != 2 {
		t.Fatal("workflow must recheck the selected source branch before and after the job update")
	}
	if !strings.Contains(workflow, "newer serialized workflow must supersede") {
		t.Fatal("workflow must explain that a newer serialized workflow supersedes an out-of-order deployment")
	}
	if strings.Contains(workflow, "olw-pipeline-prod") {
		t.Fatal("dev worker workflow must not update production")
	}

	for _, line := range strings.Split(workflow, "\n") {
		if line == "env:" {
			t.Fatal("application runtime/deploy env must not be workflow-global and contaminate source verification")
		}
	}
	verifyStep := workflowSection(t, workflow, "      - name: Verify worker source", "      - name: Authenticate to Google Cloud")
	for _, forbidden := range []string{"PROJECT_ID:", "REGION:", "JOB_NAME:", "BUCKET:", "AR_REPO:"} {
		if strings.Contains(verifyStep, forbidden) {
			t.Fatalf("source verification inherited application env %q", forbidden)
		}
	}
	authStep := workflowSection(t, workflow, "      - name: Authenticate to Google Cloud", "      - name: Set up Cloud SDK")
	setupStep := workflowSection(t, workflow, "      - name: Set up Cloud SDK", "      - name: Configure Artifact Registry Docker auth")
	for name, section := range map[string]string{"auth": authStep, "setup": setupStep} {
		if !strings.Contains(section, "PROJECT_ID: llm-wiki-cloud") {
			t.Fatalf("%s step missing scoped PROJECT_ID", name)
		}
	}
	buildStep := workflowSection(t, workflow, "      - name: Build and push immutable worker image", "      - name: Update dev worker without GCSFuse")
	if !strings.Contains(buildStep, "AR_REPO: asia-east1-docker.pkg.dev/llm-wiki-cloud/cloud-run-images") {
		t.Fatal("image build step must receive its explicit Artifact Registry repository")
	}
	updateStep := workflow[strings.Index(workflow, "      - name: Update dev worker without GCSFuse"):]
	clearMountsAt := strings.Index(updateStep, "--clear-volume-mounts")
	clearVolumesAt := strings.Index(updateStep, "--clear-volumes")
	if clearMountsAt < 0 || clearVolumesAt <= clearMountsAt {
		t.Fatal("worker update must clear volume mounts before clearing their referenced volumes")
	}
	for _, want := range []string{"REGION: asia-east1", "JOB_NAME: olw-pipeline-dev", "BUCKET: llm-wiki-data-dev"} {
		if !strings.Contains(updateStep, want) {
			t.Fatalf("worker update step missing scoped env %q", want)
		}
	}
}

func TestWorkerPromotionWorkflowsContract(t *testing.T) {
	deploy := readWorkflow(t, "../../.github/workflows/deploy-worker.yml")
	release := readWorkflow(t, "../../.github/workflows/release-worker.yml")

	for _, workflow := range map[string]string{"deploy": deploy, "release": release} {
		var document any
		if err := yaml.Unmarshal([]byte(workflow), &document); err != nil {
			t.Fatalf("%s workflow is not valid YAML: %v", workflow, err)
		}
		for _, run := range regexp.MustCompile(`(?m)^\s+run: \|\n((?:\s{10,}.+\n?)+)`).FindAllStringSubmatch(workflow, -1) {
			body := strings.TrimSpace(run[1])
			body = regexp.MustCompile(`(?m)^ {10}`).ReplaceAllString(body, "")
			body = strings.ReplaceAll(body, "${{", "${")
			body = strings.ReplaceAll(body, "}}", "}")
			cmd := exec.Command("bash", "-n")
			cmd.Stdin = strings.NewReader(body)
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("%s workflow run block has invalid shell syntax: %v\n%s", workflow, err, output)
			}
		}
	}

	for _, want := range []string{
		"SOURCE_BRANCH=",
		"case \"${GITHUB_REF_NAME}\" in",
		"develop/1.0|main)",
		"GITHUB_SHA",
		"worker-image-digest-${GITHUB_SHA}",
		"sha256:[0-9a-f]{64}",
		"immutable_image=${AR_REPO}/olw-pipeline@${DIGEST}",
		"actions/upload-artifact@v4",
		"path: worker-image-digest-${{ github.sha }}.txt",
	} {
		if !strings.Contains(deploy, want) {
			t.Fatalf("dev workflow missing %q", want)
		}
	}
	if strings.Count(deploy, "actions/upload-artifact@v4") != 1 || strings.Count(deploy, "worker-image-digest-${GITHUB_SHA}.txt") != 1 || strings.Count(deploy, "path: worker-image-digest-${{ github.sha }}.txt") != 1 {
		t.Fatal("dev workflow must persist and upload exactly one full-SHA digest artifact")
	}
	if strings.Contains(deploy, "origin/develop/1.0") {
		t.Fatal("dev workflow must not hardcode develop ordering for main runs")
	}

	for _, want := range []string{
		"commit_sha:",
		"environment: production",
		"contents: read",
		"actions: read",
		"id-token: write",
		"ref: main",
		"fetch-depth: 0",
		"git merge-base --is-ancestor",
		"event=push&status=completed&head_sha=${COMMIT_SHA}",
		".head_branch == \"main\"",
		"worker-image-digest-$COMMIT_SHA",
		"sha256:[0-9a-f]{64}",
		"olw-pipeline@${DIGEST}",
		"BUCKET: llm-wiki-data",
		"--remove-env-vars \"DATA_DIR,WORKSPACE,VAULT_PATH,WORKSPACE_DIR\"",
		"--clear-volume-mounts",
		"--clear-volumes",
		"scripts/render_worker_deployment_evidence.py",
		"prod-${COMMIT_SHA}",
	} {
		if !strings.Contains(release, want) {
			t.Fatalf("release workflow missing %q", want)
		}
	}
	for _, forbidden := range []string{"docker build", "docker push", "gcloud builds", "gcloud run jobs replace"} {
		if strings.Contains(release, forbidden) {
			t.Fatalf("release workflow must not rebuild or replace images: found %q", forbidden)
		}
	}
	if strings.Contains(release, "add-iam-policy-binding") || strings.Contains(release, "roles/") {
		t.Fatal("release workflow must not expand IAM")
	}
	if strings.Contains(release, "--allow-unauthenticated") {
		t.Fatal("production worker job must remain private")
	}
	rollback := workflowSection(t, release, "      - name: Freeze production rollback contract", "      - name: Update production worker")
	if !strings.Contains(rollback, "scripts/render_worker_deployment_evidence.py prepare-rollback") {
		t.Fatal("rollback freeze must use the checked-in deterministic validator")
	}
	for _, want := range []string{
		"id: rollback",
		"EVIDENCE_ARTIFACT_NAME=\"worker-deployment-evidence-${COMMIT_SHA}\"",
		"--artifact-name \"$EVIDENCE_ARTIFACT_NAME\"",
		"echo \"artifact_name=$EVIDENCE_ARTIFACT_NAME\" >> \"$GITHUB_OUTPUT\"",
		"--output \"$ROLLBACK_CONTRACT\"",
	} {
		if !strings.Contains(rollback, want) {
			t.Fatalf("rollback freeze missing frozen artifact identity %q", want)
		}
	}
	if strings.Contains(rollback, "production-job-contract.json") {
		t.Fatal("rollback freeze must not identify an unuploaded transient file")
	}
	for _, want := range []string{
		"EVIDENCE_DIR: ${{ runner.temp }}/worker-deployment-evidence",
		"ROLLBACK_CONTRACT: ${{ runner.temp }}/worker-deployment-evidence/rollback-contract.json",
		"METADATA: ${{ runner.temp }}/worker-deployment-evidence/metadata.json",
		"EVIDENCE: ${{ runner.temp }}/worker-deployment-evidence/deployment-evidence.json",
	} {
		if !strings.Contains(release, want) {
			t.Fatalf("release workflow missing canonical evidence path %q", want)
		}
	}
	if strings.Count(release, "EVIDENCE_ARTIFACT_NAME=\"worker-deployment-evidence-${COMMIT_SHA}\"") != 1 {
		t.Fatal("release workflow must create exactly one frozen evidence artifact identity")
	}
	metadata := workflowSection(t, release, "      - name: Prepare validated deployment metadata before mutation", "      - name: Update production worker")
	for _, want := range []string{
		"> \"$METADATA\"",
		"scripts/render_worker_deployment_evidence.py validate-metadata",
		"--metadata \"$METADATA\"",
	} {
		if !strings.Contains(metadata, want) {
			t.Fatalf("pre-mutation metadata step missing %q", want)
		}
	}
	update := workflowSection(t, release, "      - name: Update production worker", "      - name: Render normalized deployment evidence after strict read-back")
	if strings.Index(update, "--clear-volume-mounts") > strings.Index(update, "--clear-volumes") {
		t.Fatal("production update must clear mounts before volumes atomically")
	}
	readback := workflowSection(t, release, "      - name: Render normalized deployment evidence after strict read-back", "      - name: Upload normalized deployment evidence")
	for _, want := range []string{"scripts/render_worker_deployment_evidence.py render-evidence", "--bucket \"$BUCKET\"", "--rollback-contract \"$ROLLBACK_CONTRACT\"", "--metadata \"$METADATA\"", "--output \"$EVIDENCE\""} {
		if !strings.Contains(readback, want) {
			t.Fatalf("production read-back evidence step missing %q", want)
		}
	}
	if strings.Contains(readback, "CHECKED_AT") || strings.Contains(readback, "checked_at") {
		t.Fatal("provider verification timestamp must be generated by the renderer after read-back")
	}
	finalizer := workflowSection(t, release, "      - name: Finalize partial evidence when provider verification is unknown", "      - name: Upload normalized deployment evidence")
	if !strings.Contains(finalizer, "if: always()") || !strings.Contains(finalizer, "scripts/render_worker_deployment_evidence.py render-partial") {
		t.Fatal("release workflow must finalize truthful partial evidence after any failure")
	}
	if !strings.Contains(finalizer, "-f \"$ROLLBACK_CONTRACT\" && -f \"$METADATA\"") || !strings.Contains(finalizer, "[[ -e \"$EVIDENCE\" ]]") {
		t.Fatal("partial finalizer must require frozen inputs and preserve existing evidence")
	}
	upload := workflowSection(t, release, "      - name: Upload normalized deployment evidence", "      - name: Tag promoted production image")
	if !strings.Contains(upload, "if: always()") || !strings.Contains(upload, "path: ${{ env.EVIDENCE }}") || !strings.Contains(upload, "if-no-files-found: ignore") {
		t.Fatal("normalized evidence upload must run always against the canonical path")
	}
	if !strings.Contains(upload, "name: ${{ steps.rollback.outputs.artifact_name }}") {
		t.Fatal("normalized evidence upload must use the frozen rollback artifact identity")
	}
	if !strings.Contains(release, "group: promote-olw-pipeline-production") || !strings.Contains(release, "cancel-in-progress: false") {
		t.Fatal("production promotions must be serialized without cancellation")
	}
	if !strings.Contains(release, "prod-${COMMIT_SHA}-${GITHUB_RUN_ID}-${GITHUB_RUN_ATTEMPT}") {
		t.Fatal("production observability tag must be unique to the promotion run")
	}
}

func TestWorkerReleaseEmitsOneNormalizedEvidenceArtifactAfterReadback(t *testing.T) {
	release := readWorkflow(t, "../../.github/workflows/release-worker.yml")

	readbackAt := strings.Index(release, "      - name: Render normalized deployment evidence after strict read-back")
	if readbackAt < 0 {
		t.Fatal("production release is missing the strict read-back step")
	}
	evidenceSection := release[readbackAt:]
	for _, want := range []string{
		"scripts/render_worker_deployment_evidence.py render-evidence",
		"--rollback-contract",
		"--metadata",
		"--output \"$EVIDENCE\"",
		"render-partial",
		"name: ${{ steps.rollback.outputs.artifact_name }}",
	} {
		if !strings.Contains(evidenceSection, want) {
			t.Fatalf("release workflow missing normalized evidence contract %q", want)
		}
	}
	if strings.Count(evidenceSection, "actions/upload-artifact@v4") != 1 {
		t.Fatalf("release workflow must upload exactly one post-read-back artifact")
	}
	if strings.Count(evidenceSection, "path: ${{ env.EVIDENCE }}") != 1 {
		t.Fatalf("release workflow must upload exactly one canonical evidence path")
	}
	if strings.Contains(evidenceSection, "production-job-contract.json") {
		t.Fatal("release workflow must not upload the legacy standalone rollback artifact")
	}
}

const workerJobShapeFilter = `
.spec.template.spec.template.spec
| if type != "object" then error("missing Cloud Run job template spec") else . end
| if (.containers | type) != "array" or (.containers | length) != 1 then error("expected exactly one container") else . end
| if (.containers[0].image | type) != "string" then error("container image must be a string") else . end
| if (.containers[0].env | type) != "array" then error("container env must be an array") else . end
| if (.containers[0].args | type) != "array" then error("container args must be an array") else . end
| if ((.volumes // []) | type) != "array" then error("volumes must be missing/null or an array") else . end
| if ((.containers[0].volumeMounts // []) | type) != "array" then error("volume mounts must be missing/null or an array") else . end
`

func TestWorkerJobContractFixtures(t *testing.T) {
	valid := map[string]struct {
		image            string
		envNames         []string
		artifactEnvNames []string
		volumeCount      int
		mountCount       int
	}{
		"legacy-prod.json": {
			image:            "asia-east1-docker.pkg.dev/llm-wiki-cloud/cloud-run-images/olw-pipeline:latest",
			envNames:         []string{"BUCKET", "DATA_DIR", "WORKSPACE", "VAULT_PATH", "WORKSPACE_DIR", "LLM_API_KEY", "DEEPSEEK_API_KEY", "USER_ID", "PROJECT_ID", "TASK_TYPE", "UNRELATED"},
			artifactEnvNames: []string{"BUCKET", "DATA_DIR", "WORKSPACE", "VAULT_PATH", "WORKSPACE_DIR"},
			volumeCount:      1,
			mountCount:       1,
		},
		"executable-legacy.json": {
			image:            "asia-east1-docker.pkg.dev/llm-wiki-cloud/cloud-run-images/olw-pipeline:latest",
			envNames:         []string{"WORKSPACE", "WORKSPACE_DIR", "LLM_API_KEY", "DEEPSEEK_API_KEY", "USER_ID", "PROJECT_ID", "TASK_TYPE", "UNRELATED"},
			artifactEnvNames: []string{"WORKSPACE", "WORKSPACE_DIR"},
			volumeCount:      1,
			mountCount:       1,
		},
		"desired-prod.json": {
			image:            "asia-east1-docker.pkg.dev/llm-wiki-cloud/cloud-run-images/olw-pipeline@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			envNames:         []string{"BUCKET"},
			artifactEnvNames: []string{"BUCKET"},
			volumeCount:      0,
			mountCount:       0,
		},
		"desired-prod-omitted.json": {
			image:            "asia-east1-docker.pkg.dev/llm-wiki-cloud/cloud-run-images/olw-pipeline@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			envNames:         []string{"BUCKET"},
			artifactEnvNames: []string{"BUCKET"},
			volumeCount:      0,
			mountCount:       0,
		},
	}
	for name, want := range valid {
		var got struct {
			Image string `json:"image"`
			Env   []struct {
				Name string `json:"name"`
			} `json:"env"`
			Volumes      []json.RawMessage `json:"volumes"`
			VolumeMounts []json.RawMessage `json:"volumeMounts"`
		}
		out := runJQFixture(t, name, workerJobShapeFilter+` | {image: .containers[0].image, env: .containers[0].env, volumes: (.volumes // []), volumeMounts: (.containers[0].volumeMounts // [])}`)
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatalf("%s extraction was not JSON: %v", name, err)
		}
		if got.Image != want.image || len(got.Env) != len(want.envNames) || len(got.Volumes) != want.volumeCount || len(got.VolumeMounts) != want.mountCount {
			t.Fatalf("%s extracted contract mismatch: image=%q env=%d volumes=%d mounts=%d", name, got.Image, len(got.Env), len(got.Volumes), len(got.VolumeMounts))
		}
		for i, env := range got.Env {
			if env.Name != want.envNames[i] {
				t.Fatalf("%s env[%d] = %q, want %q", name, i, env.Name, want.envNames[i])
			}
		}

		var artifact struct {
			Image string `json:"image"`
			Env   []struct {
				Name  string `json:"name"`
				Value string `json:"value"`
			} `json:"env"`
			Args         []string          `json:"args"`
			Volumes      []json.RawMessage `json:"volumes"`
			VolumeMounts []json.RawMessage `json:"volumeMounts"`
		}
		artifactFilter := workerJobShapeFilter + ` | {
			image: .containers[0].image,
			env: [.containers[0].env[] | select(.name == "BUCKET" or .name == "DATA_DIR" or .name == "WORKSPACE" or .name == "VAULT_PATH" or .name == "WORKSPACE_DIR") | {name, value}],
			args: .containers[0].args,
			volumes: (.volumes // []),
			volumeMounts: (.containers[0].volumeMounts // [])
		}`
		artifactOut := runJQFixture(t, name, artifactFilter)
		if err := json.Unmarshal([]byte(artifactOut), &artifact); err != nil {
			t.Fatalf("%s artifact extraction was not JSON: %v", name, err)
		}
		if artifact.Image != want.image || len(artifact.Env) != len(want.artifactEnvNames) || len(artifact.Args) != 2 || len(artifact.Volumes) != want.volumeCount || len(artifact.VolumeMounts) != want.mountCount {
			t.Fatalf("%s artifact contract mismatch: image=%q env=%d args=%d volumes=%d mounts=%d", name, artifact.Image, len(artifact.Env), len(artifact.Args), len(artifact.Volumes), len(artifact.VolumeMounts))
		}
		for i, env := range artifact.Env {
			if env.Name != want.artifactEnvNames[i] {
				t.Fatalf("%s artifact env[%d] = %q, want %q", name, i, env.Name, want.artifactEnvNames[i])
			}
		}
	}

	for _, name := range []string{
		"malformed-missing-spec.json",
		"malformed-two-containers.json",
		"malformed-image.json",
		"malformed-env.json",
		"malformed-args.json",
		"malformed-volumes.json",
		"malformed-volume-mounts.json",
	} {
		if _, err := runJQFixtureE(name, workerJobShapeFilter); err == nil {
			t.Fatalf("%s unexpectedly passed mandatory Cloud Run shape validation", name)
		}
	}
}

func runJQFixture(t *testing.T, name, filter string) string {
	t.Helper()
	out, err := runJQFixtureE(name, filter)
	if err != nil {
		t.Fatalf("jq fixture %s failed: %v\n%s", name, err, out)
	}
	return out
}

func runJQFixtureE(name, filter string) (string, error) {
	path := filepath.Join("testdata", "lwc179", name)
	cmd := exec.Command("jq", "-e", filter, path)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func readWorkflow(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func workflowSection(t *testing.T, workflow, start, end string) string {
	t.Helper()
	startAt := strings.Index(workflow, start)
	endAt := strings.Index(workflow, end)
	if startAt < 0 || endAt <= startAt {
		t.Fatalf("invalid workflow section %q .. %q", start, end)
	}
	return workflow[startAt:endAt]
}
