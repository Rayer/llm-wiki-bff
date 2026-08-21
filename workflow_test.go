package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestDevWorkflowManualDispatchRequiresCanonicalDevelopSHA(t *testing.T) {
	contents := readWorkflow(t, ".github/workflows/deploy-bff.yml")

	var workflow struct {
		On struct {
			Push struct {
				Branches []string `yaml:"branches"`
			} `yaml:"push"`
			WorkflowDispatch struct {
				Inputs map[string]struct {
					Required bool   `yaml:"required"`
					Type     string `yaml:"type"`
				} `yaml:"inputs"`
			} `yaml:"workflow_dispatch"`
		} `yaml:"on"`
	}
	if err := yaml.Unmarshal([]byte(contents), &workflow); err != nil {
		t.Fatalf("deploy workflow is not valid YAML: %v", err)
	}
	if got, want := strings.Join(workflow.On.Push.Branches, ","), "main"; got != want {
		t.Fatalf("automatic push branches = %q, want %q", got, want)
	}
	commitInput, ok := workflow.On.WorkflowDispatch.Inputs["commit_sha"]
	if !ok || !commitInput.Required || commitInput.Type != "string" {
		t.Fatalf("workflow_dispatch.commit_sha must be a required string input, got %#v", commitInput)
	}

	checkout := workflowSection(t, contents, "      - name: Checkout code", "      - name: Validate deployment source")
	for _, want := range []string{
		"ref: ${{ github.event_name == 'workflow_dispatch' && inputs.commit_sha || github.sha }}",
		"fetch-depth: 0",
		"persist-credentials: false",
	} {
		if !strings.Contains(checkout, want) {
			t.Errorf("checkout is missing exact-candidate contract %q", want)
		}
	}

	source := workflowSection(t, contents, "      - name: Validate deployment source", "      - name: Setup Go")
	for _, want := range []string{
		`if [[ "$EVENT_NAME" == "workflow_dispatch" ]]; then`,
		`"$REF" != "refs/heads/develop"`,
		`"$REF_NAME" != "develop"`,
		`^[0-9a-f]{40}$`,
		"git fetch origin develop --force --no-tags",
		"git rev-parse HEAD",
		"git rev-parse origin/develop",
		`input commit SHA does not match checked-out HEAD`,
		`input commit SHA does not match current origin/develop`,
		`"$EVENT_NAME" == "push"`,
		`"$GITHUB_SHA"`,
	} {
		if !strings.Contains(source, want) {
			t.Errorf("source validation is missing %q", want)
		}
	}

	authAt := strings.Index(contents, "      - name: Authenticate to Google Cloud")
	buildAt := strings.Index(contents, "      - name: Build and deploy to Cloud Run")
	sourceAt := strings.Index(contents, "      - name: Validate deployment source")
	if sourceAt < 0 || authAt < 0 || buildAt < 0 || sourceAt > authAt || sourceAt > buildAt {
		t.Fatalf("source validation must precede authentication and build: source=%d auth=%d build=%d", sourceAt, authAt, buildAt)
	}
}

func TestAuthDeploymentAndRollbackReconcileAfterAttemptedMutation(t *testing.T) {
	deploy := readWorkflow(t, ".github/workflows/deploy-auth.yml")
	rollback := readWorkflow(t, ".github/workflows/rollback-auth.yml")

	strict := workflowSection(t, deploy, "      - name: Capture and verify Auth deployment evidence", "      - name: Reconcile Auth deployment outcome")
	strictUpload := workflowSection(t, deploy, "      - name: Upload Auth deployment evidence", "      - name: Reconcile Auth deployment outcome")
	reconcile := workflowSection(t, deploy, "      - name: Reconcile Auth deployment outcome", "      - name: Upload Auth deployment reconciliation")
	reconcileUpload := workflowSection(t, deploy, "      - name: Upload Auth deployment reconciliation", "      - name: Persist build image digest")
	for _, want := range []string{
		"if: ${{ always() && steps.deploy.outcome != 'skipped' }}",
		"id: strict_verify",
		".status.imageDigest",
		"status.traffic",
	} {
		if !strings.Contains(strict, want) {
			t.Errorf("strict deploy verification missing %q", want)
		}
	}
	if !strings.Contains(strictUpload, "if: steps.strict_verify.outcome == 'success'") || strings.Contains(strictUpload, "if: always()") {
		t.Fatal("strict deployment evidence upload must be success-only")
	}
	for _, want := range []string{"if: always()", "RECONCILIATION", "status.traffic", "version_http_status", "Cache-Control", "healthz_http_status", "provider_readback_available", "http_readback_available", "steps: {deploy:", "jq -n"} {
		if !strings.Contains(reconcile, want) {
			t.Errorf("deployment reconciliation missing %q", want)
		}
	}
	for name, contents := range map[string]string{"deploy": deploy, "rollback": rollback} {
		t.Run(name+" health probe path", func(t *testing.T) {
			if !strings.Contains(contents, "/api/v1/public/healthz") {
				t.Fatal("Auth health probe must use the public versioned health path")
			}
			for _, forbidden := range []string{`"https://$AUTH_DOMAIN/healthz"`, `"https://${{ env.AUTH_DOMAIN }}/healthz"`} {
				if strings.Contains(contents, forbidden) {
					t.Fatalf("Auth health probes must exclude the former public health path %q", forbidden)
				}
			}
		})
	}
	if !strings.Contains(reconcileUpload, "always()") || !strings.Contains(reconcileUpload, "if-no-files-found: error") {
		t.Fatal("deployment reconciliation upload must run after the attempted mutation and require its created artifact")
	}
	if strings.Contains(reconcile, "cat \"$VERSION_BODY\"") || strings.Contains(reconcile, "cat \"$SERVICE_JSON\"") || strings.Contains(reconcile, "response_body") {
		t.Fatal("deployment reconciliation must not dump provider or HTTP response bodies")
	}

	readback := workflowSection(t, rollback, "      - name: Verify rollback read-back", "      - name: Capture normalized rollback outcome")
	outcome := workflowSection(t, rollback, "      - name: Capture normalized rollback outcome", "      - name: Upload normalized rollback outcome")
	for _, want := range []string{"if: ${{ always() && steps.mutate.outcome != 'skipped' }}", "status.traffic", "actual_selected_traffic", "actual_serving_revision", "actual_serving_image", "version_http_status", "Cache-Control"} {
		if !strings.Contains(readback+outcome, want) {
			t.Errorf("rollback reconciliation missing %q", want)
		}
	}
	if !strings.Contains(outcome, "if: always()") || !strings.Contains(outcome, "jq -n") {
		t.Fatal("rollback outcome must always create a normalized artifact")
	}
	if strings.Contains(outcome, "cat \"$BODY\"") || strings.Contains(outcome, "response_body") {
		t.Fatal("rollback outcome must not dump HTTP response bodies")
	}
	if !strings.Contains(deploy, "previous_version_commit") || !strings.Contains(deploy, ".commit") {
		t.Fatal("deploy rollback evidence must retain the previous canonical commit")
	}
}

func TestWorkerDevWorkflowBindsManualDispatchToExactCanonicalDevelopSHA(t *testing.T) {
	contents := readWorkflow(t, ".github/workflows/deploy-worker.yml")

	var workflow struct {
		On struct {
			Push struct {
				Branches []string `yaml:"branches"`
			} `yaml:"push"`
			WorkflowDispatch struct {
				Inputs map[string]struct {
					Required bool   `yaml:"required"`
					Type     string `yaml:"type"`
				} `yaml:"inputs"`
			} `yaml:"workflow_dispatch"`
		} `yaml:"on"`
	}
	if err := yaml.Unmarshal([]byte(contents), &workflow); err != nil {
		t.Fatalf("deploy workflow is not valid YAML: %v", err)
	}
	if got, want := strings.Join(workflow.On.Push.Branches, ","), "main"; got != want {
		t.Fatalf("automatic push branches = %q, want %q", got, want)
	}
	commitInput, ok := workflow.On.WorkflowDispatch.Inputs["commit_sha"]
	if !ok || !commitInput.Required || commitInput.Type != "string" {
		t.Fatalf("workflow_dispatch.commit_sha must be a required string input, got %#v", commitInput)
	}

	checkout := workflowSection(t, contents, "      - uses: actions/checkout@v4", "      - name: Set up Go")
	for _, want := range []string{
		"ref: ${{ github.event_name == 'workflow_dispatch' && inputs.commit_sha || github.sha }}",
		"fetch-depth: 0",
		"persist-credentials: false",
	} {
		if !strings.Contains(checkout, want) {
			t.Errorf("checkout is missing exact-candidate contract %q", want)
		}
	}

	source := workflowSection(t, contents, "      - name: Validate deployment source", "      - name: Set up Go")
	for _, want := range []string{
		`if [[ "$EVENT_NAME" == "workflow_dispatch" ]]; then`,
		`"$REF" != "refs/heads/develop"`,
		`"$REF_NAME" != "develop"`,
		`^[0-9a-f]{40}$`,
		"malformed commit_sha",
		"git fetch origin develop --force --no-tags",
		"git rev-parse HEAD",
		"git rev-parse origin/develop",
		`input commit SHA does not match checked-out HEAD`,
		`input commit SHA does not match current origin/develop`,
		`"$EVENT_NAME" == "push"`,
		`"$GITHUB_SHA"`,
	} {
		if !strings.Contains(source, want) {
			t.Errorf("source validation is missing %q", want)
		}
	}
	runtimeAt := strings.Index(contents, "      - name: Update dev worker without GCSFuse")
	if runtimeAt < 0 {
		t.Fatal("worker workflow is missing the runtime update step")
	}
	runtime := contents[runtimeAt:]
	for _, want := range []string{`case "${GITHUB_REF_NAME}" in`, "develop|main)"} {
		if !strings.Contains(runtime, want) {
			t.Errorf("worker runtime is missing push branch contract %q", want)
		}
	}

	sourceAt := strings.Index(contents, "      - name: Validate deployment source")
	for _, cloudStep := range []string{
		"      - name: Authenticate to Google Cloud",
		"      - name: Set up Cloud SDK",
		"      - name: Configure Artifact Registry Docker auth",
	} {
		cloudAt := strings.Index(contents, cloudStep)
		if sourceAt < 0 || cloudAt < 0 || sourceAt > cloudAt {
			t.Fatalf("source validation must precede %s: source=%d cloud=%d", cloudStep, sourceAt, cloudAt)
		}
	}
}

func TestWorkerDevWorkflowUsesReadOnlyIAMPreflight(t *testing.T) {
	contents := readWorkflow(t, ".github/workflows/deploy-worker.yml")
	preflight := workflowSection(t, contents, "      - name: Verify existing dev worker IAM prerequisites", "      - name: Configure Artifact Registry Docker auth")
	for _, want := range []string{
		"gcloud run jobs get-iam-policy olw-pipeline-dev",
		"--project llm-wiki-cloud",
		"--region asia-east1",
		"--format=json",
		"roles/run.viewer",
		"serviceAccount:lwc-pipeline-dev@llm-wiki-cloud.iam.gserviceaccount.com",
	} {
		if !strings.Contains(preflight, want) {
			t.Errorf("worker IAM preflight is missing %q", want)
		}
	}
	if !hasWorkerIAMPolicyValidatorPipeline(preflight) {
		t.Fatal("worker IAM preflight must pipe $IAM_POLICY to the policy validator and fail closed")
	}
	mutatedPreflight := strings.Replace(preflight,
		`if ! printf '%s\n' "$IAM_POLICY" | python3 scripts/validate_cloud_run_job_iam_policy.py; then`,
		"if ! true; then", 1)
	if hasWorkerIAMPolicyValidatorPipeline(mutatedPreflight) {
		t.Fatal("worker IAM preflight assertion accepts a no-op replacement for the validator pipeline")
	}
	for _, forbidden := range []string{
		"add-iam-policy-binding",
		"remove-iam-policy-binding",
		"set-iam-policy",
	} {
		if strings.Contains(contents, forbidden) {
			t.Errorf("worker deploy workflow must not mutate IAM with %q", forbidden)
		}
	}
	for _, mutation := range []string{
		"gcloud projects add-iam-policy-binding --member serviceAccount:attacker@example.com",
		"gcloud run jobs remove-iam-policy-binding --member serviceAccount:attacker@example.com",
		"gcloud projects \\\n		  set-iam-policy policy.json",
	} {
		if !containsForbiddenWorkerIAMMutator(mutation) {
			t.Fatalf("worker IAM mutation assertion accepts %q", mutation)
		}
	}
	authAt := strings.Index(contents, "      - name: Authenticate to Google Cloud")
	sdkAt := strings.Index(contents, "      - name: Set up Cloud SDK")
	preflightAt := strings.Index(contents, "      - name: Verify existing dev worker IAM prerequisites")
	dockerAt := strings.Index(contents, "      - name: Configure Artifact Registry Docker auth")
	updateAt := strings.Index(contents, "      - name: Update dev worker without GCSFuse")
	if authAt < 0 || sdkAt < 0 || preflightAt < 0 || dockerAt < 0 || updateAt < 0 || !(authAt < sdkAt && sdkAt < preflightAt && preflightAt < dockerAt && dockerAt < updateAt) {
		t.Fatalf("worker deployment order must be auth < SDK < IAM preflight < docker auth < update: auth=%d sdk=%d preflight=%d docker=%d update=%d", authAt, sdkAt, preflightAt, dockerAt, updateAt)
	}
}

