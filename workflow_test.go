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
		"latestReadyRevisionName",
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
	if strings.Count(contents, "AUTH_DOMAIN:") != 1 {
		t.Fatalf("Auth dev workflow must declare one AUTH_DOMAIN value")
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

func TestDockerfileRestoresPreRemediationBuildSemantics(t *testing.T) {
	contents := readWorkflow(t, "Dockerfile")
	if strings.Contains(contents, "ENV CGO_ENABLED=0") {
		t.Fatal("Dockerfile must not add a global CGO_ENABLED environment setting")
	}
	if !strings.Contains(contents, "RUN go generate ./... && CGO_ENABLED=0 go build") {
		t.Fatal("Dockerfile must keep CGO_ENABLED scoped to the build instruction")
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
		`[[ "$DEPLOYED_IMAGE_DIGEST" != "$IMMUTABLE_IMAGE" ]]`,
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
