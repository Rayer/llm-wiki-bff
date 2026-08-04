package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	cloudstorage "cloud.google.com/go/storage"
	"github.com/rayer/llm-wiki-bff/internal/generation"
)

func TestLWC237RunningChildSentinelReachesContainerBeforeClose(t *testing.T) {
	var stdout, stderr, control bytes.Buffer
	withPipelineLogDestinations(t, &stdout, &stderr, &control, func() {
		vault := t.TempDir()
		if err := os.MkdirAll(filepath.Join(vault, "cache"), 0o755); err != nil {
			t.Fatal(err)
		}
		stdoutWriter, _, closeLog, err := pipelineLogWriters(vault, workerConfig{ExecutionID: "lwc237-live", cloudMode: true}, nil, false)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := stdoutWriter.Write([]byte("running-child-sentinel\n")); err != nil {
			t.Fatal(err)
		}
		line := bytes.TrimSpace(stdout.Bytes())
		var record map[string]any
		if err := json.Unmarshal(line, &record); err != nil || record["event"] != "child_output" {
			t.Fatalf("container output=%q record=%v err=%v", line, record, err)
		}
		if got := reconstructLWC237Output(t, record); string(got) != "running-child-sentinel\n" {
			t.Fatalf("container payload=%q, want sentinel", got)
		}
		if err := closeLog(); err != nil {
			t.Fatal(err)
		}
	})
}

func TestLWC237StdoutStderrAreStructuredIndependentAndExactlyOnce(t *testing.T) {
	var stdout, stderr, control bytes.Buffer
	withPipelineLogDestinations(t, &stdout, &stderr, &control, func() {
		vault := t.TempDir()
		if err := os.MkdirAll(filepath.Join(vault, "cache"), 0o755); err != nil {
			t.Fatal(err)
		}
		stdoutWriter, stderrWriter, closeLog, err := pipelineLogWriters(vault, workerConfig{
			UserID: "user-1", ProjectID: "project-1", ExecutionID: "exec-1",
		}, nil, false)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := stdoutWriter.Write([]byte("stdout-part-1")); err != nil {
			t.Fatal(err)
		}
		if _, err := stdoutWriter.Write([]byte("-part-2\n")); err != nil {
			t.Fatal(err)
		}
		if _, err := stderrWriter.Write([]byte("stderr-part-1")); err != nil {
			t.Fatal(err)
		}
		if _, err := stderrWriter.Write([]byte("-part-2\n")); err != nil {
			t.Fatal(err)
		}
		if err := closeLog(); err != nil {
			t.Fatal(err)
		}
		if got := stdout.String(); len(got) == 0 {
			t.Fatal("stdout pipeline had no data")
		}
		if got := stderr.String(); len(got) == 0 {
			t.Fatal("stderr pipeline had no data")
		}
		assertLWC237JSONStream(t, stdout.Bytes(), "stdout-part-1-part-2\n", "INFO", "stdout")
		assertLWC237JSONStream(t, stderr.Bytes(), "stderr-part-1-part-2\n", "WARNING", "stderr")
		if bytes.Contains(stdout.Bytes(), []byte("stderr-part")) || bytes.Contains(stderr.Bytes(), []byte("stdout-part")) {
			t.Fatalf("combined output was duplicated: stdout=%q stderr=%q", stdout.Bytes(), stderr.Bytes())
		}
	})
}

func TestLWC237CancellationKeepsAlreadyEmittedOutput(t *testing.T) {
	var stdout, stderr, control bytes.Buffer
	withPipelineLogDestinations(t, &stdout, &stderr, &control, func() {
		vault := t.TempDir()
		if err := os.MkdirAll(filepath.Join(vault, "cache"), 0o755); err != nil {
			t.Fatal(err)
		}
		stdoutWriter, _, closeLog, err := pipelineLogWriters(vault, workerConfig{ExecutionID: "lwc237-cancel"}, nil, false)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := stdoutWriter.Write([]byte("before-cancellation\n")); err != nil {
			t.Fatal(err)
		}
		line := bytes.TrimSpace(stdout.Bytes())
		var record map[string]any
		if err := json.Unmarshal(line, &record); err != nil || record["event"] != "child_output" {
			t.Fatalf("container output=%q record=%v err=%v", line, record, err)
		}
		if got := reconstructLWC237Output(t, record); string(got) != "before-cancellation\n" {
			t.Fatalf("container payload=%q, want before-cancellation\n", got)
		}
		if err := closeLog(); err != nil {
			t.Fatal(err)
		}
	})
}

