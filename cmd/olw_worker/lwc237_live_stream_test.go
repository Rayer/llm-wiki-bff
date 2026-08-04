package main

import (
	"bytes"
	"context"
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

func TestLWC237LiveChildOutputIsRawTextPayloadCompatible(t *testing.T) {
	var stdout, stderr bytes.Buffer
	pipeline := newLivePipeline(nil, map[pipelineStream]io.Writer{
		stdoutStream: &stdout,
		stderrStream: &stderr,
	}, workerConfig{UserID: "user-1", ProjectID: "project-1", ExecutionID: "exec-1"}, nil)
	if _, err := pipeline.writer(stdoutStream).Write([]byte("hello\nworld\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := pipeline.writer(stderrStream).Write([]byte("warning\n")); err != nil {
		t.Fatal(err)
	}
	if err := pipeline.Close(); err != nil {
		t.Fatal(err)
	}

	if got := stdout.String(); got != "hello\nworld\n" {
		t.Fatalf("stdout=%q, want %q", got, "hello\nworld\n")
	}
	if got := stderr.String(); got != "warning\n" {
		t.Fatalf("stderr=%q, want %q", got, "warning\n")
	}
	for _, data := range []string{stdout.String(), stderr.String()} {
		if strings.Contains(data, "event=child_output") {
			t.Fatalf("unexpected live envelope in output=%q", data)
		}
	}
}

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
		if got := stdout.String(); got != "running-child-sentinel\n" {
			t.Fatalf("container output=%q, want sentinel", got)
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
		assertLWC237TextStream(t, stdout.Bytes(), "stdout-part-1-part-2\n")
		assertLWC237TextStream(t, stderr.Bytes(), "stderr-part-1-part-2\n")
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
		if got := stdout.String(); got != "before-cancellation\n" {
			t.Fatalf("container output=%q, want before-cancellation\n", got)
		}
		if err := closeLog(); err != nil {
			t.Fatal(err)
		}
	})
}

