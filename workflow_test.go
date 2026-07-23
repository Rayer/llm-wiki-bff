package main

import (
	"os"
	"strings"
	"testing"
)

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

func TestDeployWorkflowPushesDevelopAndMain(t *testing.T) {
	contents := readWorkflow(t, ".github/workflows/deploy-bff.yml")
	const pushBranchesStart = "  push:\n    branches:\n"
	start := strings.Index(contents, pushBranchesStart)
	if start == -1 {
		t.Fatal("deploy workflow is missing a push branches trigger")
	}
	pushBranches := contents[start+len(pushBranchesStart):]
	if end := strings.Index(pushBranches, "  workflow_dispatch:"); end != -1 {
		pushBranches = pushBranches[:end]
	}
	var branches []string
	for _, line := range strings.Split(pushBranches, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "- ") {
			t.Fatalf("deploy workflow push branch line must use YAML list syntax; got %q", line)
		}
		branches = append(branches, strings.TrimSpace(strings.TrimPrefix(line, "- ")))
	}
	want := []string{"develop/1.0", "main"}
	if len(branches) != len(want) {
		t.Fatalf("deploy workflow push branches = %q, want %q", branches, want)
	}
	for i, branch := range want {
		if branches[i] != branch {
			t.Fatalf("deploy workflow push branch %d = %q, want %q", i, branches[i], branch)
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
	if strings.Contains(contents, "develop/1.0") {
		t.Fatal("release workflow must not accept develop/1.0 provenance")
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
	if strings.Count(contents, "actions/upload-artifact@ea165f8d65b6e75b540449e92b4886f43607fa02") != 1 {
		t.Fatal("production BFF release must upload exactly one normalized evidence artifact")
	}
	if strings.Index(contents, "Tag promoted production image") < strings.Index(contents, "Upload normalized deployment evidence") {
		t.Fatal("production image tag must follow evidence upload")
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
			if tc.name == "production" {
				if len(commands) != 0 {
					t.Fatalf("production workflow has %d IAM mutation commands, want none", len(commands))
				}
				if !strings.Contains(contents, "gcloud run jobs get-iam-policy") {
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