func hasWorkerIAMPolicyValidatorPipeline(preflight string) bool {
	const pipeline = `if ! printf '%s\n' "$IAM_POLICY" | python3 scripts/validate_cloud_run_job_iam_policy.py; then`
	return strings.Contains(preflight, pipeline) &&
		regexp.MustCompile(`(?s)`+regexp.QuoteMeta(pipeline)+`.*?exit 1\s+fi`).MatchString(preflight)
}

func containsForbiddenWorkerIAMMutator(contents string) bool {
	for _, name := range []string{
		"add-iam-policy-binding",
		"remove-iam-policy-binding",
		"set-iam-policy",
	} {
		if strings.Contains(contents, name) {
			return true
		}
	}
	return false
}

func TestCloudRunJobIAMPolicyValidator(t *testing.T) {
	testCases := []struct {
		name    string
		policy  string
		wantErr bool
	}{
		{
			name:    "required member only in different role",
			policy:  `{"bindings":[{"role":"roles/run.viewer","members":["serviceAccount:other@llm-wiki-cloud.iam.gserviceaccount.com"]},{"role":"roles/run.invoker","members":["serviceAccount:lwc-pipeline-dev@llm-wiki-cloud.iam.gserviceaccount.com"]}]}`,
			wantErr: true,
		},
		{
			name:   "valid",
			policy: `{"bindings":[{"role":"roles/run.viewer","members":["serviceAccount:lwc-pipeline-dev@llm-wiki-cloud.iam.gserviceaccount.com"]}]}`,
		},
		{name: "malformed JSON", policy: `{`, wantErr: true},
		{name: "missing bindings", policy: `{}`, wantErr: true},
		{name: "bindings wrong shape", policy: `{"bindings":{}}`, wantErr: true},
		{name: "binding wrong shape", policy: `{"bindings":["bad"]}`, wantErr: true},
		{name: "members wrong shape", policy: `{"bindings":[{"role":"roles/run.viewer","members":"bad"}]}`, wantErr: true},
		{name: "missing binding", policy: `{"bindings":[{"role":"roles/run.invoker","members":["serviceAccount:lwc-pipeline-dev@llm-wiki-cloud.iam.gserviceaccount.com"]}]}`, wantErr: true},
		{name: "wrong role", policy: `{"bindings":[{"role":"roles/run.admin","members":["serviceAccount:lwc-pipeline-dev@llm-wiki-cloud.iam.gserviceaccount.com"]}]}`, wantErr: true},
		{name: "wrong member", policy: `{"bindings":[{"role":"roles/run.viewer","members":["serviceAccount:other@llm-wiki-cloud.iam.gserviceaccount.com"]}]}`, wantErr: true},
		{name: "duplicate role binding", policy: `{"bindings":[{"role":"roles/run.viewer","members":[]},{"role":"roles/run.viewer","members":["serviceAccount:lwc-pipeline-dev@llm-wiki-cloud.iam.gserviceaccount.com"]}]}`, wantErr: true},
		{name: "duplicate required member", policy: `{"bindings":[{"role":"roles/run.viewer","members":["serviceAccount:lwc-pipeline-dev@llm-wiki-cloud.iam.gserviceaccount.com","serviceAccount:lwc-pipeline-dev@llm-wiki-cloud.iam.gserviceaccount.com"]}]}`, wantErr: true},
		{name: "required member in another binding", policy: `{"bindings":[{"role":"roles/run.viewer","members":["serviceAccount:lwc-pipeline-dev@llm-wiki-cloud.iam.gserviceaccount.com"]},{"role":"roles/run.invoker","members":["serviceAccount:lwc-pipeline-dev@llm-wiki-cloud.iam.gserviceaccount.com"]}]}`, wantErr: true},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command("python3", "scripts/validate_cloud_run_job_iam_policy.py")
			cmd.Stdin = strings.NewReader(tc.policy)
			output, err := cmd.CombinedOutput()
			if (err != nil) != tc.wantErr {
				t.Fatalf("validator error = %v, wantErr=%t; output=%s", err, tc.wantErr, output)
			}
			if strings.Contains(string(output), "lwc-pipeline-dev@llm-wiki-cloud.iam.gserviceaccount.com") {
				t.Fatal("validator must not print policy contents")
			}
		})
	}
}

func TestWorkerDevWorkflowYAMLAndRunBlocksAreExecutable(t *testing.T) {
	contents := readWorkflow(t, ".github/workflows/deploy-worker.yml")
	var document any
	if err := yaml.Unmarshal([]byte(contents), &document); err != nil {
		t.Fatalf("deploy workflow is not valid YAML: %v", err)
	}

	runBlockPattern := regexp.MustCompile(`(?m)^\s+run: \|\n((?:\s{10,}.+\n?)+)`)
	runs := runBlockPattern.FindAllStringSubmatch(contents, -1)
	if len(runs) == 0 {
		t.Fatal("deploy workflow has no executable run blocks")
	}
	for _, run := range runs {
		body := strings.TrimSpace(run[1])
		body = regexp.MustCompile(`(?m)^ {10}`).ReplaceAllString(body, "")
		body = regexp.MustCompile(`\$\{\{[^}]*\}\}`).ReplaceAllString(body, "workflow-expression")
		cmd := exec.Command("bash", "-n")
		cmd.Stdin = strings.NewReader(body)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("deploy workflow run block has invalid shell syntax: %v\n%s", err, output)
		}
	}
}

func TestDevWorkflowPreflightsExistingIAMBeforeMutation(t *testing.T) {
	contents := readWorkflow(t, ".github/workflows/deploy-bff.yml")
	preflight := workflowSection(t, contents, "      - name: Verify existing dev IAM prerequisites", "      - name: Build and deploy to Cloud Run")
	for _, want := range []string{
		"gcloud run jobs get-iam-policy",
		"roles/run.jobsExecutorWithOverrides",
		"serviceAccount:lwc-bff-dev@llm-wiki-cloud.iam.gserviceaccount.com",
		"gcloud run services get-iam-policy",
		"roles/run.invoker",
		"allUsers",
	} {
		if !strings.Contains(preflight, want) {
			t.Errorf("IAM preflight is missing %q", want)
		}
	}

	for _, forbidden := range []string{
		"gcloud run jobs add-iam-policy-binding",
		"--allow-unauthenticated",
	} {
		if strings.Contains(contents, forbidden) {
			t.Errorf("dev workflow must not contain IAM mutation %q", forbidden)
		}
	}

	preflightAt := strings.Index(contents, "      - name: Verify existing dev IAM prerequisites")
	buildAt := strings.Index(contents, "gcloud builds submit")
	deployAt := strings.Index(contents, "gcloud run deploy")
	if preflightAt < 0 || buildAt < 0 || deployAt < 0 || preflightAt > buildAt || preflightAt > deployAt {
		t.Fatalf("IAM preflight must precede Cloud Build and Cloud Run mutation: preflight=%d build=%d deploy=%d", preflightAt, buildAt, deployAt)
	}
}

func TestDevWorkflowYAMLAndRunBlocksAreExecutable(t *testing.T) {
	for _, workflow := range []string{".github/workflows/deploy-bff.yml", ".github/workflows/deploy-auth.yml", ".github/workflows/rollback-auth.yml"} {
		t.Run(workflow, func(t *testing.T) {
			contents := readWorkflow(t, workflow)
			var document any
			if err := yaml.Unmarshal([]byte(contents), &document); err != nil {
				t.Fatalf("deploy workflow is not valid YAML: %v", err)
			}

			runBlockPattern := regexp.MustCompile(`(?m)^\s+run: \|\n((?:\s{10,}.+\n?)+)`)
			runs := runBlockPattern.FindAllStringSubmatch(contents, -1)
			if len(runs) == 0 {
				t.Fatal("deploy workflow has no executable run blocks")
			}
			for _, run := range runs {
				body := strings.TrimSpace(run[1])
				body = regexp.MustCompile(`(?m)^ {10}`).ReplaceAllString(body, "")
				body = regexp.MustCompile(`\$\{\{[^}]*\}\}`).ReplaceAllString(body, "workflow-expression")
				cmd := exec.Command("bash", "-n")
				cmd.Stdin = strings.NewReader(body)
				if output, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("deploy workflow run block has invalid shell syntax: %v\n%s", err, output)
				}
			}
		})
	}
}

func TestDevWorkflowValidatesAndPassesBuildIdentity(t *testing.T) {
	contents := readWorkflow(t, ".github/workflows/deploy-bff.yml")
	for _, want := range []string{
		"fetch-depth: 0",
		"APP_VERSION=$(go run ./cmd/versioncheck VERSION)",
		"GIT_SHA=$(git rev-parse HEAD)",
		"GIT_BRANCH=\"${GITHUB_REF_NAME}\"",
		"git tag --points-at HEAD",
		"multiple exact git tags point at HEAD",
		"app_version=$APP_VERSION",
		"git_sha=$GIT_SHA",
		"git_branch=$GIT_BRANCH",
		"git_tag=$GIT_TAG",
		"--config cloudbuild-bff.yaml",
		"_APP_VERSION=$APP_VERSION",
		"_GIT_SHA=$GIT_SHA",
		"_GIT_BRANCH=$GIT_BRANCH",
		"_GIT_TAG=$GIT_TAG",
	} {
		if !strings.Contains(contents, want) {
			t.Errorf("dev workflow is missing build identity contract %q", want)
		}
	}
	assertWorkflowUsesCentralizedVersioncheck(t, contents)
}

func TestCIWorkflowPushesAndPRsMainAndDevelop(t *testing.T) {
	contents := readWorkflow(t, ".github/workflows/ci.yml")
	for _, want := range []string{
		"  push:\n    branches: [main, develop]",
		"  pull_request:\n    branches: [main, develop]",
	} {
		if !strings.Contains(contents, want) {
			t.Errorf("CI workflow is missing trigger contract %q", want)
		}
	}
}

func TestCIMainFastForwardEligibilityGate(t *testing.T) {
	contents := readWorkflow(t, ".github/workflows/ci.yml")
	job := workflowSection(t, contents, "  main-fast-forward-eligible:", "\n\n")
	want := normalizeWorkflowContract(`
main-fast-forward-eligible:
  if: github.event_name == 'push' && github.ref == 'refs/heads/develop'
  needs: test
  runs-on: ubuntu-latest
  steps:
    - uses: actions/checkout@11d5960a326750d5838078e36cf38b85af677262 # v4
      with:
        fetch-depth: 0
        persist-credentials: false
    - name: Verify main fast-forward eligibility
      run: |
        set -euo pipefail
        git fetch --no-tags origin main develop
        test "$(git rev-parse origin/develop)" = "$GITHUB_SHA"
        git merge-base --is-ancestor origin/main "$GITHUB_SHA"
`)
	if got := normalizeWorkflowContract(job); got != want {
		t.Errorf("main fast-forward gate contract changed\n got:\n%s\nwant:\n%s", got, want)
	}
}

func normalizeWorkflowContract(contents string) string {
	lines := strings.Split(strings.ReplaceAll(contents, "\r\n", "\n"), "\n")
	normalized := lines[:0]
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" {
			normalized = append(normalized, line)
		}
	}
	return strings.Join(normalized, "\n")
}

func TestBFFDevWorkflowPushesMainOnlyAndSupportsManualDispatch(t *testing.T) {
	contents := readWorkflow(t, ".github/workflows/deploy-bff.yml")
	for _, want := range []string{
		"  push:\n    branches: [main]",
		"  workflow_dispatch:",
	} {
		if !strings.Contains(contents, want) {
			t.Errorf("BFF dev workflow is missing trigger contract %q", want)
		}
	}
}

func TestBFFDevWorkflowHasRollbackTimeoutBudget(t *testing.T) {
	contents := readWorkflow(t, ".github/workflows/deploy-bff.yml")
	m := regexp.MustCompile(`(?m)^    timeout-minutes: (\d+)$`).FindStringSubmatch(contents)
	if len(m) != 2 {
		t.Fatal("BFF dev workflow is missing the job timeout")
	}
	if m[1] != "120" {
		t.Fatalf("BFF dev workflow timeout-minutes = %q, want exactly 120", m[1])
	}
}

