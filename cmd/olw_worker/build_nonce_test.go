package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkerBuildNonceDefaultAndStartupLine(t *testing.T) {
	if buildNonce != "local" {
		t.Fatalf("local build nonce = %q, want local", buildNonce)
	}

	old := buildNonce
	buildNonce = "0123456789abcdef0123456789abcdef"
	t.Cleanup(func() { buildNonce = old })

	output := captureBuildNonceOutput(t)
	if output != "worker build_nonce=0123456789abcdef0123456789abcdef\n" {
		t.Fatalf("startup output = %q, want one stable nonce line", output)
	}
	if strings.Count(output, "worker build_nonce=") != 1 {
		t.Fatalf("startup output contains %d nonce lines, want exactly one", strings.Count(output, "worker build_nonce="))
	}
}

func TestWorkerBuildNonceStartupPrecedesCobraExecution(t *testing.T) {
	mainSource := readRepoFile(t, "cmd/olw_worker/main.go")
	printAt := strings.Index(mainSource, "printBuildNonce(os.Stdout)")
	executeAt := strings.Index(mainSource, "executeWorkerCommand(newRootCommand())")
	if printAt < 0 || executeAt < 0 || printAt > executeAt {
		t.Fatal("worker must print its build nonce before executing the Cobra command")
	}
}

func TestWorkerDockerfileBuildNonceContract(t *testing.T) {
	dockerfile := readRepoFile(t, "cmd/olw_worker/Dockerfile")
	for _, want := range []string{
		"ARG BUILD_NONCE",
		"BUILD_NONCE:?BUILD_NONCE is required",
		"sh ./cmd/olw_worker/validate_build_nonce.sh \"${BUILD_NONCE}\"",
		"-X main.buildNonce=${BUILD_NONCE}",
		"io.llm-wiki.build.nonce=\"${BUILD_NONCE}\"",
	} {
		if !strings.Contains(dockerfile, want) {
			t.Fatalf("Dockerfile missing nonce contract %q", want)
		}
	}
	if strings.Contains(dockerfile, "[0-9a-f][0-9a-f][0-9a-f][0-9a-f]") {
		t.Fatal("Dockerfile must not use enumerated hex glob length checks")
	}
	validateAt := strings.Index(dockerfile, "sh ./cmd/olw_worker/validate_build_nonce.sh \"${BUILD_NONCE}\"")
	goBuildAt := strings.Index(dockerfile, "go build")
	if validateAt < 0 || goBuildAt < 0 || validateAt > goBuildAt {
		t.Fatal("Dockerfile must validate BUILD_NONCE before go build")
	}
}

func TestDeployWorkerWorkflowBuildNonceContract(t *testing.T) {
	workflow := readRepoFile(t, ".github/workflows/deploy-worker.yml")
	if strings.Count(workflow, "date -u +%s%N") != 1 {
		t.Fatal("workflow must generate exactly one UTC nanosecond timestamp")
	}
	for _, want := range []string{
		"BUILD_TIMESTAMP_NS=$(date -u +%s%N)",
		"printf -v BUILD_NONCE '%032d' \"$BUILD_TIMESTAMP_NS\"",
		"[[ ! \"$BUILD_NONCE\" =~ ^[0-9a-f]{32}$ ]]",
		"echo \"build_nonce=$BUILD_NONCE\" >> \"$GITHUB_OUTPUT\"",
		"BUILD_NONCE: ${{ steps.nonce.outputs.build_nonce }}",
		"--build-arg BUILD_NONCE=\"$BUILD_NONCE\"",
	} {
		if !strings.Contains(workflow, want) {
			t.Fatalf("workflow missing nonce contract %q", want)
		}
	}
	nonceAt := strings.Index(workflow, "BUILD_TIMESTAMP_NS=$(date -u +%s%N)")
	buildAt := strings.Index(workflow, "docker build")
	if nonceAt < 0 || buildAt < 0 || nonceAt > buildAt {
		t.Fatal("workflow must generate the nonce before docker build")
	}
	if strings.Contains(workflow, "openssl rand") || strings.Contains(workflow, "BUILD_NONCE=${GITHUB_RUN_ID}") || strings.Contains(workflow, "BUILD_NONCE=$GITHUB_RUN_ID") {
		t.Fatal("workflow must derive the build value from the UTC timestamp, not randomness or a run ID")
	}
}

func TestBuildNonceIsNotPublicBFFBuildInfo(t *testing.T) {
	buildinfo := readRepoFile(t, "internal/buildinfo/buildinfo.go")
	if strings.Contains(buildinfo, "BuildNonce") || strings.Contains(buildinfo, "build_nonce") {
		t.Fatal("worker nonce must not be added to the public BFF build info package")
	}
}

func captureBuildNonceOutput(t *testing.T) string {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	printBuildNonce(writer)
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func readRepoFile(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join("..", "..", name)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
