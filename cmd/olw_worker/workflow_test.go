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
}