func TestAuthDevWorkflowUsesCanonicalSourceAndExistingPrerequisites(t *testing.T) {
	contents := readWorkflow(t, ".github/workflows/deploy-auth.yml")
	if strings.Contains(contents, "  push:") {
		t.Fatal("Auth dev workflow must be manual-only")
	}
	if strings.Count(contents, "ALLOWED_ORIGINS: https://wiki.dev.rayer.idv.tw,https://llm-wiki-frontend-dev.vercel.app") != 2 {
		t.Fatal("Auth dev workflow must set both DEV browser origins in build and deploy steps")
	}
	for _, forbidden := range []string{"http://localhost:3000", "http://127.0.0.1:3000"} {
		if strings.Contains(contents, forbidden) {
			t.Fatalf("Auth deployed workflow must not configure local CORS origin %q", forbidden)
		}
	}
	for _, want := range []string{
		"SERVICE_NAME: llm-wiki-auth-dev",
		"AR_REPO: asia-east1-docker.pkg.dev/llm-wiki-cloud/cloud-run-images",
		"ref: ${{ inputs.commit_sha }}",
		"git fetch origin develop --force --no-tags",
		"git rev-parse origin/develop",
		"--config cloudbuild-auth.yaml",
		"llm-wiki-auth:$GIT_SHA",
		"FIRESTORE_DATABASE_ID: llm-wiki-cloud-dev",
		"RUNTIME_SERVICE_ACCOUNT: lwc-auth-dev@llm-wiki-cloud.iam.gserviceaccount.com",
		"JWT_SECRET_NAME: jwt-secret-dev",
		"gcloud iam service-accounts describe",
		"gcloud firestore databases describe",
		"gcloud secrets describe",
		"gcloud secrets get-iam-policy",
		"gcloud run services get-iam-policy",
		"roles/secretmanager.secretAccessor",
		"roles/run.invoker",
		"allUsers",
		"gcloud builds describe \"$BUILD_ID\"",
		"IMMUTABLE_IMAGE=\"${{ env.AR_REPO }}/llm-wiki-auth@$DIGEST\"",
		"latestCreatedRevisionName",
		"status.imageDigest",
		"auth-image-digest-$COMMIT_SHA.txt",
		"name: auth-image-digest-${{ steps.image_digest.outputs.commit_sha }}",
		"AUTH_DOMAIN: auth.dev.rayer.idv.tw",
		"group: deploy-auth-dev",
		"cancel-in-progress: false",
		"install_components: beta",
		"gcloud beta run domain-mappings describe",
		"--domain \"$AUTH_DOMAIN\"",
		"--region \"${{ env.REGION }}\"",
		"--platform managed",
		"--ingress all",
		"--max-instances 1",
		".metadata.annotations[\"run.googleapis.com/ingress\"]",
		"auth-deployment-evidence-$COMMIT_SHA.json",
		"previous_serving_revision",
		"previous_image",
		"new_revision",
		"new_image",
		"exact_commit",
		"https://$AUTH_DOMAIN/api/v1/public/version",
		"--max-time 20",
		"Cache-Control: no-cache",
		".commit == $commit and .service == $service",
		"previous version read-back",
		".service == $service and .revision == $revision",
		"runtime service account",
		"lwc-auth-dev@llm-wiki-cloud.iam.gserviceaccount.com",
		"GCP_PROJECT",
		"FIRESTORE_DATABASE_ID",
		"ALLOWED_HOSTS",
		"DEV_JWT",
		"JWT_SECRET",
		"jwt-secret-dev",
		"private-ranges-only",
		"status.traffic",
		"Cache-Control",
		"no-store",
	} {
		if !strings.Contains(contents, want) {
			t.Errorf("Auth dev workflow is missing contract %q", want)
		}
	}
	for _, want := range []string{
		"ALLOWED_HOSTS: auth.dev.rayer.idv.tw,auth-dev.rayer.idv.tw",
		`assert_env ALLOWED_HOSTS "auth.dev.rayer.idv.tw,auth-dev.rayer.idv.tw"`,
		`ALLOWED_ORIGINS: "https://wiki.dev.rayer.idv.tw,https://llm-wiki-frontend-dev.vercel.app"`,
	} {
		if !strings.Contains(contents, want) {
			t.Errorf("Auth dev workflow is missing migration contract %q", want)
		}
	}
	for _, forbidden := range []string{
		"gcloud run jobs",
		"add-iam-policy-binding",
		"remove-iam-policy-binding",
		"set-iam-policy",
		"--allow-unauthenticated",
		"DEEPSEEK_API_KEY",
		"BUCKET=",
		"PIPELINE_JOB",
	} {
		if strings.Contains(contents, forbidden) {
			t.Errorf("Auth dev workflow must not contain %q", forbidden)
		}
	}
	preflight := strings.Index(contents, "      - name: Verify existing dev Auth prerequisites")
	build := strings.Index(contents, "gcloud builds submit")
	deploy := strings.Index(contents, "gcloud run deploy")
	if preflight < 0 || build < 0 || deploy < 0 || preflight > build || preflight > deploy {
		t.Fatalf("Auth prerequisites must precede build and deploy: preflight=%d build=%d deploy=%d", preflight, build, deploy)
	}
	if strings.Count(contents, "gcloud run deploy") != 1 {
		t.Fatalf("Auth dev workflow must have exactly one Cloud Run deploy command")
	}
	preflightDomain := strings.Index(contents, "gcloud beta run domain-mappings describe")
	preflightBuild := strings.Index(contents, "gcloud builds submit")
	if preflightDomain < 0 || preflightBuild < 0 || preflightDomain > preflightBuild {
		t.Fatal("Auth domain mapping preflight must precede build mutation")
	}
	deployIndex := strings.Index(contents, "gcloud run deploy")
	postDeploy := strings.LastIndex(contents, "https://$AUTH_DOMAIN/api/v1/public/version")
	if postDeploy < 0 || postDeploy < deployIndex {
		t.Fatal("Auth version read-back must follow deploy")
	}
	authDomainCount := strings.Count(contents, "AUTH_DOMAIN: auth.dev.rayer.idv.tw")
	if authDomainCount != 1 {
		t.Fatalf("Auth workflow must declare canonical AUTH_DOMAIN exactly once, got %d", authDomainCount)
	}
	if !strings.Contains(contents, "LEGACY_AUTH_DOMAIN: auth-dev.rayer.idv.tw") {
		t.Fatal("Auth workflow must declare legacy auth probe domain")
	}
}

func TestAuthDeployUsesLegacyDomainForPreDeployRollbackProbeAndCanonicalPostDeployReadback(t *testing.T) {
	contents := readWorkflow(t, ".github/workflows/deploy-auth.yml")
	preDeploy := workflowSection(t, contents, "      - name: Capture pre-deploy Auth rollback evidence", "      - name: Upload pre-deploy Auth rollback evidence")
	if !strings.Contains(preDeploy, `"https://$LEGACY_AUTH_DOMAIN/api/v1/public/version"`) {
		t.Error("pre-deploy Auth rollback evidence must probe legacy host version endpoint")
	}
	if strings.Contains(preDeploy, `"https://$AUTH_DOMAIN/api/v1/public/version"`) {
		t.Error("pre-deploy Auth rollback probe must not use canonical host")
	}
	if !strings.Contains(preDeploy, `--arg previous_version_probe_domain "$LEGACY_AUTH_DOMAIN"`) || !strings.Contains(preDeploy, "previous_version_probe_domain") {
		t.Error("pre-deploy Auth rollback evidence must record the legacy probe domain")
	}

	postDeploy := workflowSection(t, contents, "      - name: Capture and verify Auth deployment evidence", "      - name: Upload Auth deployment evidence")
	if !strings.Contains(postDeploy, `"https://$AUTH_DOMAIN/api/v1/public/version"`) {
		t.Fatal("post-deploy Auth evidence capture must probe canonical version endpoint")
	}
	if !strings.Contains(postDeploy, `"https://$AUTH_DOMAIN/api/v1/public/healthz"`) {
		t.Fatal("post-deploy Auth evidence capture must probe canonical healthz endpoint")
	}
	if strings.Contains(postDeploy, `"https://$LEGACY_AUTH_DOMAIN/api/v1/public/version"`) {
		t.Fatal("post-deploy Auth evidence capture must not probe legacy domain")
	}

	legacyPreDeployProbeAt := strings.Index(contents, "https://$LEGACY_AUTH_DOMAIN/api/v1/public/version")
	deployAt := strings.Index(contents, "      - name: Deploy Auth to Cloud Run")
	if legacyPreDeployProbeAt < 0 || deployAt < 0 {
		t.Fatal("Auth workflow is missing legacy probe or deploy step")
	}
	if legacyPreDeployProbeAt > deployAt {
		t.Fatal("legacy pre-deploy version read-back must happen before Cloud Run deploy")
	}

	canonicalReadBackAt := strings.Index(contents, "https://$AUTH_DOMAIN/api/v1/public/version")
	if canonicalReadBackAt < 0 || canonicalReadBackAt < deployAt {
		t.Fatal("canonical version read-back must happen at or after Cloud Run deploy")
	}
	canonicalHealthAt := strings.Index(contents, "https://$AUTH_DOMAIN/api/v1/public/healthz")
	if canonicalHealthAt < 0 || canonicalHealthAt < deployAt {
		t.Fatal("canonical healthz read-back must happen at or after Cloud Run deploy")
	}
}

func TestBFFDevWorkflowAllowsBothMigrationOrigins(t *testing.T) {
	contents := readWorkflow(t, ".github/workflows/deploy-bff.yml")
	for _, want := range []string{
		"ALLOWED_ORIGINS: https://wiki.dev.rayer.idv.tw,https://llm-wiki-frontend-dev.vercel.app,http://localhost:3000,http://127.0.0.1:3000",
		"@ALLOWED_ORIGINS=${{ env.ALLOWED_ORIGINS }}",
	} {
		if !strings.Contains(contents, want) {
			t.Errorf("BFF DEV workflow is missing migration origin contract %q", want)
		}
	}
}

func TestBFFDevWorkflowUsesImmutableQueryConfigWithoutChangingSecretBinding(t *testing.T) {
	contents := readWorkflow(t, ".github/workflows/deploy-bff.yml")
	for _, want := range []string{
		"QUERY_STAGE_CONFIG_PATH: /app/configs/query/dev/query-dev-2026-08-21.1.json",
		"QUERY_STAGE_CONFIG_REVISION: query-dev-2026-08-21.1",
		"QUERY_STAGE_CONFIG_DIGEST: sha256:a35955fe4a451c740e6252cae8087f114fbac6b4162245d3de7818c1ad37a5c6",
		"QUERY_STAGE_CONFIG_PATH=${{ env.QUERY_STAGE_CONFIG_PATH }}",
		"--remove-env-vars \"QUERY_EXPANSION_MODEL,QUERY_EXPANSION_REASONING,ANSWER_SYNTHESIS_MODEL,ANSWER_SYNTHESIS_REASONING,QUERY_SELECTION_LIMIT,QUERY_SELECTION_EXPLORATION_SLOTS,QUERY_SELECTION_EVIDENCE_THRESHOLD,QUERY_EXPANSION_KEYWORDS_PER_ATTEMPT,QUERY_EXPANSION_ATTEMPTS,QUERY_MATCHING_RARE_KEYWORD_MAX_DOCUMENT_FREQUENCY\"",
	} {
		if !strings.Contains(contents, want) {
			t.Fatalf("BFF DEV workflow missing %q", want)
		}
	}
	for _, legacy := range []string{"QUERY_EXPANSION_MODEL", "QUERY_EXPANSION_REASONING", "ANSWER_SYNTHESIS_MODEL", "ANSWER_SYNTHESIS_REASONING", "QUERY_SELECTION_LIMIT", "QUERY_SELECTION_EXPLORATION_SLOTS", "QUERY_SELECTION_EVIDENCE_THRESHOLD", "QUERY_EXPANSION_KEYWORDS_PER_ATTEMPT", "QUERY_EXPANSION_ATTEMPTS", "QUERY_MATCHING_RARE_KEYWORD_MAX_DOCUMENT_FREQUENCY"} {
		if strings.Count(contents, legacy) != 1 {
			t.Fatalf("BFF DEV workflow must remove legacy query env %q exactly once", legacy)
		}
	}
	if !strings.Contains(contents, "DEEPSEEK_API_KEY=deepseek-apikey:latest") {
		t.Fatal("BFF DEV deploy must preserve the existing DeepSeek secret reference")
	}
}

