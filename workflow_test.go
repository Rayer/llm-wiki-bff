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
		body = regexp.MustCompile(`\$\{\{[^}]*\}\}`).ReplaceAllString(body, "workflow-expression")
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

func TestBFFDevWorkflowSerializesDeployments(t *testing.T) {
	contents := readWorkflow(t, ".github/workflows/deploy-bff.yml")
	var workflow struct {
		Concurrency struct {
			Group            string `yaml:"group"`
			CancelInProgress bool   `yaml:"cancel-in-progress"`
		} `yaml:"concurrency"`
	}
	if err := yaml.Unmarshal([]byte(contents), &workflow); err != nil {
		t.Fatalf("deploy workflow is not valid YAML: %v", err)
	}
	if workflow.Concurrency.Group != "llm-wiki-bff-dev" {
		t.Fatalf("BFF DEV concurrency group = %q, want llm-wiki-bff-dev", workflow.Concurrency.Group)
	}
	if workflow.Concurrency.CancelInProgress {
		t.Fatal("BFF DEV deploys must not cancel an in-flight provider mutation")
	}
}

func TestBFFDevWorkflowVerifiesExactRevisionAndTrafficBeforePublishing(t *testing.T) {
	contents := readWorkflow(t, ".github/workflows/deploy-bff.yml")
	deployAt := strings.Index(contents, "gcloud run deploy ${{ env.SERVICE_NAME }}")
	firstDescribe := strings.Index(contents, `gcloud run revisions describe "$LATEST_CREATED_REVISION"`)
	trafficAt := strings.Index(contents, "gcloud run services update-traffic")
	readbackAt := strings.Index(contents, `if ! SERVING_REVISION=$(jq`)
	secondDescribe := strings.Index(contents[firstDescribe+1:], `gcloud run revisions describe "$LATEST_CREATED_REVISION"`)
	if secondDescribe >= 0 {
		secondDescribe += firstDescribe + 1
	}
	artifactAt := strings.Index(contents, "- name: Upload image digest artifact")
	if deployAt < 0 || firstDescribe < 0 || trafficAt < 0 || readbackAt < 0 || secondDescribe < 0 || artifactAt < 0 ||
		!(deployAt < firstDescribe && firstDescribe < trafficAt && trafficAt < readbackAt && readbackAt < secondDescribe && secondDescribe < artifactAt) {
		t.Fatalf("BFF deployment contract must order deploy < first exact-revision verification < traffic update < readback < second exact-revision verification < artifact: deploy=%d first=%d traffic=%d readback=%d second=%d artifact=%d", deployAt, firstDescribe, trafficAt, readbackAt, secondDescribe, artifactAt)
	}

	canonicalCheck := `if [[ "$DEPLOYED_IMAGE_DIGEST" != "$IMMUTABLE_IMAGE" && "$DEPLOYED_IMAGE_DIGEST" != "$DIGEST" ]]; then`
	if strings.Count(contents, canonicalCheck) < 2 {
		t.Fatalf("BFF workflow must fail closed against both canonical digest representations at both verification points; found %d checks", strings.Count(contents, canonicalCheck))
	}
	if strings.Contains(contents, `[[ "$DEPLOYED_IMAGE_DIGEST" != "$IMMUTABLE_IMAGE" ]]`) {
		t.Fatal("BFF workflow must not compare revision image evidence only to the full immutable image reference")
	}

	trafficReadback := contents[readbackAt:secondDescribe]
	for _, want := range []string{
		"[ $traffic[]? | select((.percent // 0) > 0) ] as $positive",
		"($positive | length) != 1",
		"$positive[0].percent != 100",
		"($positive[0].tag? != null)",
		"$positive[0].revisionName != $revision",
	} {
		if !strings.Contains(trafficReadback, want) {
			t.Errorf("traffic readback is missing precise positive-target contract %q", want)
		}
	}
	if strings.Contains(trafficReadback, "($traffic | length) != 1") {
		t.Fatal("traffic readback must tolerate unrelated zero-percent tagged entries")
	}
	if strings.Contains(contents, "latestReadyRevisionName") {
		t.Fatal("BFF workflow must not use latestReadyRevisionName as deployment provenance")
	}
}

