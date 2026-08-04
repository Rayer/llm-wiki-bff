package main

import (
	"os"
	"os/exec"
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
		body = strings.ReplaceAll(body, "${{", "${")
		body = strings.ReplaceAll(body, "}}", "}")
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
	contents := readWorkflow(t, ".github/workflows/deploy-bff.yml")
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
		body = strings.ReplaceAll(body, "${{", "${")
		body = strings.ReplaceAll(body, "}}", "}")
		cmd := exec.Command("bash", "-n")
		cmd.Stdin = strings.NewReader(body)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("deploy workflow run block has invalid shell syntax: %v\n%s", err, output)
		}
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
		"- name: Checkout main",
		"ref: main",
		"git fetch origin main --force --no-tags",
		`git merge-base --is-ancestor "$COMMIT_SHA" origin/main`,
		"commit_sha is not an ancestor of main",
		`.head_branch == "main"`,
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
		"gcloud run deploy ${{ env.SERVICE_NAME }} \\",
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
			if tc.name == "development" {
				if len(commands) != 0 {
					t.Fatalf("development workflow has %d gcloud run jobs add-iam-policy-binding commands, want none", len(commands))
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
