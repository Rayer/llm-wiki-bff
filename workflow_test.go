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
	if strings.Contains(contents, "develop/1.0") {
		t.Fatal("release workflow must not accept develop/1.0 provenance")
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