func TestAuthPreflightUsesNamedFirestoreDatabaseAndSourceReconciliationIdentity(t *testing.T) {
	contents := readWorkflow(t, ".github/workflows/deploy-auth.yml")
	preflight := workflowSection(t, contents, "      - name: Verify existing dev Auth prerequisites", "      - name: Resolve build identity")
	if !strings.Contains(preflight, "gcloud firestore databases describe \\") || !strings.Contains(preflight, "            --database \"$FIRESTORE_DATABASE_ID\"") {
		t.Error("Auth Firestore preflight must use the named --database flag")
	}
	if strings.Contains(preflight, "gcloud firestore databases describe \"$FIRESTORE_DATABASE_ID\"") {
		t.Error("Auth Firestore preflight must not pass the database ID positionally")
	}

	reconcile := workflowSection(t, contents, "      - name: Reconcile Auth deployment outcome", "      - name: Persist build image digest")
	if want := "EVIDENCE_ID: ${{ steps.source.outputs.candidate_sha || format('run-{0}', github.run_id) }}"; !strings.Contains(reconcile, want) {
		t.Fatalf("Auth reconciliation must prefer the validated source SHA and fall back to the run ID: missing %q", want)
	}
	if strings.Contains(reconcile, "inputs.commit_sha") || strings.Contains(reconcile, "auth-deployment-reconciliation-${{ steps.source.outputs.candidate_sha }}") {
		t.Fatal("Auth reconciliation must not use raw input or an empty-prone source expression in a filename")
	}
	if !strings.Contains(reconcile, `if [[ ! "$EVIDENCE_ID" =~ ^([0-9a-f]{40}|run-[0-9]+)$ ]]; then`) {
		t.Fatal("Auth reconciliation must validate the evidence identity before creating a filename")
	}

	run := workflowSection(t, reconcile, "        run: |", "\n\n      - name: Upload Auth deployment reconciliation")
	run = regexp.MustCompile(`(?m)^ {10}`).ReplaceAllString(strings.TrimPrefix(run, "        run: |\n"), "")
	filenameAt := strings.Index(run, "HEADERS=$(mktemp)")
	if filenameAt < 0 {
		t.Fatal("Auth reconciliation identity prelude is missing")
	}
	identityPrelude := run[:filenameAt]
	for _, tc := range []struct {
		name       string
		evidenceID string
		wantPath   string
	}{
		{name: "validated candidate SHA", evidenceID: strings.Repeat("a", 40), wantPath: "auth-deployment-reconciliation-" + strings.Repeat("a", 40) + ".json"},
		{name: "source validation fallback", evidenceID: "run-123456789", wantPath: "auth-deployment-reconciliation-run-123456789.json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command("bash", "-c", identityPrelude+`printf '%s\n' "$EVIDENCE_ID" "$RECONCILIATION"`)
			cmd.Env = append(os.Environ(), "EVIDENCE_ID="+tc.evidenceID)
			output, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("reconciliation identity prelude failed: %v\n%s", err, output)
			}
			if got := strings.TrimSpace(string(output)); got != tc.evidenceID+"\n"+tc.wantPath {
				t.Fatalf("reconciliation identity output = %q, want %q", got, tc.evidenceID+"\n"+tc.wantPath)
			}
		})
	}
	cmd := exec.Command("bash", "-c", identityPrelude)
	cmd.Env = append(os.Environ(), "EVIDENCE_ID=bad/name")
	if output, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("unsafe reconciliation identity was accepted: %s", output)
	}

	if !strings.Contains(reconcile, `echo "evidence_id=$EVIDENCE_ID" >> "$GITHUB_OUTPUT"`) || !strings.Contains(reconcile, `echo "reconciliation=$RECONCILIATION" >> "$GITHUB_OUTPUT"`) {
		t.Fatal("Auth reconciliation must output both its evidence ID and generated path")
	}
	reconcileUpload := workflowSection(t, contents, "      - name: Upload Auth deployment reconciliation", "      - name: Persist build image digest")
	if !strings.Contains(reconcileUpload, "name: auth-deployment-reconciliation-${{ steps.reconcile.outputs.evidence_id }}") || !strings.Contains(reconcileUpload, "path: ${{ steps.reconcile.outputs.reconciliation }}") {
		t.Fatal("Auth reconciliation upload must use the reconcile step outputs")
	}
	if !strings.Contains(reconcileUpload, "steps.reconcile.outputs.evidence_id != ''") || !strings.Contains(reconcileUpload, "steps.reconcile.outputs.reconciliation != ''") {
		t.Fatal("Auth reconciliation upload must not run with empty reconcile outputs")
	}
	if strings.Contains(reconcileUpload, "steps.source.outputs.candidate_sha") || strings.Contains(reconcileUpload, "path: auth-deployment-reconciliation-") {
		t.Fatal("Auth reconciliation upload must not reconstruct a possibly empty filename")
	}
	if strings.Contains(reconcile, "auth-deployment-reconciliation-${{ steps.identity.outputs.git_sha }}") {
		t.Error("Auth reconciliation naming and path must not depend on the later identity step")
	}

	strictAndDigest := workflowSection(t, contents, "      - name: Capture and verify Auth deployment evidence", "      - name: Show deployment info")
	for _, want := range []string{
		"COMMIT_SHA: ${{ steps.identity.outputs.git_sha }}",
		"name: auth-deployment-evidence-${{ steps.identity.outputs.git_sha }}",
		"path: auth-deployment-evidence-${{ steps.identity.outputs.git_sha }}.json",
		"COMMIT_SHA=\"${{ steps.identity.outputs.git_sha }}\"",
		"DEV_IMAGE_TAG=\"${{ env.AR_REPO }}/llm-wiki-auth:dev-${{ steps.identity.outputs.git_sha }}\"",
	} {
		if !strings.Contains(strictAndDigest, want) {
			t.Errorf("Auth strict evidence/image digest identity binding changed: missing %q", want)
		}
	}
}

func TestAuthReconciliationMaterializationFailsClosedBeforeOutputs(t *testing.T) {
	contents := readWorkflow(t, ".github/workflows/deploy-auth.yml")
	reconcile := workflowSection(t, contents, "      - name: Reconcile Auth deployment outcome", "      - name: Upload Auth deployment reconciliation")
	materializeAt := strings.Index(reconcile, "          if ! jq -n \\")
	if materializeAt < 0 {
		t.Fatal("Auth reconciliation materialization is missing")
	}
	epilogue := reconcile[materializeAt:]
	epilogue = regexp.MustCompile(`(?m)^ {10}`).ReplaceAllString(epilogue, "")

	ifGuardAt := strings.Index(epilogue, "if ! jq -n")
	emptyGuardAt := strings.Index(epilogue, `if [[ ! -s "$RECONCILIATION" ]]; then`)
	outputAt := strings.Index(epilogue, `echo "evidence_id=$EVIDENCE_ID" >> "$GITHUB_OUTPUT"`)
	if ifGuardAt < 0 || emptyGuardAt < 0 || outputAt < 0 || ifGuardAt >= emptyGuardAt || emptyGuardAt >= outputAt {
		t.Fatal("Auth reconciliation must materialize, verify non-empty output, then export outputs")
	}
	if strings.Contains(epilogue, `test -s "$RECONCILIATION"`) || strings.Contains(epilogue, "exit 0") {
		t.Fatal("Auth reconciliation must not use a non-fatal bare test or unconditional success exit")
	}

	fakeBin := t.TempDir()
	if err := os.WriteFile(filepath.Join(fakeBin, "jq"), []byte("#!/usr/bin/env bash\ncase ${JQ_MODE:-valid} in\nfail) exit 1 ;;\nempty) exit 0 ;;\nesac\nprintf '%s\\n' '{\"evidence\":\"reconciliation\"}'\n"), 0o755); err != nil {
		t.Fatalf("write fake jq: %v", err)
	}
	run := func(t *testing.T, mode string) (string, error) {
		t.Helper()
		dir := t.TempDir()
		reconciliation := filepath.Join(dir, "reconciliation.json")
		outputPath := filepath.Join(dir, "github-output")
		env := os.Environ()
		for i, value := range env {
			if strings.HasPrefix(value, "PATH=") {
				env[i] = "PATH=" + fakeBin + ":" + os.Getenv("PATH")
			}
		}
		env = append(env,
			"SERVICE_NAME=llm-wiki-auth-dev",
			"DEPLOY_OUTCOME=failure",
			"STRICT_OUTCOME=failure",
			"PROVIDER_READBACK_AVAILABLE=false",
			"ACTUAL_TRAFFIC=[]",
			"SERVING_REVISIONS=[]",
			"VERSION_STATUS=000",
			"CACHE_CONTROL=",
			"VERSION_FIELDS={}",
			"HEALTH_STATUS=000",
			"EVIDENCE_ID=run-123",
			"RECONCILIATION="+reconciliation,
			"GITHUB_OUTPUT="+outputPath,
			"JQ_MODE="+mode,
		)
		cmd := exec.Command("bash", "-c", epilogue)
		cmd.Env = env
		return stringOutput(cmd, outputPath)
	}

	for _, mode := range []string{"fail", "empty"} {
		if output, err := run(t, mode); err == nil || output != "" {
			t.Fatalf("%s materialization reached output export: err=%v output=%q", mode, err, output)
		}
	}
	if output, err := run(t, "valid"); err != nil {
		t.Fatalf("valid materialization failed: %v", err)
	} else {
		lines := strings.Split(strings.TrimSpace(output), "\n")
		if len(lines) != 2 || lines[0] != "evidence_id=run-123" || !strings.HasPrefix(lines[1], "reconciliation=") {
			t.Fatalf("valid materialization output = %q", output)
		}
	}
}

func stringOutput(cmd *exec.Cmd, outputPath string) (string, error) {
	err := cmd.Run()
	contents, readErr := os.ReadFile(outputPath)
	if readErr != nil && !os.IsNotExist(readErr) {
		return "", readErr
	}
	return string(contents), err
}

func TestBFFDevWorkflowLimitsStageACompatibilityToOneInstance(t *testing.T) {
	contents := readWorkflow(t, ".github/workflows/deploy-bff.yml")
	deploy := workflowSection(t, contents, "      - name: Build and deploy to Cloud Run", "      - name: Persist build image digest")
	if !strings.Contains(deploy, "--max-instances 1") {
		t.Fatal("BFF DEV deploy must cap Stage A compatibility to one instance")
	}
	if strings.Contains(readWorkflow(t, ".github/workflows/release-bff.yml"), "--max-instances 1") {
		t.Fatal("production BFF release must not inherit the temporary DEV instance cap")
	}
}

func TestAuthRollbackWorkflowIsManualDEVOnlyAndEvidenceFirst(t *testing.T) {
	contents := readWorkflow(t, ".github/workflows/rollback-auth.yml")
	if strings.Contains(contents, "  push:") || strings.Contains(contents, "pull_request:") {
		t.Fatal("Auth rollback must be manual-only")
	}
	for _, want := range []string{
		"workflow_dispatch:",
		"target_revision:",
		"expected_image:",
		"expected_commit:",
		"environment: development",
		"group: deploy-auth-dev",
		"persist-credentials: false",
		"^[a-z][a-z0-9-]{0,61}[a-z0-9]$",
		"^[0-9a-f]{40}$",
		"asia-east1-docker\\.pkg\\.dev/llm-wiki-cloud/cloud-run-images/llm-wiki-auth@sha256:[0-9a-f]{64}",
		"gcloud run revisions describe",
		"serving.knative.dev/service",
		"status.imageDigest",
		"llm-wiki-auth-dev",
		"pre-mutation",
		"actions/upload-artifact@v4",
		"gcloud run services update-traffic",
		"--to-revisions",
		"status.traffic",
		"/api/v1/public/healthz",
		"/api/v1/public/version",
		"Cache-Control",
		"no-store",
		"if: always()",
		"outcome",
	} {
		if !strings.Contains(contents, want) {
			t.Errorf("Auth rollback workflow is missing contract %q", want)
		}
	}
	for _, forbidden := range []string{`"https://$AUTH_DOMAIN/healthz"`, `"https://${{ env.AUTH_DOMAIN }}/healthz"`} {
		if strings.Contains(contents, forbidden) {
			t.Fatalf("Auth rollback workflow must exclude the former public health path %q", forbidden)
		}
	}
	for _, forbidden := range []string{
		"gcloud run deploy",
		"add-iam-policy-binding",
		"remove-iam-policy-binding",
		"set-iam-policy",
		"iam service-accounts",
		"--update-env-vars",
		"--update-secrets",
		"production",
	} {
		if strings.Contains(contents, forbidden) {
			t.Errorf("Auth rollback workflow must not contain %q", forbidden)
		}
	}
	if strings.Index(contents, "pre-mutation") > strings.Index(contents, "gcloud run services update-traffic") {
		t.Fatal("rollback evidence must be created before traffic mutation")
	}
}