func TestLWC237LiveOutputExceedsDurableArtifactBoundLosslessly(t *testing.T) {
	input := bytes.Repeat([]byte("x"), maxPipelineLog+1024)
	var stdout, stderr, control bytes.Buffer
	withPipelineLogDestinations(t, &stdout, &stderr, &control, func() {
		vault := t.TempDir()
		if err := os.MkdirAll(filepath.Join(vault, "cache"), 0o755); err != nil {
			t.Fatal(err)
		}
		stdoutWriter, _, closeLog, err := pipelineLogWriters(vault, workerConfig{ExecutionID: "lwc237-large"}, nil, false)
		if err != nil {
			t.Fatal(err)
		}
		input := bytes.Repeat([]byte("x"), maxPipelineLog+1024)
		if _, err := stdoutWriter.Write(input); err != nil {
			t.Fatal(err)
		}
		if err := closeLog(); err != nil {
			t.Fatal(err)
		}
	})
	data := stdout.Bytes()
	if len(data) <= maxPipelineLog {
		t.Fatalf("live output length=%d, want > durable bound %d", len(data), maxPipelineLog)
	}
	var reconstructed strings.Builder
	for _, line := range bytes.Split(bytes.TrimSpace(data), []byte("\n")) {
		var record map[string]any
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatalf("live fragment is not JSON: %v", err)
		}
		if got := int(record["output_bytes"].(float64)); got > liveFrameBytes {
			t.Fatalf("fragment bytes=%d, want <= %d", got, liveFrameBytes)
		}
		reconstructed.Write(reconstructLWC237Output(t, record))
	}
	if reconstructed.String() != string(input) {
		t.Fatalf("live reconstruction length=%d, want %d", reconstructed.Len(), len(input))
	}
}

func TestLWC237PipelineExactCapThenOverflowRetainsMarker(t *testing.T) {
	var durable bytes.Buffer
	sink := newDiagnosticSink([]io.Writer{&durable}, nil)
	pipeline := newLivePipeline(sink, nil, workerConfig{}, nil)
	if _, err := pipeline.writer(stdoutStream).Write(bytes.Repeat([]byte{'x'}, maxPipelineLog)); err != nil {
		t.Fatal(err)
	}
	if _, err := pipeline.writer(stderrStream).Write([]byte("late-overflow")); err != nil {
		t.Fatal(err)
	}
	if err := pipeline.Close(); err != nil {
		t.Fatal(err)
	}
	if len(durable.Bytes()) != maxPipelineLog || !bytes.HasSuffix(durable.Bytes(), []byte(pipelineLogTruncationMarker)) {
		t.Fatalf("durable length=%d suffix=%q, want cap and marker", durable.Len(), durable.Bytes()[maxPipelineLog-len(pipelineLogTruncationMarker):])
	}
}

func TestLWC237SharedDurableSinkPreservesCrossStreamOrder(t *testing.T) {
	var durable bytes.Buffer
	sink := newDiagnosticSink([]io.Writer{&durable}, []string{"api-secret"})
	pipeline := newLivePipeline(sink, nil, workerConfig{}, []string{"api-secret"})
	if _, err := pipeline.writer(stdoutStream).Write([]byte("api-")); err != nil {
		t.Fatal(err)
	}
	if _, err := pipeline.writer(stderrStream).Write([]byte("secretB")); err != nil {
		t.Fatal(err)
	}
	if err := pipeline.Close(); err != nil {
		t.Fatal(err)
	}
	if got, want := durable.String(), "[REDACTED]B"; got != want {
		t.Fatalf("durable=%q, want %q", got, want)
	}
}

