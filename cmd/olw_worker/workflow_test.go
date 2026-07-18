package main

import (
	"os"
	"strings"
	"testing"
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
		"branches: [develop/1.0]",
		"workflow_dispatch:",
		"group: deploy-olw-pipeline-dev",
		"cancel-in-progress: true",
		"actions/setup-go@v5",
		"go test ./... -count=1",
		"go vet ./...",
		"go build ./...",
		"${SHA}-${GITHUB_RUN_ID}-${GITHUB_RUN_ATTEMPT}",
		"git rev-parse origin/develop/1.0",
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
	if strings.Count(workflow, "git rev-parse origin/develop/1.0") != 2 {
		t.Fatal("workflow must recheck develop/1.0 before and after the job update")
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

func workflowSection(t *testing.T, workflow, start, end string) string {
	t.Helper()
	startAt := strings.Index(workflow, start)
	endAt := strings.Index(workflow, end)
	if startAt < 0 || endAt <= startAt {
		t.Fatalf("invalid workflow section %q .. %q", start, end)
	}
	return workflow[startAt:endAt]
}
