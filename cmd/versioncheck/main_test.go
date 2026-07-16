package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

var versioncheckBinary string

func TestMain(m *testing.M) {
	temporaryDirectory, err := os.MkdirTemp("", "versioncheck-test-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "create temporary directory: %v\n", err)
		os.Exit(1)
	}

	versioncheckBinary = filepath.Join(temporaryDirectory, "versioncheck")
	build := exec.Command("go", "build", "-o", versioncheckBinary, ".")
	build.Env = append(os.Environ(), "GOCACHE="+filepath.Join(temporaryDirectory, "go-build"))
	if output, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build versioncheck: %v\n%s", err, output)
		_ = os.RemoveAll(temporaryDirectory)
		os.Exit(1)
	}

	code := m.Run()
	_ = os.RemoveAll(temporaryDirectory)
	os.Exit(code)
}

func TestVersioncheckPrintsStrictSemVerVersion(t *testing.T) {
	tests := []struct {
		name     string
		contents string
		want     string
	}{
		{name: "zero version", contents: "0.0.0\n", want: "0.0.0\n"},
		{name: "release version", contents: "1.0.0\n", want: "1.0.0\n"},
		{name: "release version without trailing newline", contents: "1.0.0", want: "1.0.0\n"},
		{name: "prerelease", contents: "1.0.0-alpha\n", want: "1.0.0-alpha\n"},
		{name: "prerelease with numeric identifier", contents: "1.0.0-alpha.1\n", want: "1.0.0-alpha.1\n"},
		{name: "prerelease numeric identifiers", contents: "1.0.0-0.3.7\n", want: "1.0.0-0.3.7\n"},
		{name: "mixed prerelease identifiers", contents: "1.0.0-x.7.z.92\n", want: "1.0.0-x.7.z.92\n"},
		{name: "build metadata", contents: "1.0.0+20130313144700\n", want: "1.0.0+20130313144700\n"},
		{name: "build numeric identifier with leading zero", contents: "1.0.0+001\n", want: "1.0.0+001\n"},
		{name: "prerelease and build metadata", contents: "1.0.0-beta+exp.sha.5114f85\n", want: "1.0.0-beta+exp.sha.5114f85\n"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			versionFile := writeVersionFile(t, tt.contents)
			stdout, stderr, err := runVersioncheck(t, versionFile)
			if err != nil {
				t.Fatalf("versioncheck error = %v, stderr = %q", err, stderr)
			}
			if stdout != tt.want {
				t.Errorf("stdout = %q, want %q", stdout, tt.want)
			}
			if stderr != "" {
				t.Errorf("stderr = %q, want empty", stderr)
			}
		})
	}
}

func TestVersioncheckRejectsInvalidVersionFile(t *testing.T) {
	tests := []struct {
		name     string
		contents string
	}{
		{name: "missing patch", contents: "1\n"},
		{name: "missing minor and patch", contents: "1.0\n"},
		{name: "v prefix", contents: "v1.0.0\n"},
		{name: "leading zero major", contents: "01.0.0\n"},
		{name: "leading zero minor", contents: "1.01.0\n"},
		{name: "leading zero patch", contents: "1.0.01\n"},
		{name: "leading zero prerelease numeric identifier", contents: "1.0.0-01\n"},
		{name: "empty prerelease", contents: "1.0.0-\n"},
		{name: "empty prerelease identifier", contents: "1.0.0-a..b\n"},
		{name: "prerelease starts with dot", contents: "1.0.0-.a\n"},
		{name: "prerelease ends with dot", contents: "1.0.0-a.\n"},
		{name: "empty build identifier", contents: "1.0.0+..\n"},
		{name: "missing build identifier", contents: "1.0.0+\n"},
		{name: "repeated build separator", contents: "1.0.0+build+metadata\n"},
		{name: "invalid prerelease character", contents: "1.0.0-alpha_beta\n"},
		{name: "leading space", contents: " 1.0.0\n"},
		{name: "trailing space", contents: "1.0.0 \n"},
		{name: "embedded whitespace", contents: "1.0.0-alpha beta\n"},
		{name: "tab whitespace", contents: "1.0.0\t\n"},
		{name: "windows line ending", contents: "1.0.0\r\n"},
		{name: "multiple lines", contents: "1.0.0\n1.0.1\n"},
		{name: "multiple trailing newlines", contents: "1.0.0\n\n"},
		{name: "empty file", contents: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			versionFile := writeVersionFile(t, tt.contents)
			stdout, stderr, err := runVersioncheck(t, versionFile)
			if err == nil {
				t.Fatal("versioncheck error = nil, want non-zero exit status")
			}
			if got := exitCode(t, err); got != 1 {
				t.Errorf("exit code = %d, want 1", got)
			}
			if stdout != "" {
				t.Errorf("stdout = %q, want empty", stdout)
			}
			if !strings.Contains(stderr, "invalid SemVer") {
				t.Errorf("stderr = %q, want invalid SemVer message", stderr)
			}
		})
	}
}

func TestVersioncheckReportsUsageAndReadFailures(t *testing.T) {
	versionFile := writeVersionFile(t, "1.0.0\n")

	tests := []struct {
		name       string
		args       []string
		stderrPart string
		wantCode   int
	}{
		{name: "no arguments", stderrPart: "usage:", wantCode: 2},
		{name: "extra arguments", args: []string{versionFile, "extra"}, stderrPart: "usage:", wantCode: 2},
		{name: "missing file", args: []string{filepath.Join(t.TempDir(), "missing")}, stderrPart: "read VERSION file", wantCode: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stdout, stderr, err := runVersioncheck(t, tt.args...)
			if err == nil {
				t.Fatal("versioncheck error = nil, want non-zero exit status")
			}
			if got := exitCode(t, err); got != tt.wantCode {
				t.Errorf("exit code = %d, want %d", got, tt.wantCode)
			}
			if stdout != "" {
				t.Errorf("stdout = %q, want empty", stdout)
			}
			if !strings.Contains(stderr, tt.stderrPart) {
				t.Errorf("stderr = %q, want message containing %q", stderr, tt.stderrPart)
			}
		})
	}
}

func writeVersionFile(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "VERSION")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write VERSION: %v", err)
	}
	return path
}

func runVersioncheck(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	command := exec.Command(versioncheckBinary, args...)
	var stderrBuffer bytes.Buffer
	command.Stderr = &stderrBuffer
	output, err := command.Output()
	return string(output), stderrBuffer.String(), err
}

func exitCode(t *testing.T, err error) int {
	t.Helper()
	exitError, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("versioncheck error = %T %v, want process exit error", err, err)
	}
	return exitError.ExitCode()
}