func TestLWC237FramingMetadataIsTruthfulAndImmediate(t *testing.T) {
	var live bytes.Buffer
	pipeline := newLivePipeline(nil, map[pipelineStream]io.Writer{stdoutStream: &live}, workerConfig{}, nil)
	if _, err := pipeline.writer(stdoutStream).Write([]byte("short")); err != nil {
		t.Fatal(err)
	}
	if live.Len() == 0 {
		t.Fatal("short no-newline output was buffered until Close")
	}
	var immediate map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(live.Bytes()), &immediate); err != nil {
		t.Fatal(err)
	}
	if immediate["event"] != "child_output" || immediate["fragment_final"] != true || immediate["fragmented"] != false || immediate["newline"] != false {
		t.Fatalf("immediate metadata=%v, want final unfragmented no-newline frame", immediate)
	}
	live.Reset()
	input := bytes.Repeat([]byte{'f'}, liveFrameBytes+1)
	if _, err := pipeline.writer(stdoutStream).Write(input); err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimSpace(live.Bytes()), []byte("\n"))
	if len(lines) != 2 {
		t.Fatalf("got %d fragments, want 2: %q", len(lines), live.Bytes())
	}
	var frame uint64
	for index, line := range lines {
		var record map[string]any
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatal(err)
		}
		if index == 0 {
			frame = uint64(record["frame_id"].(float64))
		}
		if got := uint64(record["frame_id"].(float64)); got != frame {
			t.Fatalf("fragment frame=%d, want %d", got, frame)
		}
		if got := int(record["fragment_index"].(float64)); got != index {
			t.Fatalf("fragment index=%d, want %d", got, index)
		}
		if record["fragmented"] != true || record["newline"] != false {
			t.Fatalf("fragment metadata=%v, want fragmented no-newline", record)
		}
		wantFinal := index == len(lines)-1
		if record["fragment_final"] != wantFinal {
			t.Fatalf("fragment_final=%v, want %v", record["fragment_final"], wantFinal)
		}
	}
	if err := pipeline.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestLWC237InvalidUTF8OutputReconstructsLosslessly(t *testing.T) {
	var live bytes.Buffer
	pipeline := newLivePipeline(nil, map[pipelineStream]io.Writer{stdoutStream: &live}, workerConfig{}, nil)
	want := []byte{0xff, 0xfe, 'x'}
	if _, err := pipeline.writer(stdoutStream).Write(want); err != nil {
		t.Fatal(err)
	}
	if err := pipeline.Close(); err != nil {
		t.Fatal(err)
	}
	var record map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(live.Bytes()), &record); err != nil {
		t.Fatal(err)
	}
	if record["event"] != "child_output" || record["output_encoding"] != "base64" {
		t.Fatalf("record=%v, want event=child_output/output_encoding=base64", record)
	}
	if got := reconstructLWC237Output(t, record); !bytes.Equal(got, want) {
		t.Fatalf("reconstructed=%v, want %v", got, want)
	}
}

func TestLWC237UTF8FrameMessageEqualsSafePayloadAndEventStaysChildOutput(t *testing.T) {
	var live bytes.Buffer
	pipeline := newLivePipeline(nil, map[pipelineStream]io.Writer{
		stdoutStream: &live,
	}, workerConfig{}, nil)
	input := bytes.Repeat([]byte("abcdef"), 2048)
	if _, err := pipeline.writer(stdoutStream).Write(input); err != nil {
		t.Fatal(err)
	}
	if err := pipeline.Close(); err != nil {
		t.Fatal(err)
	}
	var reconstructed strings.Builder
	for _, record := range parseLWC237JSONRecords(t, live.Bytes()) {
		if got := record["event"]; got != "child_output" {
			t.Fatalf("record=%v, want event=child_output", record)
		}
		output, ok := record["output"].(string)
		if !ok {
			t.Fatalf("record=%v, want output string", record)
		}
		if got := record["output_encoding"]; got != "utf-8" {
			t.Fatalf("record=%v, want output_encoding=utf-8", record)
		}
		if got := record["message"]; got != output {
			t.Fatalf("record=%v, want message=%q output=%q", record, output, output)
		}
		reconstructed.Write(reconstructLWC237Output(t, record))
	}
	if got, want := reconstructed.String(), string(input); got != want {
		t.Fatalf("reconstructed=%q, want %q", got, want)
	}
}