func TestAuthWorkflowsUseServingRevisionAndBindSourceCommitBeforeMutation(t *testing.T) {
	deploy := readWorkflow(t, ".github/workflows/deploy-auth.yml")
	rollback := readWorkflow(t, ".github/workflows/rollback-auth.yml")

	for _, want := range []string{
		"@LWC_SOURCE_COMMIT=$GIT_SHA",
		"assert_env LWC_SOURCE_COMMIT \"$COMMIT_SHA\"",
		"runtime_source_commit",
		"PREVIOUS_SERVING_REVISION",
		"previous_serving_revision",
		"NEW_REVISION",
	} {
		if !strings.Contains(deploy, want) {
			t.Errorf("Auth deploy workflow is missing revision source contract %q", want)
		}
	}
	preDeploy := workflowSection(t, deploy, "      - name: Capture pre-deploy Auth rollback evidence", "      - name: Upload pre-deploy Auth rollback evidence")
	if strings.Contains(preDeploy, "latestReadyRevisionName") {
		t.Fatal("pre-deploy rollback authority must come from serving traffic, not latestReadyRevisionName")
	}
	for _, want := range []string{
		".status.traffic",
		".percent // 0",
		".revisionName // \"\"",
		"previous_serving_revision",
		"previous_version_revision",
	} {
		if !strings.Contains(preDeploy, want) {
			t.Errorf("pre-deploy rollback evidence is missing serving revision contract %q", want)
		}
	}
	if strings.Contains(deploy, "previous_ready_revision") || strings.Contains(deploy, "PREVIOUS_READY_REVISION") {
		t.Fatal("Auth deploy evidence must not call the prior serving revision ready")
	}

	if strings.Contains(rollback, "latestReadyRevisionName") || strings.Contains(rollback, "CURRENT_READY_REVISION") || strings.Contains(rollback, "current_ready_revision") {
		t.Fatal("Auth rollback must not use latestReadyRevisionName as current routing authority")
	}
	for _, want := range []string{
		"LWC_SOURCE_COMMIT",
		"expected_commit",
		"type == \"Ready\"",
		"current_serving_revision",
		"current_image",
		".status.traffic",
	} {
		if !strings.Contains(rollback, want) {
			t.Errorf("Auth rollback workflow is missing pre-mutation contract %q", want)
		}
	}
	mutation := strings.Index(rollback, "gcloud run services update-traffic")
	if mutation < 0 {
		t.Fatal("Auth rollback workflow is missing traffic mutation")
	}
	for _, check := range []string{"LWC_SOURCE_COMMIT", "type == \"Ready\"", "current_serving_revision"} {
		if at := strings.Index(rollback, check); at < 0 || at > mutation {
			t.Fatalf("Auth rollback must check %q before traffic mutation", check)
		}
	}
}
func TestAuthDeployPostVerifyUsesServingTrafficAndLatestCreated(t *testing.T) {
	deploy := readWorkflow(t, ".github/workflows/deploy-auth.yml")
	section := workflowSection(t, deploy, "      - name: Capture and verify Auth deployment evidence", "      - name: Upload Auth deployment evidence")
	for _, want := range []string{
		"if ! LATEST_CREATED_REVISION=$(jq -er '.status.latestCreatedRevisionName // empty' <<<\"$SERVICE_JSON\")",
		"if ! NEW_REVISION=$(jq -er '",
		".status.traffic as $traffic |",
		"($traffic[0].percent // 0) != 100",
		"($traffic[0].revisionName // \"\")",
		"($traffic[0].tag? != null)",
		`[[ "$NEW_REVISION" != "$LATEST_CREATED_REVISION" ]]`,
		"gcloud run revisions describe \"$NEW_REVISION\"",
	} {
		if !strings.Contains(section, want) {
			t.Errorf("Auth post-deploy evidence is missing contract %q", want)
		}
	}
	if strings.Contains(section, "latestReadyRevisionName") {
		t.Fatal("Auth post-deploy evidence must not read latestReadyRevisionName")
	}
	selectorAt := strings.Index(section, "if ! NEW_REVISION=$(jq -er '")
	describeAt := strings.Index(section, "if ! REVISION_JSON=$(gcloud run revisions describe \"$NEW_REVISION\"")
	if selectorAt < 0 || describeAt < 0 || selectorAt > describeAt {
		t.Fatal("Auth post-deploy evidence must select revision from service traffic before revision describe")
	}

	reconcile := workflowSection(t, deploy, "      - name: Reconcile Auth deployment outcome", "      - name: Upload Auth deployment reconciliation")
	if !strings.Contains(reconcile, "latest_created_revision:") {
		t.Fatal("Auth reconciliation should report latest_created_revision")
	}
}

func TestAuthDeployUsesSupportedExplicitTrafficPromotionCommand(t *testing.T) {
	deploy := readWorkflow(t, ".github/workflows/deploy-auth.yml")
	section := workflowSection(t, deploy, "      - name: Deploy Auth to Cloud Run", "      - name: Capture and verify Auth deployment evidence")
	deployAt := strings.Index(section, "gcloud run deploy")
	promoteAt := strings.Index(section, "gcloud run services update-traffic")
	if deployAt < 0 || promoteAt < 0 || deployAt >= promoteAt {
		t.Fatal("Auth deploy must create the revision before explicit traffic promotion")
	}
	if strings.Contains(section[deployAt:promoteAt], "--to-latest") {
		t.Fatal("gcloud run deploy does not support --to-latest")
	}
	if !strings.Contains(section[promoteAt:], "--to-latest") {
		t.Fatal("Auth traffic promotion must explicitly use services update-traffic --to-latest")
	}
	if strings.Count(section, "gcloud run services update-traffic") != 1 {
		t.Fatal("Auth deploy must contain exactly one explicit traffic promotion command")
	}
}

func TestDockerfileRestoresPreRemediationBuildSemantics(t *testing.T) {
	contents := readWorkflow(t, "Dockerfile")
	if strings.Contains(contents, "ENV CGO_ENABLED=0") {
		t.Fatal("Dockerfile must not add a global CGO_ENABLED environment setting")
	}
	if !strings.Contains(contents, "RUN CGO_ENABLED=0 go generate ./... && CGO_ENABLED=0 go build") {
		t.Fatal("Dockerfile must disable CGO for both generation and build")
	}
}

func TestDockerfileProvidesPythonForBFFGeneration(t *testing.T) {
	contents := readWorkflow(t, "Dockerfile")
	buildStage := contents[:strings.Index(contents, "# Runtime")]
	pythonInstall := strings.Index(buildStage, "apk add --no-cache python3")
	generate := strings.Index(buildStage, "RUN CGO_ENABLED=0 go generate ./...")
	if pythonInstall < 0 || generate < 0 || pythonInstall > generate {
		t.Fatal("BFF build stage must provide python3 before go generate")
	}
}

func TestAuthCloudBuildAndDockerfileUseDistinctBinaryAndImmutableIdentity(t *testing.T) {
	cloudBuild := readWorkflow(t, "cloudbuild-auth.yaml")
	dockerfile := readWorkflow(t, "Dockerfile.auth")
	for _, want := range []string{
		"--file",
		"Dockerfile.auth",
		"APP_VERSION=${_APP_VERSION}",
		"GIT_SHA=${_GIT_SHA}",
		"GIT_BRANCH=${_GIT_BRANCH}",
		"GIT_TAG=${_GIT_TAG}",
		"${_IMAGE}",
	} {
		if !strings.Contains(cloudBuild, want) {
			t.Errorf("Auth Cloud Build config is missing %q", want)
		}
	}
	for _, want := range []string{
		"go build",
		"./cmd/auth",
		"ARG APP_VERSION=dev",
		"ARG GIT_SHA=unknown",
		"ARG GIT_BRANCH=unknown",
		"ARG GIT_TAG=",
		"org.opencontainers.image.version=${APP_VERSION}",
		"org.opencontainers.image.revision=${GIT_SHA}",
		"io.llm-wiki.image.tag=${GIT_SHA}",
		"-X github.com/rayer/llm-wiki-bff/internal/buildinfo.ProductVersion=${APP_VERSION}",
		"-X github.com/rayer/llm-wiki-bff/internal/buildinfo.GitSHA=${GIT_SHA}",
		"-X github.com/rayer/llm-wiki-bff/internal/buildinfo.GitBranch=${GIT_BRANCH}",
		"-X github.com/rayer/llm-wiki-bff/internal/buildinfo.GitTag=${GIT_TAG}",
		"-X github.com/rayer/llm-wiki-bff/internal/buildinfo.ImageTag=${GIT_SHA}",
	} {
		if !strings.Contains(dockerfile, want) {
			t.Errorf("Auth Dockerfile is missing %q", want)
		}
	}
}

func TestDeployWorkflowUsesImmutableCloudBuildResultDigest(t *testing.T) {
	contents := readWorkflow(t, ".github/workflows/deploy-bff.yml")
	for _, want := range []string{
		"id: build",
		`BUILD_RESULT=$(gcloud builds describe "$BUILD_ID"`,
		"--format=json",
		`[.results.images[]? | select(.name == $image)]`,
		`if [[ "$RESULT_IMAGE_COUNT" != "1" ]]; then`,
		`DIGEST=$(jq -er '.[0].digest' <<<"$RESULT_IMAGES")`,
		`if [[ ! "$DIGEST" =~ ^sha256:[0-9a-f]{64}$ ]]; then`,
		`IMMUTABLE_IMAGE="${{ env.AR_REPO }}/llm-wiki-bff@$DIGEST"`,
		`--image "$IMMUTABLE_IMAGE"`,
		`echo "digest=$DIGEST" >> "$GITHUB_OUTPUT"`,
		`DIGEST: ${{ steps.build.outputs.digest }}`,
		"latestReadyRevisionName",
		"gcloud run revisions describe",
		"status.imageDigest",
		`verify_revision "$CREATED_REVISION" "$IMMUTABLE_IMAGE"`,
		`(.status.imageDigest // "") == $image`,
		`validate_exact_traffic "$CREATED_REVISION" "$SERVICE_JSON"`,
	} {
		if !strings.Contains(contents, want) {
			t.Errorf("deploy workflow is missing immutable build provenance contract %q", want)
		}
	}
	if strings.Contains(contents, "gcloud artifacts docker images describe") {
		t.Fatal("deploy workflow must not re-resolve the mutable image tag after Cloud Build")
	}
	if strings.Contains(contents, `--image "$IMAGE"`) {
		t.Fatal("deploy workflow must not deploy the mutable Cloud Build output tag")
	}
	if strings.Index(contents, `BUILD_RESULT=$(gcloud builds describe "$BUILD_ID"`) > strings.Index(contents, `--image "$IMMUTABLE_IMAGE"`) {
		t.Fatal("deploy workflow must read the exact Cloud Build result before deploying")
	}
	if strings.Index(contents, "latestReadyRevisionName") > strings.Index(contents, "Upload image digest artifact") {
		t.Fatal("deploy workflow must verify the latest ready revision before uploading the digest artifact")
	}
}

func TestBFFDevImageTagIsRunScopedAndImmutable(t *testing.T) {
	contents := readWorkflow(t, ".github/workflows/deploy-bff.yml")
	tagStep := workflowSection(t, contents, "      - name: Tag deployed dev image (fail-closed)", "      - name: Show deployment info")
	want := `DEV_IMAGE_TAG="${{ env.AR_REPO }}/llm-wiki-bff:dev-${{ steps.identity.outputs.git_sha }}-${{ github.run_id }}-${{ github.run_attempt }}"`
	if !strings.Contains(tagStep, want) {
		t.Fatalf("BFF DEV tag must include commit SHA, run ID, and run attempt: missing %q", want)
	}
	if strings.Contains(tagStep, `:dev-${{ steps.identity.outputs.git_sha }}"`) {
		t.Fatal("BFF DEV tag must not be commit-only scoped")
	}
	if !strings.Contains(tagStep, "gcloud artifacts docker tags add") {
		t.Fatal("BFF DEV tag step must add the immutable image tag")
	}
}