func TestBFFDevWorkflowTrafficPredicateFixtures(t *testing.T) {
	contents := readWorkflow(t, ".github/workflows/deploy-bff.yml")
	filter := extractWorkflowText(t, contents,
		`jq -er --arg revision "$LATEST_CREATED_REVISION" '`,
		`' <<<"$SERVICE_JSON"`,
	)
	revision := "bff-dev-20260819-abc"
	cases := []struct {
		name string
		file string
		want string
	}{
		{name: "exact untagged 100 percent target", file: "traffic-exact.json", want: revision},
		{name: "target plus unrelated zero percent tagged entries", file: "traffic-zero-percent-tagged.json", want: revision},
		{name: "multiple positive targets", file: "traffic-multiple-positive.json"},
		{name: "tagged 100 percent target", file: "traffic-tagged.json"},
		{name: "wrong revision", file: "traffic-wrong-revision.json"},
		{name: "percent is not exactly 100", file: "traffic-not-100.json"},
		{name: "no positive target", file: "traffic-no-positive.json"},
		{name: "missing revision name", file: "traffic-missing-revision-name.json"},
		{name: "invalid revision name", file: "traffic-invalid-revision-name.json"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixture := readWorkflow(t, "scripts/fixtures/"+tc.file)
			cmd := exec.Command("jq", "-er", "--arg", "revision", revision, filter)
			cmd.Stdin = strings.NewReader(fixture)
			output, err := cmd.CombinedOutput()
			if tc.want == "" {
				if err == nil {
					t.Fatalf("predicate accepted fixture; output=%q", output)
				}
				return
			}
			if err != nil {
				t.Fatalf("predicate rejected fixture: %v\n%s", err, output)
			}
			if got := strings.TrimSpace(string(output)); got != tc.want {
				t.Fatalf("predicate output = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestBFFDevWorkflowDigestGuardFixtures(t *testing.T) {
	contents := readWorkflow(t, ".github/workflows/deploy-bff.yml")
	guardStart := `if [[ "$DEPLOYED_IMAGE_DIGEST" != "$IMMUTABLE_IMAGE" && "$DEPLOYED_IMAGE_DIGEST" != "$DIGEST" ]]; then`
	guard := guardStart + extractWorkflowText(t, contents,
		guardStart,
		"\n          fi",
	)
	guard += "\nfi"
	guard = strings.ReplaceAll(guard, "\n            ", "\n")
	const digest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	const immutableImage = "asia-east1-docker.pkg.dev/llm-wiki-cloud/cloud-run-images/llm-wiki-bff@" + digest
	cases := []struct {
		name    string
		image   string
		wantErr bool
	}{
		{name: "full immutable image", image: immutableImage},
		{name: "bare digest", image: digest},
		{name: "unrelated repository", image: "asia-east1-docker.pkg.dev/other/repo@" + digest, wantErr: true},
		{name: "unrelated digest", image: "sha256:fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210", wantErr: true},
		{name: "malformed", image: "not-a-digest", wantErr: true},
		{name: "empty", image: "", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command("bash", "-euo", "pipefail", "-c", "set -euo pipefail\n"+guard)
			cmd.Env = append(os.Environ(), "IMMUTABLE_IMAGE="+immutableImage, "DIGEST="+digest, "DEPLOYED_IMAGE_DIGEST="+tc.image, "LATEST_CREATED_REVISION=bff-dev-20260819-abc")
			output, err := cmd.CombinedOutput()
			if tc.wantErr == (err == nil) {
				t.Fatalf("guard error = %v, want error %v; output=%s", err, tc.wantErr, output)
			}
		})
	}
}

func extractWorkflowText(t *testing.T, contents, start, end string) string {
	t.Helper()
	startAt := strings.Index(contents, start)
	if startAt < 0 {
		t.Fatalf("workflow is missing extraction start %q", start)
	}
	endAt := strings.Index(contents[startAt+len(start):], end)
	if endAt < 0 {
		t.Fatalf("workflow is missing extraction end %q after %q", end, start)
	}
	return contents[startAt+len(start) : startAt+len(start)+endAt]
}

func TestBFFDevWorkflowSetsExpansionModelWithoutChangingSecretBinding(t *testing.T) {
	contents := readWorkflow(t, ".github/workflows/deploy-bff.yml")
	if !strings.Contains(contents, "QUERY_EXPANSION_MODEL: deepseek-v4-flash") {
		t.Fatal("BFF DEV workflow must set the platform-owned expansion model")
	}
	for _, want := range []string{"QUERY_EXPANSION_REASONING: none", "ANSWER_SYNTHESIS_MODEL: deepseek-v4-pro", "ANSWER_SYNTHESIS_REASONING: none", "@QUERY_EXPANSION_REASONING=${{ env.QUERY_EXPANSION_REASONING }}", "@ANSWER_SYNTHESIS_MODEL=${{ env.ANSWER_SYNTHESIS_MODEL }}", "@ANSWER_SYNTHESIS_REASONING=${{ env.ANSWER_SYNTHESIS_REASONING }}"} {
		if !strings.Contains(contents, want) {
			t.Fatalf("BFF DEV workflow missing %q", want)
		}
	}
	if !strings.Contains(contents, "@QUERY_EXPANSION_MODEL=${{ env.QUERY_EXPANSION_MODEL }}") {
		t.Fatal("BFF DEV deploy must pass the non-secret expansion model config")
	}
	if !strings.Contains(contents, "DEEPSEEK_API_KEY=deepseek-apikey:latest") {
		t.Fatal("BFF DEV deploy must preserve the existing DeepSeek secret reference")
	}
}

func TestBFFDevWorkflowSetsQuerySelectionPolicy(t *testing.T) {
	contents := readWorkflow(t, ".github/workflows/deploy-bff.yml")
	for _, want := range []string{
		"QUERY_SELECTION_LIMIT: 10",
		"QUERY_SELECTION_EXPLORATION_SLOTS: 1",
		"QUERY_SELECTION_EVIDENCE_THRESHOLD: 2",
		"QUERY_EXPANSION_KEYWORDS_PER_ATTEMPT: 24",
		"QUERY_EXPANSION_ATTEMPTS: 3",
		"QUERY_MATCHING_RARE_KEYWORD_MAX_DOCUMENT_FREQUENCY: 1",
		"QUERY_SELECTION_LIMIT=${{ env.QUERY_SELECTION_LIMIT }}",
		"QUERY_SELECTION_EXPLORATION_SLOTS=${{ env.QUERY_SELECTION_EXPLORATION_SLOTS }}",
		"QUERY_SELECTION_EVIDENCE_THRESHOLD=${{ env.QUERY_SELECTION_EVIDENCE_THRESHOLD }}",
		"QUERY_EXPANSION_KEYWORDS_PER_ATTEMPT=${{ env.QUERY_EXPANSION_KEYWORDS_PER_ATTEMPT }}",
		"QUERY_EXPANSION_ATTEMPTS=${{ env.QUERY_EXPANSION_ATTEMPTS }}",
		"QUERY_MATCHING_RARE_KEYWORD_MAX_DOCUMENT_FREQUENCY=${{ env.QUERY_MATCHING_RARE_KEYWORD_MAX_DOCUMENT_FREQUENCY }}",
	} {
		if !strings.Contains(contents, want) {
			t.Fatalf("BFF DEV workflow missing %q", want)
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
		"status.latestCreatedRevisionName",
		"gcloud run revisions describe",
		"status.imageDigest",
		`[[ "$DEPLOYED_IMAGE_DIGEST" != "$IMMUTABLE_IMAGE" && "$DEPLOYED_IMAGE_DIGEST" != "$DIGEST" ]]`,
		"gcloud run services update-traffic",
		"--to-revisions \"$LATEST_CREATED_REVISION=100\"",
		"exactly one untagged 100% serving revision",
	} {
		if !strings.Contains(contents, want) {
			t.Errorf("deploy workflow is missing immutable build provenance contract %q", want)
		}
	}
	if strings.Contains(contents, "latestReadyRevisionName") {
		t.Fatal("deploy workflow must not use latestReadyRevisionName as deployment provenance")
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
	deployAt := strings.Index(contents, "gcloud run deploy")
	routeAt := strings.Index(contents, "gcloud run services update-traffic")
	artifactAt := strings.Index(contents, "Upload image digest artifact")
	preVerifyAt := strings.Index(contents, `if ! DEPLOYED_IMAGE_DIGEST=$(jq -er '.status.imageDigest' <<<"$REVISION_JSON")`)
	if deployAt < 0 || preVerifyAt < 0 || routeAt < 0 || artifactAt < 0 || deployAt > preVerifyAt || preVerifyAt > routeAt || routeAt > artifactAt {
		t.Fatal("deploy, exact revision verification/routing, and artifact publication are out of order")
	}
	if strings.Count(contents, "gcloud run revisions describe \"$LATEST_CREATED_REVISION\"") < 2 {
		t.Fatal("deploy workflow must describe the exact created revision before and after traffic mutation")
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