func TestLWC237StructuredIdentityAndCredentialRedaction(t *testing.T) {
	var stdout, stderr, control bytes.Buffer
	withPipelineLogDestinations(t, &stdout, &stderr, &control, func() {
		vault := t.TempDir()
		if err := os.MkdirAll(filepath.Join(vault, "cache"), 0o755); err != nil {
			t.Fatal(err)
		}
		stdoutWriter, _, closeLog, err := pipelineLogWriters(vault, workerConfig{
			UserID: "user-visible", ProjectID: "project-visible", ExecutionID: "execution-visible", APIKey: "secret-token",
		}, nil, false)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := stdoutWriter.Write([]byte("api-secret-")); err != nil {
			t.Fatal(err)
		}
		if _, err := stdoutWriter.Write([]byte("token /workspace/visible\n")); err != nil {
			t.Fatal(err)
		}
		if err := closeLog(); err != nil {
			t.Fatal(err)
		}
	})
	data := stdout.Bytes()
	if strings.Contains(string(data), "secret-token") {
		t.Fatalf("credential leaked: %q", data)
	}
	var messages strings.Builder
	for _, line := range bytes.Split(bytes.TrimSpace(data), []byte("\n")) {
		var record map[string]any
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatalf("live output is not JSON: %v; output=%q", err, data)
		}
		for key, want := range map[string]string{
			"component": "olw_worker", "child_component": "synto", "user_id": "user-visible", "project_id": "project-visible", "execution_id": "execution-visible",
			"stream": "stdout", "severity": "INFO",
		} {
			if got := record[key]; got != want {
				t.Fatalf("record[%q]=%v, want %q; record=%v", key, got, want, record)
			}
		}
		if _, ok := record["sequence"]; !ok {
			t.Fatalf("record missing sequence: %v", record)
		}
		if record["event"] != "child_output" {
			t.Fatalf("record=%v, want event=child_output", record)
		}
		messages.Write(reconstructLWC237Output(t, record))
	}
	if got, want := messages.String(), "api-[REDACTED] /workspace/visible\n"; got != want {
		t.Fatalf("message=%q, want %q", got, want)
	}
}