func TestBFFDevWorkflowUsesCanonicalRevisionTransaction(t *testing.T) {
	contents := readWorkflow(t, ".github/workflows/deploy-bff.yml")
	build := workflowSection(t, contents, "      - name: Build and deploy to Cloud Run", "      - name: Persist build image digest")
	for _, want := range []string{
		"concurrency:\n  group: deploy-bff-dev\n  cancel-in-progress: false",
		"validate_exact_traffic() {",
		".status.traffic as $status",
		".spec.traffic as $spec",
		"latestRevision // false",
		"(($status[0].revisionName // \"\") == ($spec[0].revisionName // \"\"))",
		"PREVIOUS_ROLLBACK_REVISION_JSON=$(gcloud run revisions describe \"$PREVIOUS_ROLLBACK_REVISION\"",
		"PREVIOUS_ROLLBACK_IMAGE=$(jq -er '.status.imageDigest // empty' <<<\"$PREVIOUS_ROLLBACK_REVISION_JSON\")",
		"EXPECTED_IMAGE_PREFIX=\"${{ env.AR_REPO }}/llm-wiki-bff@\"",
		"PREVIOUS_ROLLBACK_DIGEST=\"${PREVIOUS_ROLLBACK_IMAGE#\"$EXPECTED_IMAGE_PREFIX\"}\"",
		"^sha256:[0-9a-f]{64}$",
		".type == \"Ready\" and .status == \"True\"",
		".type == \"ContainerReady\" and .status == \"True\"",
		"gcloud run deploy \"${{ env.SERVICE_NAME }}\"",
		"--no-traffic",
		"gcloud run services update-traffic \"${{ env.SERVICE_NAME }}\"",
		"--to-revisions \"${CREATED_REVISION}=100\"",
		"CUTOVER_EXIT=$?",
		"CUTOVER_VERIFIED=1",
		"CUTOVER_EXIT != 0 || CUTOVER_VERIFIED != 1",
		"rollback_traffic \"$PREVIOUS_ROLLBACK_REVISION\"",
		"echo \"previous_revision=$PREVIOUS_ROLLBACK_REVISION\" >> \"$GITHUB_OUTPUT\"",
		"echo \"new_revision=$CREATED_REVISION\" >> \"$GITHUB_OUTPUT\"",
		"echo \"traffic_mutated=true\" >> \"$GITHUB_OUTPUT\"",
		"GH_TOKEN: ${{ github.token }}",
		`gh api "repos/${GITHUB_REPOSITORY}/git/ref/heads/${GITHUB_REF_NAME}" --jq .object.sha`,
		`REMOTE_SHA=$(gh api "repos/${GITHUB_REPOSITORY}/git/ref/heads/${GITHUB_REF_NAME}" --jq .object.sha)`,
		`[[ ! "$REMOTE_SHA" =~ ^[0-9a-f]{40}$ ]]`,
		"Cloud Run traffic changed before cutover; refusing to mutate traffic.",
		"--remove-env-vars",
		"--update-env-vars",
		"QUERY_STAGE_CONFIG_PATH=${{ env.QUERY_STAGE_CONFIG_PATH }}",
		"--proto '=https'",
		"((.build | keys | sort) == [\"commit\", \"revision\", \"service\"])",
		"public query config readback identity/schema is invalid",
	} {
		if !strings.Contains(contents, want) {
			t.Errorf("BFF workflow missing contract %q", want)
		}
	}
	readback := workflowSection(t, contents, "      - name: Verify deployed query config readback", "      - name: Persist build image digest")
	if strings.Contains(readback, "--location") {
		t.Fatal("BFF readback must not follow redirects")
	}
	if !strings.Contains(readback, `^https://llm-wiki-bff-dev-[a-z0-9-]+\.a\.run\.app$`) {
		t.Fatal("BFF readback must restrict the Cloud Run host")
	}
	if strings.Count(readback, "--proto '=https'") != 1 || strings.Count(readback, "HTTP_STATUS") < 2 || !strings.Contains(readback, `[[ "$HTTP_STATUS" != "200" || "$READBACK_VALID" != "1" ]]`) {
		t.Fatal("BFF readback must use HTTPS-only no-redirect requests and reject non-200/schema-invalid responses")
	}

	previousCreatedAt := strings.Index(build, `PREVIOUS_CREATED_REVISION=$(jq -r '.status.latestCreatedRevisionName // empty'`)
	deployAt := strings.Index(build, `gcloud run deploy "${{ env.SERVICE_NAME }}"`)
	newCreatedAt := strings.Index(build, "\n            CREATED_REVISION=$(jq -r '.status.latestCreatedRevisionName // empty'")
	contextAt := strings.Index(build, `echo "previous_revision=$PREVIOUS_ROLLBACK_REVISION" >> "$GITHUB_OUTPUT"`)
	newRevisionOutputAt := strings.Index(build, `echo "new_revision=$CREATED_REVISION" >> "$GITHUB_OUTPUT"`)
	markerAt := strings.Index(build, `echo "traffic_mutated=true" >> "$GITHUB_OUTPUT"`)
	cutoverAt := strings.Index(build, "gcloud run services update-traffic \"${{ env.SERVICE_NAME }}\" \\\n            --to-revisions \"${CREATED_REVISION}=100\"")
	latestReadyAt := strings.Index(build, "latestReadyRevisionName")
	verifyNewAt := strings.Index(build, `if ! verify_revision "$CREATED_REVISION" "$IMMUTABLE_IMAGE"; then`)
	if previousCreatedAt < 0 || deployAt < 0 || newCreatedAt < 0 || contextAt < 0 || newRevisionOutputAt < 0 || markerAt < 0 || cutoverAt < 0 || latestReadyAt < 0 || verifyNewAt < 0 {
		t.Fatal("BFF workflow is missing revision identity or cutover anchors")
	}
	if !(previousCreatedAt < deployAt && deployAt < newCreatedAt && newCreatedAt < verifyNewAt && verifyNewAt < contextAt && contextAt < newRevisionOutputAt && newRevisionOutputAt < markerAt && markerAt < cutoverAt && cutoverAt < latestReadyAt) {
		t.Fatal("BFF workflow must identify and verify the new revision, persist rollback context, then cut over and verify latestReady")
	}
	ghTokenAt := strings.Index(build, "GH_TOKEN: ${{ github.token }}")
	remoteCheckAt := strings.Index(build, `REMOTE_SHA=$(gh api "repos/${GITHUB_REPOSITORY}/git/ref/heads/${GITHUB_REF_NAME}" --jq .object.sha)`)
	if ghTokenAt < 0 || remoteCheckAt < 0 || !(ghTokenAt < remoteCheckAt && remoteCheckAt < deployAt) {
		t.Fatal("BFF workflow must use GH_TOKEN and revalidate the exact remote ref immediately before deploy")
	}
	if strings.Contains(build, "git fetch") {
		t.Fatal("BFF workflow must not fetch the remote late in the deploy step")
	}
	preCutoverReadAt := strings.LastIndex(build[:cutoverAt], `SERVICE_JSON=$(gcloud run services describe "${{ env.SERVICE_NAME }}"`)
	preCutoverCheckAt := strings.LastIndex(build[:cutoverAt], `validate_exact_traffic "$PREVIOUS_ROLLBACK_REVISION" "$SERVICE_JSON"`)
	if preCutoverReadAt < 0 || preCutoverCheckAt < 0 || !(preCutoverReadAt < preCutoverCheckAt && preCutoverCheckAt < contextAt) {
		t.Fatal("BFF workflow must revalidate OLD traffic immediately before arming rollback and cutting over")
	}
	failureAt := strings.Index(build, "if (( CUTOVER_EXIT != 0 || CUTOVER_VERIFIED != 1 )); then")
	if failureAt < 0 {
		t.Fatal("BFF workflow is missing cutover failure branch")
	}
	successAt := strings.Index(build[failureAt:], `echo "✅ Deployed`)
	if successAt < 0 {
		t.Fatal("BFF workflow is missing successful deployment marker")
	}
	failureBranch := build[failureAt : failureAt+successAt]
	cutoverFlow := build[cutoverAt:latestReadyAt]
	verificationAt := strings.Index(cutoverFlow, "VERIFY_DEADLINE=$((SECONDS + 600))")
	if verificationAt < 0 || !strings.Contains(cutoverFlow[:verificationAt], "if (( CUTOVER_EXIT == 0 )); then") {
		t.Fatal("nonzero cutover command must roll back before the 600-second verification loop")
	}
	rollbackAt := strings.Index(failureBranch, `rollback_traffic "$PREVIOUS_ROLLBACK_REVISION"`)
	if rollbackAt < 0 || strings.Count(failureBranch, `rollback_traffic "$PREVIOUS_ROLLBACK_REVISION"`) != 1 {
		t.Fatal("BFF cutover failure must attempt exactly one rollback")
	}
	markerResetAt := strings.Index(failureBranch, `echo "traffic_mutated=false" >> "$GITHUB_OUTPUT"`)
	if markerResetAt < 0 {
		t.Fatal("BFF cutover failure should clear traffic_mutated on successful self-rollback path")
	}
	elseAt := strings.Index(failureBranch, "else")
	if elseAt < 0 {
		t.Fatal("BFF cutover failure branch must contain a successful self-rollback else path")
	}
	failedBranch := failureBranch[:elseAt]
	failedBranchExitAt := strings.Index(failedBranch, "exit 1")
	if failedBranchExitAt < 0 {
		t.Fatal("BFF failed self-rollback path should exit 1")
	}
	if markerResetAt <= failedBranchExitAt {
		t.Fatal("BFF cutover failure marker reset for failed self-rollback must not appear before exit")
	}
	successfulRollbackPath := failureBranch[elseAt:]
	successfulRollbackMarkerAt := strings.Index(successfulRollbackPath, `echo "traffic_mutated=false" >> "$GITHUB_OUTPUT"`)
	if successfulRollbackMarkerAt < 0 {
		t.Fatal("BFF cutover successful self-rollback path must reset traffic_mutated")
	}
	successfulRollbackExitAt := strings.Index(successfulRollbackPath, "exit 1")
	if successfulRollbackExitAt < 0 {
		t.Fatal("BFF cutover successful self-rollback path must exit after reset attempt")
	}
	if !(successfulRollbackMarkerAt < successfulRollbackExitAt) {
		t.Fatal("BFF successful self-rollback should reset marker before final exit")
	}
	if strings.Count(build, "latestReadyRevisionName") != 1 {
		t.Fatal("latestReadyRevisionName must only be used for post-cutover verification")
	}
	if strings.Contains(build, "\n            CREATED_REVISION=$(jq -er '.status.latestCreatedRevisionName // empty'") {
		t.Fatal("latestCreated polling must tolerate missing latestCreatedRevisionName")
	}
	if !strings.Contains(build, "PREVIOUS_CREATED_REVISION=$(jq -r '.status.latestCreatedRevisionName // empty'") {
		t.Fatal("pre-deploy previous created revision capture must use jq -r")
	}
	if strings.Contains(build, "PREVIOUS_CREATED_REVISION=$(jq -er '.status.latestCreatedRevisionName // empty'") {
		t.Fatal("pre-deploy previous created revision capture must not use jq -er")
	}
	digestOutputAt := strings.Index(build, `echo "digest=$DIGEST" >> "$GITHUB_OUTPUT"`)
	if digestOutputAt < 0 || digestOutputAt < failureAt {
		t.Fatal("digest outputs must be written only after successful cutover")
	}
	if strings.Contains(build[:cutoverAt], "--to-latest") || strings.Contains(build[:cutoverAt], "latestReadyRevisionName") {
		t.Fatal("BFF workflow must not use latestReady as created revision identity or use to-latest traffic")
	}

	preAt := strings.Index(contents, "      - name: Verify existing dev IAM prerequisites")
	stepDeployAt := strings.Index(contents, "      - name: Build and deploy to Cloud Run")
	persistAt := strings.Index(contents, "      - name: Persist build image digest")
	uploadAt := strings.Index(contents, "      - name: Upload image digest artifact")
	tagAt := strings.Index(contents, "      - name: Tag deployed dev image (fail-closed)")
	showAt := strings.Index(contents, "      - name: Show deployment info")
	cleanupAt := strings.Index(contents, "      - name: Restore BFF traffic on post-cutover failure")
	if preAt < 0 || stepDeployAt < 0 || persistAt < 0 || uploadAt < 0 || tagAt < 0 || showAt < 0 || cleanupAt < 0 {
		t.Fatalf("BFF workflow is missing required step anchors")
	}
	if !(preAt < stepDeployAt && stepDeployAt < persistAt && persistAt < uploadAt && uploadAt < tagAt && tagAt < showAt && showAt < cleanupAt) {
		t.Fatalf("BFF traffic safety flow must run: IAM preflight < deploy < persist < upload < tag < show < cleanup")
	}

	cleanup := contents[cleanupAt:]
	for _, want := range []string{
		"if: ${{ always() && (failure() || cancelled()) && steps.build.outputs.traffic_mutated == 'true' }}",
		"PREVIOUS_REVISION=\"${{ steps.build.outputs.previous_revision }}\"",
		"NEW_REVISION=\"${{ steps.build.outputs.new_revision }}\"",
		"gcloud run services update-traffic \"${{ env.SERVICE_NAME }}\"",
		"--to-revisions \"${PREVIOUS_REVISION}=100\"",
		"validate_exact_traffic() {",
		".status.traffic as $status",
		`SERVICE_JSON=$(gcloud run services describe "${{ env.SERVICE_NAME }}"`,
		`validate_exact_traffic "$PREVIOUS_REVISION" "$SERVICE_JSON"`,
		`validate_exact_traffic "$NEW_REVISION" "$SERVICE_JSON"`,
	} {
		if !strings.Contains(cleanup, want) {
			t.Errorf("BFF cleanup step missing contract %q", want)
		}
	}
	cleanupReadAt := strings.Index(cleanup, `SERVICE_JSON=$(gcloud run services describe "${{ env.SERVICE_NAME }}"`)
	oldCheckAt := strings.Index(cleanup, `validate_exact_traffic "$PREVIOUS_REVISION" "$SERVICE_JSON"`)
	newCheckAt := strings.Index(cleanup, `validate_exact_traffic "$NEW_REVISION" "$SERVICE_JSON"`)
	cleanupMutationAt := strings.Index(cleanup, "gcloud run services update-traffic")
	if cleanupReadAt < 0 || oldCheckAt < 0 || newCheckAt < 0 || cleanupMutationAt < 0 || !(cleanupReadAt < oldCheckAt && oldCheckAt < newCheckAt && newCheckAt < cleanupMutationAt) {
		t.Fatal("BFF cleanup must read and validate OLD/NEW traffic before mutation")
	}
	if strings.Contains(cleanup, "latestReadyRevisionName") {
		t.Fatal("BFF cleanup should validate exact serving traffic without requiring latestReadyRevisionName")
	}

	if strings.Contains(contents, "add-iam-policy-binding") || strings.Contains(contents, "set-iam-policy") || strings.Contains(contents, "remove-iam-policy-binding") || strings.Contains(contents, "--allow-unauthenticated") {
		t.Fatal("BFF dev workflow must not mutate IAM")
	}
}

func TestReleaseWorkflowRequiresMainBuildProvenance(t *testing.T) {
	contents := readWorkflow(t, ".github/workflows/release-bff.yml")
	for _, want := range []string{
		"if: github.ref == 'refs/heads/main'",
		"concurrency:",
		"group: promote-bff-production",
		"cancel-in-progress: false",
		"- name: Checkout main",
		"actions/checkout@11d5960a326750d5838078e36cf38b85af677262",
		"# v4",
		"ref: ${{ github.sha }}",
		"persist-credentials: false",
		"google-github-actions/auth@7c6bc770dae815cd3e89ee6cdf493a5fab2cc093",
		"# v3",
		"google-github-actions/setup-gcloud@e427ad8a34f8676edf47cf7d7925499adf3eb74f",
		"# v2",
		"actions/upload-artifact@ea165f8d65b6e75b540449e92b4886f43607fa02",
		"# v4.6.2",
		`git cat-file -e "$COMMIT_SHA^{commit}"`,
		`git merge-base --is-ancestor "$COMMIT_SHA" HEAD`,
		"commit_sha is not an ancestor of main",
		`.head_branch == "main"`,
		".html_url",
		"run_event=",
		"run_head_branch=",
		"run_head_sha=",
		"run_conclusion=",
		"roles/run.jobsExecutorWithOverrides",
		"get-iam-policy",
		"scripts/render_bff_deployment_evidence.py prepare-rollback",
		"scripts/render_bff_deployment_evidence.py render-evidence",
		"scripts/render_bff_deployment_evidence.py render-partial",
		"/api/v1/public/version",
		"actions/upload-artifact@ea165f8d65b6e75b540449e92b4886f43607fa02",
	} {
		if !strings.Contains(contents, want) {
			t.Errorf("release workflow is missing main provenance contract %q", want)
		}
	}
	for _, forbidden := range []string{"ref: develop", `origin/develop`, `.head_branch == "develop"`} {
		if strings.Contains(contents, forbidden) {
			t.Fatalf("release workflow must not accept develop provenance %q", forbidden)
		}
	}
	if strings.Contains(contents, "git fetch") {
		t.Fatal("release workflow must validate promotion ancestry from the full local checkout")
	}
	checkoutStart := strings.Index(contents, "      - name: Checkout main")
	checkoutEnd := strings.Index(contents, "      - name: Initialize deployment evidence paths")
	if checkoutStart < 0 || checkoutEnd < 0 || checkoutStart >= checkoutEnd {
		t.Fatal("release workflow checkout section is missing")
	}
	checkout := contents[checkoutStart:checkoutEnd]
	for _, forbidden := range []string{"token:", "GH_TOKEN", "github.token", "http.extraheader", "git config"} {
		if strings.Contains(checkout, forbidden) {
			t.Fatalf("release checkout must not inject credentials or tokens: found %q", forbidden)
		}
	}
}