func TestLWC237LiveOutputExceedsDurableArtifactBoundLosslessly(t *testing.T) {
	input := bytes.Repeat([]byte("x"), maxPipelineLog+1024)
	sink := newDiagnosticSink([]io.Writer{&bytes.Buffer{}}, nil)
	out := &recordingChunkWriter{}
	pipeline := newLivePipeline(sink, map[pipelineStream]io.Writer{
		stdoutStream: out,
	}, workerConfig{}, nil)
	if _, err := pipeline.writer(stdoutStream).Write(input); err != nil {
		t.Fatal(err)
	}
	if err := pipeline.Close(); err != nil {
		t.Fatal(err)
	}
	data := out.Bytes()
	if len(data) <= maxPipelineLog {
		t.Fatalf("live output length=%d, want > durable bound %d", len(data), maxPipelineLog)
	}
	if !bytes.Equal(reconstructLWC237OutputData(t, data), input) {
		t.Fatalf("live reconstruction=%q, want=%q", data, input)
	}
	if len(out.writes) <= 1 {
		t.Fatalf("live chunk count=%d, want > 1", len(out.writes))
	}
	for _, size := range out.writes {
		if size > liveFrameBytes {
			t.Fatalf("live chunk size=%d, want <= %d", size, liveFrameBytes)
		}
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

func TestLWC237ImmediateAndLargeNoNewlineStreaming(t *testing.T) {
	out := &recordingChunkWriter{}
	pipeline := newLivePipeline(nil, map[pipelineStream]io.Writer{stdoutStream: out}, workerConfig{}, nil)
	if _, err := pipeline.writer(stdoutStream).Write([]byte("short")); err != nil {
		t.Fatal(err)
	}
	if out.Len() == 0 {
		t.Fatal("short no-newline output was buffered until Close")
	}
	if got := out.String(); got != "short" {
		t.Fatalf("immediate output=%q, want short", got)
	}
	out.writes = nil
	out.Reset()
	input := bytes.Repeat([]byte{'f'}, liveFrameBytes+1)
	if _, err := pipeline.writer(stdoutStream).Write(input); err != nil {
		t.Fatal(err)
	}
	if got := out.Len(); got <= liveFrameBytes {
		t.Fatalf("got output length=%d, want > %d", got, liveFrameBytes)
	}
	if len(out.writes) != 2 {
		t.Fatalf("got %d fragments, want 2", len(out.writes))
	}
	for _, size := range out.writes {
		if size > liveFrameBytes {
			t.Fatalf("fragment size=%d, want <= %d", size, liveFrameBytes)
		}
	}
	if got := reconstructLWC237OutputData(t, out.Bytes()); len(got) != liveFrameBytes+1 {
		t.Fatalf("reconstructed length=%d, want %d", len(got), liveFrameBytes+1)
	}
	for _, line := range strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n") {
		if len(line) > liveFrameBytes {
			t.Fatalf("physical line bytes=%d, want <= %d", len(line), liveFrameBytes)
		}
	}
	if err := pipeline.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestLWC237SmallWritesCanExceedLiveFrameWithoutNewline(t *testing.T) {
	out := &recordingChunkWriter{}
	pipeline := newLivePipeline(nil, map[pipelineStream]io.Writer{stdoutStream: out}, workerConfig{}, nil)
	for i := 0; i < liveFrameBytes+1; i++ {
		if _, err := pipeline.writer(stdoutStream).Write([]byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if len(lines) == 1 {
		t.Fatalf("physical lines are unbounded without continuation: first=%q", lines[0])
	}
	if len(lines[0]) > liveFrameBytes {
		t.Fatalf("physical line length=%d, want <=%d", len(lines[0]), liveFrameBytes)
	}
	if !strings.Contains(out.String(), lwc237ContinuationToken) {
		t.Fatalf("expected continuation marker in repeated no-newline writes")
	}
	if got := reconstructLWC237OutputData(t, out.Bytes()); len(got) != liveFrameBytes+1 {
		t.Fatalf("reconstructed length=%d, want=%d", len(got), liveFrameBytes+1)
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
	if got := reconstructLWC237OutputData(t, live.Bytes()); !bytes.Equal(got, want) {
		t.Fatalf("reconstructed=%v, want %v", got, want)
	}
	if got := live.String(); got == string(want) {
		t.Fatalf("invalid UTF-8 should be escaped, got raw bytes: %q", got)
	}
}

func TestLWC237InvalidUTF8EscapeIsInjective(t *testing.T) {
	inputs := [][]byte{
		[]byte("literal \\x80 and \\c marker"),
		{0x80, 'x', '8', '0', 0x80},
	}
	for _, input := range inputs {
		var live bytes.Buffer
		pipeline := newLivePipeline(nil, map[pipelineStream]io.Writer{stdoutStream: &live}, workerConfig{}, nil)
		if _, err := pipeline.writer(stdoutStream).Write(input); err != nil {
			t.Fatal(err)
		}
		if err := pipeline.Close(); err != nil {
			t.Fatal(err)
		}
		if got := reconstructLWC237OutputData(t, live.Bytes()); !bytes.Equal(got, input) {
			t.Fatalf("encoded=%q reconstructed=%v, want=%v", live.Bytes(), got, input)
		}
	}
}

func TestLWC237OrdinaryUTF8WithBackslashesAndLiteralsIsLossless(t *testing.T) {
	var live bytes.Buffer
	pipeline := newLivePipeline(nil, map[pipelineStream]io.Writer{
		stdoutStream: &live,
	}, workerConfig{}, nil)
	input := "first line with \"quoted\" and a literal backslash: \\\n" +
		"second line with windows path: C:\\tmp\\project\\src\\main.go\n" +
		"third line includes literal \\x80 and literal \\c sequences\n"
	inputBytes := []byte(input)
	if _, err := pipeline.writer(stdoutStream).Write(inputBytes[:len(inputBytes)/2]); err != nil {
		t.Fatal(err)
	}
	if _, err := pipeline.writer(stdoutStream).Write(inputBytes[len(inputBytes)/2:]); err != nil {
		t.Fatal(err)
	}
	if err := pipeline.Close(); err != nil {
		t.Fatal(err)
	}
	if got := reconstructLWC237OutputData(t, live.Bytes()); !bytes.Equal(got, inputBytes) {
		t.Fatalf("reconstructed=%q, want=%q", got, inputBytes)
	}
}

func TestLWC237InjectiveAcrossWritesForLiteralEscapeTokenInvalidBytesAndContinuation(t *testing.T) {
	var live bytes.Buffer
	pipeline := newLivePipeline(nil, map[pipelineStream]io.Writer{stdoutStream: &live}, workerConfig{}, nil)
	escape := []byte(lwc237EscapeToken)
	input := make([]byte, 0, liveFrameBytes+16+len(escape))
	input = append(input, bytes.Repeat([]byte{'a'}, liveFrameBytes-1)...)
	input = append(input, escape...)
	input = append(input, []byte(" mid x80 LATE")...)
	input = append(input, 0x80)
	input = append(input, []byte(" and done")...)
	input = append(input, bytes.Repeat([]byte{'b'}, 16)...)
	if _, err := pipeline.writer(stdoutStream).Write(input[:liveFrameBytes]); err != nil {
		t.Fatal(err)
	}
	if _, err := pipeline.writer(stdoutStream).Write(input[liveFrameBytes : liveFrameBytes+len(escape)-1]); err != nil {
		t.Fatal(err)
	}
	if _, err := pipeline.writer(stdoutStream).Write(input[liveFrameBytes+len(escape)-1:]); err != nil {
		t.Fatal(err)
	}
	if err := pipeline.Close(); err != nil {
		t.Fatal(err)
	}
	got := reconstructLWC237OutputData(t, live.Bytes())
	if !bytes.Equal(got, input) {
		t.Fatalf("reconstructed=%q, want=%q", got, input)
	}
	if !bytes.Contains(live.Bytes(), []byte(lwc237ContinuationToken)) {
		t.Fatalf("expected continuation marker in encoded output: %q", live.Bytes())
	}
	if !bytes.Contains(got, escape) {
		t.Fatalf("escape token text not preserved: %q", got)
	}
}

func TestLWC237ShortLiveWriteDisablesOnceAndWarnsOnce(t *testing.T) {
	var control bytes.Buffer
	withPipelineLogDestinations(t, nil, nil, &control, func() {
		out := &shortWriteErrorWriter{err: errors.New("short destination")}
		pipeline := newLivePipeline(nil, map[pipelineStream]io.Writer{stdoutStream: out}, workerConfig{APIKey: "secret"}, []string{"secret"})
		if _, err := pipeline.writer(stdoutStream).Write([]byte("secret output\n")); err != nil {
			t.Fatal(err)
		}
		if _, err := pipeline.writer(stdoutStream).Write([]byte("second\n")); err != nil {
			t.Fatal(err)
		}
		if out.writes != 1 {
			t.Fatalf("destination writes=%d, want 1", out.writes)
		}
		warnings := parseLWC237JSONRecords(t, control.Bytes())
		if len(warnings) != 1 || warnings[0]["destination"] != "live_stdout" {
			t.Fatalf("warnings=%v, want one live_stdout warning", warnings)
		}
		if strings.Contains(string(control.Bytes()), "secret destination") {
			t.Fatalf("credential leaked in warning=%q", control.Bytes())
		}
	})
}

func TestLWC237ShortLiveWriteWithoutErrorDisablesOnce(t *testing.T) {
	out := &shortWriteWriter{}
	pipeline := newLivePipeline(nil, map[pipelineStream]io.Writer{stdoutStream: out}, workerConfig{}, nil)
	if _, err := pipeline.writer(stdoutStream).Write([]byte("short write\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := pipeline.writer(stdoutStream).Write([]byte("must not retry\n")); err != nil {
		t.Fatal(err)
	}
	if out.writes != 1 {
		t.Fatalf("destination writes=%d, want 1", out.writes)
	}
}

func TestLWC237EncodedPhysicalLinesAreBoundedAndReversible(t *testing.T) {
	input := bytes.Repeat([]byte{0x80}, liveFrameBytes)
	var live bytes.Buffer
	pipeline := newLivePipeline(nil, map[pipelineStream]io.Writer{stdoutStream: &live}, workerConfig{}, nil)
	if _, err := pipeline.writer(stdoutStream).Write(input); err != nil {
		t.Fatal(err)
	}
	if err := pipeline.Close(); err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSuffix(live.String(), "\n"), "\n") {
		if len(line) > liveFrameBytes {
			t.Fatalf("physical line bytes=%d, want <= %d", len(line), liveFrameBytes)
		}
	}
	if got := reconstructLWC237OutputData(t, live.Bytes()); !bytes.Equal(got, input) {
		t.Fatalf("reconstructed length=%d, want %d", len(got), len(input))
	}
}

func TestLWC237UTF8RuneCrossingInternalChunkBoundaryStaysNatural(t *testing.T) {
	input := append(bytes.Repeat([]byte{'a'}, liveRedactionChunkBytes-1), []byte("界\n")...)
	var live bytes.Buffer
	pipeline := newLivePipeline(nil, map[pipelineStream]io.Writer{stdoutStream: &live}, workerConfig{}, nil)
	if _, err := pipeline.writer(stdoutStream).Write(input); err != nil {
		t.Fatal(err)
	}
	if err := pipeline.Close(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(live.String(), `\xE7`) || strings.Contains(live.String(), `\x95`) || strings.Contains(live.String(), `\x8C`) {
		t.Fatalf("live output escaped a valid rune: suffix=%q", live.String()[len(live.String())-20:])
	}
	if got := reconstructLWC237OutputData(t, live.Bytes()); !bytes.Equal(got, input) {
		t.Fatalf("reconstructed output differs at rune boundary")
	}
}

func TestLWC237SecretsSplitAcrossWritesAndChunksOnBothStreams(t *testing.T) {
	secret := strings.Repeat("s", liveRedactionChunkBytes+17)
	var stdout, stderr bytes.Buffer
	pipeline := newLivePipeline(nil, map[pipelineStream]io.Writer{stdoutStream: &stdout, stderrStream: &stderr}, workerConfig{}, []string{secret})
	for _, stream := range []pipelineStream{stdoutStream, stderrStream} {
		writer := pipeline.writer(stream)
		if _, err := writer.Write([]byte("prefix " + secret[:liveRedactionChunkBytes-3])); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write([]byte(secret[liveRedactionChunkBytes-3:] + "\ntrailing")); err != nil {
			t.Fatal(err)
		}
	}
	if err := pipeline.Close(); err != nil {
		t.Fatal(err)
	}
	for stream, got := range map[pipelineStream][]byte{stdoutStream: stdout.Bytes(), stderrStream: stderr.Bytes()} {
		if bytes.Contains(got, []byte(secret)) || !bytes.Contains(got, []byte("[REDACTED]")) {
			t.Fatalf("%s output did not redact split secret: %q", stream, got)
		}
		want := append([]byte("prefix [REDACTED]\ntrailing"), 0)
		if reconstructed := reconstructLWC237OutputData(t, got); !bytes.Equal(reconstructed, want[:len(want)-1]) {
			t.Fatalf("%s reconstructed=%q, want=%q", stream, reconstructed, want[:len(want)-1])
		}
	}
}

func TestLWC237UTF8OutputPreservesText(t *testing.T) {
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
	reconstructed := reconstructLWC237OutputData(t, live.Bytes())
	if got, want := string(reconstructed), string(input); got != want {
		t.Fatalf("reconstructed=%q, want %q", got, want)
	}
}

func TestLWC237ParseLWC237TextLineQuotedBackslashQuote(t *testing.T) {
	var live bytes.Buffer
	pipeline := newLivePipeline(nil, map[pipelineStream]io.Writer{
		stdoutStream: &live,
	}, workerConfig{}, nil)
	input := []byte(`prefix "quoted" sequence`)
	if _, err := pipeline.writer(stdoutStream).Write(input); err != nil {
		t.Fatal(err)
	}
	if err := pipeline.Close(); err != nil {
		t.Fatal(err)
	}
	if got := reconstructLWC237OutputData(t, live.Bytes()); !bytes.Equal(got, input) {
		t.Fatalf("reconstructed=%q, want=%q", got, input)
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
	if got, want := string(reconstructLWC237OutputData(t, data)), "api-[REDACTED] /workspace/visible\n"; got != want {
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
	if got := string(reconstructLWC237OutputData(t, live.Bytes())); got != "durable write fails first line\ndurable write keeps live second\n" {
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
	if len(live.Bytes()) == 0 {
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

func assertLWC237TextStream(t *testing.T, data []byte, wantMessage string) {
	t.Helper()
	if got := string(reconstructLWC237OutputData(t, data)); got != wantMessage {
		t.Fatalf("messages=%q, want %q", got, wantMessage)
	}
}

func reconstructLWC237OutputData(t *testing.T, data []byte) []byte {
	t.Helper()
	encoded := data
	decoded := make([]byte, 0, len(encoded))
	escapeToken := []byte(lwc237EscapeToken)
	continuationToken := []byte(lwc237ContinuationToken)
	for i := 0; i < len(encoded); {
		if bytes.HasPrefix(encoded[i:], continuationToken) {
			i += len(continuationToken)
			continue
		}
		if !bytes.HasPrefix(encoded[i:], escapeToken) {
			decoded = append(decoded, encoded[i])
			i++
			continue
		}
		i += len(escapeToken)
		if i+len(escapeToken) <= len(encoded) && bytes.HasPrefix(encoded[i:], escapeToken) {
			decoded = append(decoded, escapeToken...)
			i += len(escapeToken)
			continue
		}
		if i < len(encoded) && encoded[i] == 'x' && i+2 < len(encoded) {
			high, ok := unhexDigit(encoded[i+1])
			if !ok {
				decoded = append(decoded, escapeToken...)
				i++
				continue
			}
			low, ok := unhexDigit(encoded[i+2])
			if !ok {
				decoded = append(decoded, escapeToken...)
				i++
				continue
			}
			decoded = append(decoded, high<<4|low)
			i += 3
			continue
		}
		decoded = append(decoded, escapeToken...)
		if i < len(encoded) {
			decoded = append(decoded, encoded[i])
			i++
		}
	}
	return decoded
}

func unhexDigit(ch byte) (byte, bool) {
	switch {
	case '0' <= ch && ch <= '9':
		return ch - '0', true
	case 'a' <= ch && ch <= 'f':
		return ch - 'a' + 10, true
	case 'A' <= ch && ch <= 'F':
		return ch - 'A' + 10, true
	default:
		return 0, false
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

type shortWriteErrorWriter struct {
	err    error
	writes int
}

func (w *shortWriteErrorWriter) Write(p []byte) (int, error) {
	w.writes++
	if len(p) == 0 {
		return 0, nil
	}
	return len(p) - 1, w.err
}

type failingOnNthWriteWriter struct {
	FailOnWrite int
	Err         error
	writes      int
}

type recordingChunkWriter struct {
	bytes.Buffer
	writes []int
}

func (w *recordingChunkWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	w.writes = append(w.writes, len(p))
	return w.Buffer.Write(p)
}

func (w *recordingChunkWriter) WriteString(s string) (int, error) {
	if len(s) == 0 {
		return 0, nil
	}
	w.writes = append(w.writes, len(s))
	return w.Buffer.WriteString(s)
}

func (w *failingOnNthWriteWriter) Write(p []byte) (int, error) {
	w.writes++
	if w.FailOnWrite > 0 && w.writes >= w.FailOnWrite {
		return 0, w.Err
	}
	return len(p), nil
}