func TestLWC237DurableFailureDoesNotAffectHealthyLiveOutput(t *testing.T) {
	var control, live bytes.Buffer
	secret := "secret-token"
	pipeline := newLivePipeline(&countingFailingWriter{err: errors.New("durable write failed: " + secret)}, map[pipelineStream]io.Writer{
		stdoutStream: &live,
	}, workerConfig{}, []string{secret})
	pipeline.degradedWriter = slog.NewJSONHandler(&control, &slog.HandlerOptions{ReplaceAttr: cloudLoggingAttr})
	if _, err := pipeline.writer(stdoutStream).Write([]byte("durable write fails first line\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := pipeline.writer(stdoutStream).Write([]byte("durable write keeps live second\n")); err != nil {
		t.Fatal(err)
	}
	if err := pipeline.Close(); err != nil {
		t.Fatalf("pipeline close=%v, want nil", err)
	}
	lines := parseLWC237OutputLines(t, live.Bytes())
	if len(lines) != 2 {
		t.Fatalf("child output frames=%d, want 2", len(lines))
	}
	var payload strings.Builder
	for _, record := range lines {
		if got := record["event"]; got != "child_output" {
			t.Fatalf("record=%v, want event=child_output", record)
		}
		payload.Write(reconstructLWC237Output(t, record))
	}
	if got := payload.String(); got != "durable write fails first line\ndurable write keeps live second\n" {
		t.Fatalf("live output=%q", got)
	}
	warnings := parseLWC237JSONRecords(t, control.Bytes())
	if len(warnings) != 1 {
		t.Fatalf("warning count=%d, want 1", len(warnings))
	}
	if warnings[0]["message"] != "observability_degraded" {
		t.Fatalf("warning=%v, want observability_degraded", warnings[0])
	}
	if cause, ok := warnings[0]["cause"].(string); !ok || strings.Contains(cause, secret) {
		t.Fatalf("warning cause leaked secret: %v", warnings[0])
	}
}

func TestLWC237DurableCloseFailureWarnsOnce(t *testing.T) {
	var control, live bytes.Buffer
	secret := "secret-token"
	durable := &bytes.Buffer{}
	pipeline := newLivePipeline(durable, map[pipelineStream]io.Writer{
		stdoutStream: &live,
	}, workerConfig{}, []string{secret})
	pipeline.durableClose = func() error {
		return errors.New("durable close failed: " + secret)
	}
	pipeline.degradedWriter = slog.NewJSONHandler(&control, &slog.HandlerOptions{ReplaceAttr: cloudLoggingAttr})
	if _, err := pipeline.writer(stdoutStream).Write([]byte("close-output\n")); err != nil {
		t.Fatal(err)
	}
	if err := pipeline.Close(); err != nil {
		t.Fatalf("pipeline close=%v, want nil", err)
	}
	pipeline.recordDegraded("durable", errors.New("durable first"))
	pipeline.recordDegraded("durable", errors.New("durable second"))
	if len(parseLWC237OutputLines(t, live.Bytes())) != 1 {
		t.Fatalf("live output missing expected child frame")
	}
	if got := parseLWC237JSONRecords(t, control.Bytes()); len(got) != 1 {
		t.Fatalf("warning count=%d, want 1", len(got))
	}
}

func TestLWC237SuppressOutputStillEmitsWorkerOwnedWarn(t *testing.T) {
	var stdout bytes.Buffer
	vault := t.TempDir()
	if err := os.MkdirAll(filepath.Join(vault, "cache"), 0o755); err != nil {
		t.Fatal(err)
	}
	var control bytes.Buffer
	oldDurable := pipelineDurableDestination
	oldControl := pipelineControlDestination
	t.Cleanup(func() {
		pipelineDurableDestination = oldDurable
		pipelineControlDestination = oldControl
	})
	pipelineDurableDestination = func(_ *os.File) io.Writer {
		return &countingFailingWriter{err: errors.New("durable path secret-token")}
	}
	pipelineControlDestination = func() io.Writer { return &control }
	stdoutWriter, _, closeLog, err := pipelineLogWriters(vault, workerConfig{
		ExecutionID: "lwc237-suppressed", APIKey: "secret-token",
	}, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stdoutWriter.Write([]byte("durable-only")); err != nil {
		t.Fatal(err)
	}
	if err := closeLog(); err != nil {
		t.Fatal(err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("suppress-output should omit raw child output, got %q", stdout.String())
	}
	warnings := parseLWC237JSONRecords(t, control.Bytes())
	if len(warnings) != 1 || warnings[0]["message"] != "observability_degraded" {
		t.Fatalf("warning=%v, want exactly one observability_degraded", warnings)
	}
	if stderr := control.String(); strings.Contains(stderr, "durable-only") {
		t.Fatalf("worker-owned warn should contain worker text only: %s", stderr)
	}
}

func TestLWC237OrdinaryBusinessErrorNotMaskedByLoggingFailure(t *testing.T) {
	oldExec := execOLW
	oldLive := pipelineLiveDestination
	oldDurable := pipelineDurableDestination
	oldControl := pipelineControlDestination
	t.Cleanup(func() {
		execOLW = oldExec
		pipelineLiveDestination = oldLive
		pipelineDurableDestination = oldDurable
		pipelineControlDestination = oldControl
	})
	var control bytes.Buffer
	var warnCount int
	pipelineControlDestination = func() io.Writer { return &control }
	pipelineLiveDestination = func(_ pipelineStream) io.Writer { return failingWriter{err: errors.New("child output failed")} }
	pipelineDurableDestination = func(_ *os.File) io.Writer {
		return failingWriter{err: errors.New("durable output failed secret-token")}
	}
	businessErr := errors.New("child business failed")
	execOLW = func(_ context.Context, _ string, _ []string, _ []string, stdout, _ io.Writer) error {
		_, _ = stdout.Write([]byte("dual-evidence-child-output"))
		return businessErr
	}
	m := newMemoryObjects()
	prefix := "users/user/projects/project/"
	seedCloudSource(t, m, prefix, "raw", "", priorCloudReceipt())
	cfg := cloudCfgFor("user", "project", "execution-dual")
	cfg.SuppressOutput = false
	err := runCloudWorkerBatch(context.Background(), cfg, [][]string{{"run"}}, m)
	if err == nil || !errors.Is(err, businessErr) {
		t.Fatalf("error=%v, want business error", err)
	}
	if _, _, readErr := m.Read(context.Background(), prefix+generation.ManifestPath, 0, generation.MaxManifestBytes); !errors.Is(readErr, cloudstorage.ErrObjectNotExist) {
		t.Fatalf("publication committed after business failure: %v", readErr)
	}
	for _, record := range parseLWC237JSONRecords(t, control.Bytes()) {
		if record["message"] == "observability_degraded" {
			warnCount++
		}
	}
	if warnCount < 1 {
		t.Fatalf("no degradation warning recorded: %v", control.String())
	}
}

func withPipelineLogDestinations(t *testing.T, stdout, stderr, control io.Writer, fn func()) {
	t.Helper()
	oldLive := pipelineLiveDestination
	oldControl := pipelineControlDestination
	oldDurable := pipelineDurableDestination
	defer func() {
		pipelineLiveDestination = oldLive
		pipelineControlDestination = oldControl
		pipelineDurableDestination = oldDurable
	}()
	pipelineLiveDestination = func(stream pipelineStream) io.Writer {
		if stream == stderrStream {
			return stderr
		}
		return stdout
	}
	pipelineControlDestination = func() io.Writer { return control }
	fn()
}

func captureWorkerStdout(t *testing.T) (*os.File, *os.File, func()) {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stdout
	os.Stdout = writer
	return reader, writer, func() { os.Stdout = original; _ = writer.Close(); _ = reader.Close() }
}

func captureWorkerStderr(t *testing.T) (*os.File, *os.File, func()) {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stderr
	os.Stderr = writer
	return reader, writer, func() { os.Stderr = original; _ = writer.Close(); _ = reader.Close() }
}

func assertLWC237JSONStream(t *testing.T, data []byte, wantMessage, wantSeverity, wantStream string) {
	t.Helper()
	var messages strings.Builder
	for _, line := range bytes.Split(bytes.TrimSpace(data), []byte("\n")) {
		var record map[string]any
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatalf("output line=%q is not JSON: %v", line, err)
		}
		if record["severity"] != wantSeverity || record["stream"] != wantStream {
			t.Fatalf("record=%v, want severity=%q stream=%q", record, wantSeverity, wantStream)
		}
		if record["event"] != "child_output" {
			t.Fatalf("record event=%v, want child_output", record["event"])
		}
		messages.Write(reconstructLWC237Output(t, record))
	}
	if messages.String() != wantMessage {
		t.Fatalf("messages=%q, want %q", messages.String(), wantMessage)
	}
}

func reconstructLWC237Output(t *testing.T, record map[string]any) []byte {
	t.Helper()
	output, ok := record["output"].(string)
	if !ok {
		t.Fatalf("record missing output: %v", record)
	}
	switch record["output_encoding"] {
	case "utf-8":
		return []byte(output)
	case "base64":
		decoded, err := base64.StdEncoding.DecodeString(output)
		if err != nil {
			t.Fatalf("invalid base64 output %q: %v", output, err)
		}
		return decoded
	default:
		t.Fatalf("unknown output_encoding=%v", record["output_encoding"])
		return nil
	}
}

func parseLWC237JSONRecords(t *testing.T, data []byte) []map[string]any {
	t.Helper()
	lines := bytes.Split(bytes.TrimSpace(data), []byte("\n"))
	if len(lines) == 1 && len(lines[0]) == 0 {
		return nil
	}
	records := make([]map[string]any, 0, len(lines))
	for _, line := range lines {
		var record map[string]any
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatalf("output line=%q is not JSON: %v", line, err)
		}
		records = append(records, record)
	}
	return records
}

func parseLWC237OutputLines(t *testing.T, data []byte) []map[string]any {
	return parseLWC237JSONRecords(t, data)
}

type failingWriter struct{ err error }

func (w failingWriter) Write([]byte) (int, error) { return 0, w.err }

type countingFailingWriter struct {
	err    error
	writes int
}

func (w *countingFailingWriter) Write([]byte) (int, error) {
	w.writes++
	return 0, w.err
}

type shortWriteWriter struct {
	writes int
}

func (w *shortWriteWriter) Write(p []byte) (int, error) {
	w.writes++
	if len(p) == 0 {
		return 0, nil
	}
	return len(p) - 1, nil
}

type failingOnNthWriteWriter struct {
	FailOnWrite int
	Err         error
	writes      int
}

func (w *failingOnNthWriteWriter) Write(p []byte) (int, error) {
	w.writes++
	if w.FailOnWrite > 0 && w.writes >= w.FailOnWrite {
		return 0, w.Err
	}
	return len(p), nil
}