func TestReleaseWorkflowAuthenticatesOnlyAfterReadOnlyGatesAndHasOneProviderMutation(t *testing.T) {
	contents := readWorkflow(t, ".github/workflows/release-bff.yml")
	auth := strings.Index(contents, "- name: Authenticate to Google Cloud")
	provenance := strings.Index(contents, "- name: Locate successful main dev deployment")
	image := strings.Index(contents, "- name: Download exact dev image digest")
	preflight := strings.Index(contents, "- name: Verify production IAM preflight")
	deploy := strings.Index(contents, "gcloud run deploy")
	if auth < 0 || provenance < 0 || image < 0 || preflight < 0 || deploy < 0 {
		t.Fatal("release workflow is missing the source, provenance, IAM, or deploy gates")
	}
	if auth < provenance || auth < image || auth > preflight || preflight > deploy {
		t.Fatal("release authentication/deploy order is not fail-closed")
	}
	serviceIAM := strings.Index(contents, "gcloud run services get-iam-policy")
	jobIAM := strings.Index(contents, "gcloud run jobs get-iam-policy")
	if serviceIAM < 0 || jobIAM < 0 || serviceIAM > deploy || jobIAM > deploy {
		t.Fatal("production release must read service and job IAM before deploy")
	}
	if strings.Contains(contents, "--allow-unauthenticated") {
		t.Fatal("production BFF release must not mutate service IAM through deploy")
	}
	if strings.Contains(contents, "add-iam-policy-binding") || strings.Contains(contents, "set-iam-policy") || strings.Contains(contents, "remove-iam-policy-binding") {
		t.Fatal("production BFF release must not mutate IAM")
	}
	if strings.Count(contents, "gcloud run deploy") != 1 {
		t.Fatal("production BFF release must have exactly one Cloud Run deploy mutation")
	}
	if strings.Count(contents, "actions/upload-artifact@ea165f8d65b6e75b540449e92b4886f43607fa02") != 2 {
		t.Fatal("production BFF release must upload exactly two pinned artifacts")
	}
	if strings.Index(contents, "Tag promoted production image") < strings.Index(contents, "Upload normalized deployment evidence") {
		t.Fatal("production image tag must follow evidence upload")
	}
}

func TestReleaseWorkflowStrictlyParsesQueryConfigReceipt(t *testing.T) {
	contents := readWorkflow(t, ".github/workflows/release-bff.yml")
	const startMarker = "          python3 - \"$DIGEST_FILE\" \"$GITHUB_OUTPUT\" <<'PY'\n"
	const endMarker = "          PY\n"
	start := strings.Index(contents, startMarker)
	if start < 0 {
		t.Fatal("release workflow is missing the strict receipt parser")
	}
	start += len(startMarker)
	end := strings.Index(contents[start:], endMarker)
	if end < 0 {
		t.Fatal("release workflow receipt parser heredoc is unterminated")
	}
	parser := contents[start : start+end]
	parserLines := strings.Split(parser, "\n")
	for i, line := range parserLines {
		parserLines[i] = strings.TrimPrefix(line, "          ")
	}
	parser = strings.Join(parserLines, "\n")

	imageDigest := "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	revision := "query-dev-2026-08-21.1"
	configDigest := "sha256:a35955fe4a451c740e6252cae8087f114fbac6b4162245d3de7818c1ad37a5c6"
	receipt := "image_digest=" + imageDigest + "\nquery_config_revision=" + revision + "\nquery_config_digest=" + configDigest + "\n"
	run := func(t *testing.T, input string) ([]byte, string, error) {
		t.Helper()
		dir := t.TempDir()
		artifact := filepath.Join(dir, "receipt.txt")
		output := filepath.Join(dir, "github-output")
		if err := os.WriteFile(artifact, []byte(input), 0600); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("python3", "-", artifact, output)
		cmd.Stdin = strings.NewReader(parser)
		cmd.Env = append(os.Environ(), "QUERY_STAGE_CONFIG_REVISION="+revision, "QUERY_STAGE_CONFIG_DIGEST="+configDigest, "AR_REPO=repo")
		combined, err := cmd.CombinedOutput()
		if err != nil {
			return combined, "", err
		}
		contents, err := os.ReadFile(output)
		return combined, string(contents), err
	}
	if output, written, err := run(t, receipt); err != nil {
		t.Fatalf("valid receipt rejected: %v: %s", err, output)
	} else if want := "digest=" + imageDigest + "\nimage=repo/llm-wiki-bff@" + imageDigest + "\nquery_config_revision=" + revision + "\nquery_config_digest=" + configDigest + "\n"; written != want {
		t.Fatalf("parser output = %q, want %q", written, want)
	}

	for name, input := range map[string]string{
		"duplicate":         receipt + "image_digest=" + imageDigest + "\n",
		"unknown":           "image_digest=" + imageDigest + "\nunknown=value\nquery_config_digest=" + configDigest + "\n",
		"missing":           "image_digest=" + imageDigest + "\nquery_config_revision=" + revision + "\n",
		"malformed digest":  "image_digest=sha256:BAD\nquery_config_revision=" + revision + "\nquery_config_digest=" + configDigest + "\n",
		"malformed key":     "image_digest=" + imageDigest + "\nquery_config_revision;evil=" + revision + "\nquery_config_digest=" + configDigest + "\n",
		"trailing junk":     receipt + "junk\n",
		"crlf":              strings.ReplaceAll(receipt, "\n", "\r\n"),
		"bare cr":           strings.ReplaceAll(receipt, "\n", "\r"),
		"shell content":     strings.Replace(receipt, imageDigest, imageDigest+";touch /tmp/pwned", 1),
		"revision mismatch": strings.Replace(receipt, revision, "query-prod-2026.08.21", 1),
		"digest mismatch":   strings.Replace(receipt, configDigest, "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", 1),
	} {
		t.Run(name, func(t *testing.T) {
			if output, _, err := run(t, input); err == nil {
				t.Fatalf("invalid receipt accepted: %s", output)
			}
		})
	}
}

func TestReleaseWorkflowDurablyUploadsRollbackBeforeMutation(t *testing.T) {
	contents := readWorkflow(t, ".github/workflows/release-bff.yml")
	freeze := strings.Index(contents, "- name: Freeze production rollback contract")
	metadata := strings.Index(contents, "- name: Prepare validated deployment metadata before mutation")
	rollbackUpload := strings.Index(contents, "- name: Upload immutable rollback contract")
	deploy := strings.Index(contents, "gcloud run deploy")
	evidenceUpload := strings.Index(contents, "- name: Upload normalized deployment evidence")
	if freeze < 0 || metadata < 0 || rollbackUpload < 0 || deploy < 0 || evidenceUpload < 0 {
		t.Fatal("release workflow is missing the freeze, metadata, rollback upload, deploy, or evidence upload stage")
	}
	if !(freeze < metadata && metadata < rollbackUpload && rollbackUpload < deploy && deploy < evidenceUpload) {
		t.Fatal("release workflow must freeze, validate metadata, durably upload rollback, deploy once, then upload final evidence")
	}
	if strings.Count(contents, "gcloud run deploy") != 1 {
		t.Fatal("production BFF release must have exactly one Cloud Run deploy mutation")
	}
	for _, want := range []string{
		`ROLLBACK_ARTIFACT_NAME="bff-rollback-contract-${COMMIT_SHA}"`,
		`EVIDENCE_ARTIFACT_NAME="bff-deployment-evidence-${COMMIT_SHA}"`,
		`--artifact-name "$ROLLBACK_ARTIFACT_NAME"`,
		`echo "rollback_artifact_name=$ROLLBACK_ARTIFACT_NAME" >> "$GITHUB_OUTPUT"`,
		"ROLLBACK_ARTIFACT_NAME: ${{ steps.rollback.outputs.rollback_artifact_name }}",
		`--arg rollback_artifact_name "$ROLLBACK_ARTIFACT_NAME"`,
		"name: ${{ steps.rollback.outputs.rollback_artifact_name }}",
		"path: ${{ env.ROLLBACK_CONTRACT }}",
		"path: ${{ env.EVIDENCE }}",
		"if-no-files-found: error",
		"retention-days: 90",
	} {
		if !strings.Contains(contents, want) {
			t.Errorf("release workflow is missing durable rollback contract %q", want)
		}
	}
	rollbackBlock := contents[rollbackUpload:deploy]
	if strings.Contains(rollbackBlock, "if: always()") || strings.Contains(rollbackBlock, "continue-on-error") {
		t.Fatal("rollback contract upload must fail closed before deploy")
	}
	evidenceBlock := contents[evidenceUpload:]
	if !strings.Contains(evidenceBlock, "if: always()") {
		t.Fatal("final deployment evidence upload must run always")
	}
	if !strings.Contains(evidenceBlock, "name: ${{ steps.rollback.outputs.evidence_artifact_name }}") {
		t.Fatal("final deployment evidence must use the distinct evidence artifact name")
	}
}

func TestReleaseWorkflowHasBoundedTimeoutBudget(t *testing.T) {
	contents := readWorkflow(t, ".github/workflows/release-bff.yml")
	if !strings.Contains(contents, "    timeout-minutes: 40") {
		t.Fatal("production release must reserve bounded time for deploy, convergence, HTTP readback, rollback, and evidence")
	}
	for _, forbidden := range []string{"    timeout-minutes: 20", "while true", "until true"} {
		if strings.Contains(contents, forbidden) {
			t.Fatalf("production release must not use the unbounded or undersized timeout contract %q", forbidden)
		}
	}
	readback := workflowSection(t, contents, "      - name: Verify deployed query config readback", "      - name: Persist build image digest")
	for _, want := range []string{"READBACK_REVISION_DEADLINE=$((SECONDS + 300))", "READBACK_DEADLINE=$((SECONDS + 300))"} {
		if !strings.Contains(readback, want) {
			t.Errorf("post-deploy readback is missing bounded loop %q", want)
		}
	}
}

func TestReleaseWorkflowValidatesAndPersistsFrozenLiveRevisionBeforeMutation(t *testing.T) {
	contents := readWorkflow(t, ".github/workflows/release-bff.yml")
	validation := workflowSection(t, contents, "      - name: Validate frozen rollback traffic before mutation", "      - name: Deploy existing immutable image to Cloud Run")
	for _, want := range []string{
		`SERVICE_JSON=$(gcloud run services describe "$SERVICE_NAME"`,
		`.status.latestCreatedRevisionName == $ready_revision`,
		`.status.latestReadyRevisionName == $ready_revision`,
		`.status.traffic | type == "array" and length == 1`,
		`.status.traffic[0].revisionName == $ready_revision`,
		`.status.traffic[0].percent == 100`,
		`(.status.traffic[0].latestRevision? // false) == false`,
		`echo "FROZEN_CREATED_REVISION=$FROZEN_CREATED_REVISION" >> "$GITHUB_ENV"`,
	} {
		if !strings.Contains(validation, want) {
			t.Errorf("pre-mutation frozen live validation is missing %q", want)
		}
	}
	if strings.Index(validation, "gcloud run services describe") > strings.Index(validation, "FROZEN_CREATED_REVISION=$(jq -er") {
		t.Fatal("frozen created revision must be extracted from the live service read")
	}
	if strings.Contains(validation, "gcloud run services update-traffic") || strings.Contains(validation, "gcloud run deploy") {
		t.Fatal("frozen live validation must not mutate before completing")
	}
}

func TestReleaseWorkflowRequiresExactChangedCreatedRevision(t *testing.T) {
	contents := readWorkflow(t, ".github/workflows/release-bff.yml")
	readback := workflowSection(t, contents, "      - name: Verify deployed query config readback", "      - name: Persist build image digest")
	deploy := strings.Index(contents, "gcloud run deploy")
	validation := strings.Index(contents, "      - name: Validate frozen rollback traffic before mutation")
	readbackStart := strings.Index(contents, "      - name: Verify deployed query config readback")
	if validation < 0 || deploy < 0 || readbackStart < 0 || !(validation < deploy && deploy < readbackStart) {
		t.Fatal("release mutation and post-deploy validation anchors are missing or misordered")
	}
	for _, want := range []string{
		`FROZEN_CREATED_REVISION="${FROZEN_CREATED_REVISION:?}"`,
		`CANDIDATE_REVISION=$(jq -er '.status.latestCreatedRevisionName | select(type == "string" and length > 0)'`,
		`[[ "$CANDIDATE_REVISION" != "$FROZEN_CREATED_REVISION" ]]`,
		`.status.latestCreatedRevisionName == $revision and .status.latestReadyRevisionName == $revision`,
	} {
		if !strings.Contains(readback, want) {
			t.Errorf("post-deploy exact changed-created contract is missing %q", want)
		}
	}
	if strings.Contains(readback, "CANDIDATE_REVISION=$(jq -er '.status.latestCreatedRevisionName // empty'") {
		t.Fatal("post-deploy candidate revision must reject empty latestCreatedRevisionName")
	}
	if strings.Contains(readback, "!= \"$FROZEN_READY_REVISION\"") {
		t.Fatal("post-deploy convergence must compare against the exact frozen latest-created revision")
	}
}

func TestReleaseWorkflowReconcilesFrozenRestoreRegardlessOfUpdateTrafficStatus(t *testing.T) {
	contents := readWorkflow(t, ".github/workflows/release-bff.yml")
	restore := workflowSection(t, contents, "      - name: Restore frozen production traffic after query-config readback failure", "      - name: Render normalized deployment evidence after strict read-back")
	mutation := strings.Index(restore, `gcloud run services update-traffic "$SERVICE_NAME"`)
	if mutation < 0 {
		t.Fatal("restore step is missing the traffic mutation")
	}
	command := restore[mutation:]
	status := strings.Index(command, "ROLLBACK_EXIT=$?")
	readback := strings.Index(command, "ROLLBACK_READBACK_DEADLINE=$((SECONDS + 240))")
	if status < 0 || readback < 0 || status > readback {
		t.Fatal("restore must capture update-traffic status before bounded readback")
	}
	if !strings.Contains(restore[:mutation], "set +e") || !strings.Contains(restore[mutation:], "set -e") {
		t.Fatal("restore must disable errexit only around update-traffic and restore it before readback")
	}
	if strings.LastIndex(restore[:mutation], "set +e") < 0 || strings.Index(restore[mutation:], "set -e") < 0 {
		t.Fatal("restore must bracket update-traffic with errexit handling")
	}
	for _, want := range []string{
		"while (( SECONDS < ROLLBACK_READBACK_DEADLINE )); do",
		"gcloud run services describe \"$SERVICE_NAME\"",
		"validate_restored_effective_traffic \"$SERVICE_JSON\"",
		".status.traffic | type == \"array\" and length == 1",
		"(.status.traffic[0] | keys | sort) == [\"percent\", \"revisionName\"]",
		".status.traffic[0].revisionName == $ready_revision",
		".status.traffic[0].percent == 100",
		"echo \"frozen production traffic was not authoritative within timeout (update exit $ROLLBACK_EXIT)\"",
		"echo \"restored effective routing: ${RESTORED_EFFECTIVE_REVISION}=${RESTORED_EFFECTIVE_PERCENT} (update exit $ROLLBACK_EXIT); preserving workflow failure\"",
		"exit 1",
	} {
		if !strings.Contains(restore, want) {
			t.Errorf("restore reconciliation is missing %q", want)
		}
	}
	if strings.Index(command, "ROLLBACK_EXIT=$?") > strings.Index(command, "while (( SECONDS < ROLLBACK_READBACK_DEADLINE )); do") {
		t.Fatal("readback must follow update-traffic for both zero and nonzero command statuses")
	}
}

func TestCIWorkflowValidatesProductVersion(t *testing.T) {
	contents := readWorkflow(t, ".github/workflows/ci.yml")
	for _, want := range []string{
		"Validate product version",
		"APP_VERSION=$(go run ./cmd/versioncheck VERSION)",
	} {
		if !strings.Contains(contents, want) {
			t.Errorf("CI workflow is missing VERSION validation contract %q", want)
		}
	}
	assertWorkflowUsesCentralizedVersioncheck(t, contents)
}

func assertWorkflowUsesCentralizedVersioncheck(t *testing.T, contents string) {
	t.Helper()
	for _, forbidden := range []string{
		"APP_VERSION=$(< VERSION)",
		"tr -d '[:space:]' < VERSION",
		`"$APP_VERSION" =~`,
	} {
		if strings.Contains(contents, forbidden) {
			t.Fatalf("workflow must use cmd/versioncheck without inline normalization or SemVer regex %q", forbidden)
		}
	}
}

func TestCloudBuildPassesAllIdentityArguments(t *testing.T) {
	contents := readWorkflow(t, "cloudbuild-bff.yaml")
	for _, want := range []string{
		"APP_VERSION=${_APP_VERSION}",
		"GIT_SHA=${_GIT_SHA}",
		"GIT_BRANCH=${_GIT_BRANCH}",
		"GIT_TAG=${_GIT_TAG}",
		"${_IMAGE}",
	} {
		if !strings.Contains(contents, want) {
			t.Errorf("Cloud Build config is missing %q", want)
		}
	}
}

func TestDockerfileEmbedsBuildIdentityWithoutGitContext(t *testing.T) {
	contents := readWorkflow(t, "Dockerfile")
	for _, want := range []string{
		"ARG APP_VERSION=dev",
		"ARG GIT_SHA=unknown",
		"ARG GIT_BRANCH=unknown",
		"ARG GIT_TAG=",
		"org.opencontainers.image.version=${APP_VERSION}",
		"org.opencontainers.image.revision=${GIT_SHA}",
		"org.opencontainers.image.ref.name=${GIT_BRANCH}",
		"io.llm-wiki.git.branch=${GIT_BRANCH}",
		"io.llm-wiki.git.tag=${GIT_TAG}",
		"io.llm-wiki.image.tag=${GIT_SHA}",
		"-X github.com/rayer/llm-wiki-bff/internal/buildinfo.ProductVersion=${APP_VERSION}",
		"-X github.com/rayer/llm-wiki-bff/internal/buildinfo.GitSHA=${GIT_SHA}",
		"-X github.com/rayer/llm-wiki-bff/internal/buildinfo.GitBranch=${GIT_BRANCH}",
		"-X github.com/rayer/llm-wiki-bff/internal/buildinfo.GitTag=${GIT_TAG}",
		"-X github.com/rayer/llm-wiki-bff/internal/buildinfo.ImageTag=${GIT_SHA}",
	} {
		if !strings.Contains(contents, want) {
			t.Errorf("Dockerfile is missing build identity contract %q", want)
		}
	}
	if strings.Contains(contents, "git rev-parse") || strings.Contains(contents, ".git/") {
		t.Fatal("Dockerfile must not derive build identity from a Git context")
	}
	if strings.Contains(contents, "GitRef") || strings.Contains(contents, "GIT_REF") || strings.Contains(contents, "io.llm-wiki.git.ref") {
		t.Fatal("Dockerfile must consistently use branch identity, not generic ref identity")
	}
}

func TestReleaseWorkflowPromotesExistingDigestWithoutRebuild(t *testing.T) {
	contents := readWorkflow(t, ".github/workflows/release-bff.yml")
	for _, want := range []string{
		"gcloud run deploy \"$SERVICE_NAME\" \\",
		"--image \"$IMMUTABLE_IMAGE\"",
		"gcloud artifacts docker tags add",
	} {
		if !strings.Contains(contents, want) {
			t.Errorf("release workflow is missing digest promotion contract %q", want)
		}
	}
	if strings.Contains(contents, "gcloud builds submit") || strings.Contains(contents, "docker build") {
		t.Fatal("release workflow must promote an existing digest without rebuilding")
	}
}

func readWorkflow(t *testing.T, path string) string {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(contents)
}

func workflowSection(t *testing.T, contents, start, end string) string {
	t.Helper()
	startAt := strings.Index(contents, start)
	if startAt < 0 {
		t.Fatalf("workflow is missing section start %q", start)
	}
	section := contents[startAt:]
	if endAt := strings.Index(section, end); endAt >= 0 {
		section = section[:endAt]
	}
	return section
}

func TestCloudRunWorkflowsUsePrivateRangesOnlyEgress(t *testing.T) {
	for _, workflow := range []string{
		".github/workflows/deploy-bff.yml",
		".github/workflows/deploy-auth.yml",
		".github/workflows/release-bff.yml",
	} {
		t.Run(workflow, func(t *testing.T) {
			contents, err := os.ReadFile(workflow)
			if err != nil {
				t.Fatalf("read workflow: %v", err)
			}

			if strings.Contains(string(contents), "--vpc-egress all-traffic") {
				t.Fatal("Cloud Run egress must not route all traffic through the VPC")
			}
			if !strings.Contains(string(contents), "--vpc-egress private-ranges-only") {
				t.Fatal("Cloud Run egress must route only private ranges through the VPC")
			}
		})
	}
}

func TestBFFWorkflowsGrantOnlyMatchingRuntimeServiceAccountJobExecution(t *testing.T) {
	testCases := []struct {
		name              string
		workflow          string
		pipelineJob       string
		runtimeServiceAcc string
	}{
		{
			name:              "development",
			workflow:          ".github/workflows/deploy-bff.yml",
			pipelineJob:       "olw-pipeline-dev",
			runtimeServiceAcc: "lwc-bff-dev@llm-wiki-cloud.iam.gserviceaccount.com",
		},
		{
			name:              "production",
			workflow:          ".github/workflows/release-bff.yml",
			pipelineJob:       "olw-pipeline",
			runtimeServiceAcc: "lwc-bff-prod@llm-wiki-cloud.iam.gserviceaccount.com",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			contents := readWorkflow(t, tc.workflow)
			assertWorkflowEnvDeclaration(t, contents, "PROJECT_ID", "llm-wiki-cloud")
			assertWorkflowEnvDeclaration(t, contents, "REGION", "asia-east1")
			assertWorkflowEnvDeclaration(t, contents, "PIPELINE_JOB_NAME", tc.pipelineJob)
			assertWorkflowEnvDeclaration(t, contents, "RUNTIME_SERVICE_ACCOUNT", tc.runtimeServiceAcc)

			commands := iamBindingCommands(contents)
			if tc.name == "development" || tc.name == "production" {
				if len(commands) != 0 {
					t.Fatalf("%s workflow has %d IAM mutation commands, want none", tc.name, len(commands))
				}
				if tc.name == "production" && !strings.Contains(contents, "gcloud run jobs get-iam-policy") {
					t.Fatal("production workflow must preflight the existing job IAM binding read-only")
				}
				return
			}
			if len(commands) != 1 {
				t.Fatalf("workflow has %d gcloud run jobs add-iam-policy-binding commands, want exactly 1", len(commands))
			}
			wantCommand := []string{
				`gcloud run jobs add-iam-policy-binding "${{ env.PIPELINE_JOB_NAME }}" \`,
				`--region "${{ env.REGION }}" \`,
				`--project "${{ env.PROJECT_ID }}" \`,
				`--member "serviceAccount:${{ env.RUNTIME_SERVICE_ACCOUNT }}" \`,
				`--role roles/run.jobsExecutorWithOverrides \`,
				"--quiet",
			}
			if got := strings.Join(commands[0], "\n"); got != strings.Join(wantCommand, "\n") {
				t.Errorf("job execution IAM command =\n%s\nwant exactly:\n%s", got, strings.Join(wantCommand, "\n"))
			}
			for _, forbidden := range []string{
				"gcloud projects add-iam-policy-binding",
				"roles/editor",
				"roles/owner",
				"roles/run.admin",
				"roles/run.developer",
			} {
				if strings.Contains(contents, forbidden) {
					t.Errorf("workflow must not grant project-wide Developer/Admin access %q", forbidden)
				}
			}
		})
	}
}

func assertWorkflowEnvDeclaration(t *testing.T, contents, key, want string) {
	t.Helper()
	values := workflowEnvValues(contents, key)
	if len(values) != 1 || values[0] != want {
		t.Fatalf("workflow env %s declarations = %q, want exactly [%q]", key, values, want)
	}
}

func workflowEnvValues(contents, key string) []string {
	var values []string
	envIndent := -1
	for _, line := range strings.Split(contents, "\n") {
		trimmed := strings.TrimSpace(line)
		indent := len(line) - len(strings.TrimLeft(line, " "))
		if envIndent >= 0 && trimmed != "" && indent <= envIndent {
			envIndent = -1
		}
		if trimmed == "env:" {
			envIndent = indent
			continue
		}
		if envIndent < 0 || trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		declaration, value, found := strings.Cut(trimmed, ": ")
		if found && declaration == key {
			values = append(values, value)
		}
	}
	return values
}

func iamBindingCommands(contents string) [][]string {
	var commands [][]string
	lines := strings.Split(contents, "\n")
	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(line, "gcloud run jobs add-iam-policy-binding") {
			continue
		}

		command := []string{line}
		for strings.HasSuffix(line, "\\") && i+1 < len(lines) {
			i++
			line = strings.TrimSpace(lines[i])
			command = append(command, line)
		}
		commands = append(commands, command)
	}
	return commands
}
