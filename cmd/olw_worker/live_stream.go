package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const liveFrameBytes = 16 << 10
const liveRedactionChunkBytes = 32 << 10

type pipelineStream string

const (
	stdoutStream pipelineStream = "stdout"
	stderrStream pipelineStream = "stderr"
)

type livePipeline struct {
	durable io.Writer
	live    map[pipelineStream]*liveDestination
	redact  map[pipelineStream]*streamRedactor
	secrets []string
	cfg     workerConfig

	durableDisabled bool
	durableClose    func() error
	degradedWriter  slog.Handler
	nextSequence    uint64
	nextFrame       uint64
	degraded        map[string]bool
	closed          bool
	mu              sync.Mutex
}

type liveDestination struct {
	writer   io.Writer
	disabled bool
}

func newLivePipeline(durable io.Writer, live map[pipelineStream]io.Writer, cfg workerConfig, secrets []string) *livePipeline {
	if durable != nil {
		if _, ok := durable.(*diagnosticSink); !ok {
			durable = newDiagnosticSink([]io.Writer{durable}, secrets)
		}
	}
	destinations := make(map[pipelineStream]*liveDestination, len(live))
	for stream, writer := range live {
		if writer == nil {
			continue
		}
		destinations[stream] = &liveDestination{
			writer: writer,
		}
	}
	redactors := map[pipelineStream]*streamRedactor{
		stdoutStream: newStreamRedactor(secrets),
		stderrStream: newStreamRedactor(secrets),
	}
	controlWriter := pipelineControlDestination()
	pipeline := &livePipeline{
		durable:  durable,
		live:     destinations,
		degraded: make(map[string]bool),
		redact:   redactors,
		secrets:  append([]string(nil), secrets...),
		cfg:      cfg,
	}
	if controlWriter != nil {
		pipeline.degradedWriter = slog.NewJSONHandler(controlWriter, &slog.HandlerOptions{ReplaceAttr: cloudLoggingAttr})
	}
	if sink, ok := durable.(*diagnosticSink); ok {
		pipeline.durableClose = sink.Close
	}
	return pipeline
}

func cloudLoggingAttr(_ []string, a slog.Attr) slog.Attr {
	switch a.Key {
	case slog.LevelKey:
		levelText := strings.ToUpper(a.Value.String())
		severity := "INFO"
		switch levelText {
		case "ERROR":
			severity = "ERROR"
		case "WARN":
			severity = "WARNING"
		case "DEBUG":
			severity = "DEBUG"
		}
		return slog.String("severity", severity)
	case slog.MessageKey:
		return slog.String("message", a.Value.String())
	default:
		return a
	}
}

func (p *livePipeline) writer(stream pipelineStream) io.Writer {
	return &pipelineStreamWriter{pipeline: p, stream: stream}
}

type pipelineStreamWriter struct {
	pipeline *livePipeline
	stream   pipelineStream
}

func (w *pipelineStreamWriter) Write(data []byte) (int, error) {
	if w == nil || w.pipeline == nil {
		return len(data), nil
	}
	return w.pipeline.write(w.stream, data)
}

func (p *livePipeline) write(stream pipelineStream, data []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return 0, errors.New("live pipeline closed")
	}
	if p.durable != nil && !p.durableDisabled {
		_, err := p.durable.Write(data)
		if err != nil {
			p.disableDurableLocked(fmt.Errorf("write durable pipeline log: %w", err))
		}
	}
	redactor := p.redact[stream]
	err := redactor.write(data, false, func(output []byte) error {
		if len(output) == 0 {
			return nil
		}
		p.nextFrame++
		return p.emitChildLocked(stream, p.nextFrame, output)
	})
	if err != nil {
		return len(data), nil
	}
	return len(data), nil
}

func (p *livePipeline) emitChildLocked(stream pipelineStream, frame uint64, data []byte) error {
	frameBytes := len(data)
	fragmented := frameBytes > liveFrameBytes
	fragmentIndex := 0
	for len(data) > 0 {
		n := len(data)
		if n > liveFrameBytes {
			n = liveFrameBytes
		}
		part := data[:n]
		if destination := p.live[stream]; destination != nil && !destination.disabled {
			if err := p.writeLiveLocked(stream, destination, part, frame, frameBytes, fragmented, fragmentIndex); err != nil {
				p.disableLiveLocked(stream, destination, err)
			}
		}
		data = data[n:]
		fragmentIndex++
	}
	return nil
}

func (p *livePipeline) writeLiveLocked(stream pipelineStream, destination *liveDestination, data []byte, frame uint64, frameBytes int, fragmented bool, fragmentIndex int) error {
	output, encoding := liveOutputMessage(data)
	p.nextSequence++
	line := strings.Join([]string{
		"event=child_output",
		"component=olw_worker",
		"child_component=synto",
		"user_id=" + strconv.Quote(p.cfg.UserID),
		"project_id=" + strconv.Quote(p.cfg.ProjectID),
		"execution_id=" + strconv.Quote(p.cfg.ExecutionID),
		"stream=" + strconv.Quote(string(stream)),
		"severity=" + strconv.Quote(liveSeverity(stream)),
		"sequence=" + strconv.FormatUint(p.nextSequence, 10),
		"frame_id=" + strconv.FormatUint(frame, 10),
		"fragment_index=" + strconv.Itoa(fragmentIndex),
		"fragment_final=" + strconv.FormatBool((fragmentIndex+1)*liveFrameBytes >= frameBytes),
		"fragmented=" + strconv.FormatBool(fragmented),
		"output_bytes=" + strconv.Itoa(len(data)),
		"output_encoding=" + strconv.Quote(encoding),
		"newline=" + strconv.FormatBool(bytes.HasSuffix(data, []byte{'\n'})),
		"output=" + strconv.Quote(output),
	}, " ") + "\n"
	if _, err := io.WriteString(destination.writer, line); err != nil {
		return fmt.Errorf("write live %s output: %w", stream, err)
	}
	return nil
}

func liveSeverity(stream pipelineStream) string {
	if stream == stderrStream {
		return "WARNING"
	}
	return "INFO"
}

func liveOutputMessage(data []byte) (string, string) {
	if utf8.Valid(data) {
		return string(data), "utf-8"
	}
	return base64.StdEncoding.EncodeToString(data), "base64"
}

func (p *livePipeline) disableDurableLocked(err error) {
	if p.durableDisabled {
		return
	}
	p.durableDisabled = true
	p.recordDegradedLocked("durable", err)
}

func (p *livePipeline) disableLiveLocked(stream pipelineStream, destination *liveDestination, err error) {
	if destination.disabled {
		return
	}
	destination.disabled = true
	p.recordDegradedLocked("live_"+string(stream), err)
}

func (p *livePipeline) recordDegraded(destination string, cause error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.recordDegradedLocked(destination, cause)
}

func (p *livePipeline) recordDegradedLocked(destination string, cause error) {
	if p.degraded[destination] {
		return
	}
	p.degraded[destination] = true
	causeText := string(redactDiagnosticBytes([]byte(cause.Error()), p.secrets))
	if p.degradedWriter == nil {
		return
	}
	p.nextSequence++
	record := slog.NewRecord(time.Now(), slog.LevelWarn, "observability_degraded", 0)
	record.AddAttrs(
		slog.String("component", "olw_worker"),
		slog.String("child_component", "synto"),
		slog.String("user_id", p.cfg.UserID),
		slog.String("project_id", p.cfg.ProjectID),
		slog.String("execution_id", p.cfg.ExecutionID),
		slog.String("stream", "stderr"),
		slog.Uint64("sequence", p.nextSequence),
		slog.String("event", "observability_degraded"),
		slog.String("destination", destination),
		slog.String("cause", causeText),
	)
	if err := p.degradedWriter.Handle(context.Background(), record); err != nil {
		p.degradedWriter = nil
	}
}

func (p *livePipeline) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil
	}
	for stream, redactor := range p.redact {
		_ = redactor.write(nil, true, func(data []byte) error {
			if len(data) == 0 {
				return nil
			}
			p.nextFrame++
			return p.emitChildLocked(stream, p.nextFrame, data)
		})
	}
	if p.durableClose != nil && !p.durableDisabled {
		if err := p.durableClose(); err != nil {
			p.disableDurableLocked(fmt.Errorf("close durable pipeline log: %w", err))
		}
	}
	p.closed = true
	return nil
}

type streamRedactor struct {
	secrets []string
	pending []byte
}

func newStreamRedactor(secrets []string) *streamRedactor {
	ordered := append([]string(nil), secrets...)
	sort.Slice(ordered, func(i, j int) bool { return len(ordered[i]) > len(ordered[j]) })
	return &streamRedactor{secrets: ordered}
}

func (r *streamRedactor) write(data []byte, final bool, emit func([]byte) error) error {
	for len(data) > 0 {
		n := len(data)
		if n > liveRedactionChunkBytes {
			n = liveRedactionChunkBytes
		}
		combined := make([]byte, 0, len(r.pending)+n)
		combined = append(combined, r.pending...)
		combined = append(combined, data[:n]...)
		r.pending = r.pending[:0]
		if err := r.process(combined, false, emit); err != nil {
			return err
		}
		data = data[n:]
	}
	if final && len(r.pending) > 0 {
		pending := append([]byte(nil), r.pending...)
		r.pending = r.pending[:0]
		return r.process(pending, true, emit)
	}
	return nil
}

func (r *streamRedactor) process(data []byte, final bool, emit func([]byte) error) error {
	for len(data) > 0 {
		index, secret := r.firstSecret(data)
		if index >= 0 {
			if err := emit(data[:index]); err != nil {
				return err
			}
			if err := emit([]byte("[REDACTED]")); err != nil {
				return err
			}
			data = data[index+len(secret):]
			continue
		}
		if final {
			if err := emit(data); err != nil {
				return err
			}
			return nil
		}
		keep := r.longestSecretPrefixSuffix(data)
		if keep == 0 {
			return emit(data)
		}
		cut := len(data) - keep
		if cut > 0 {
			if err := emit(data[:cut]); err != nil {
				return err
			}
		}
		r.pending = append(r.pending[:0], data[cut:]...)
		return nil
	}
	return nil
}

func (r *streamRedactor) firstSecret(data []byte) (int, string) {
	index := -1
	var found string
	for _, secret := range r.secrets {
		if secret == "" {
			continue
		}
		candidate := strings.Index(string(data), secret)
		if candidate >= 0 && (index < 0 || candidate < index || candidate == index && len(secret) > len(found)) {
			index, found = candidate, secret
		}
	}
	return index, found
}

func (r *streamRedactor) longestSecretPrefixSuffix(data []byte) int {
	best := 0
	for _, secret := range r.secrets {
		if secret == "" {
			continue
		}
		limit := len(secret) - 1
		if limit > len(data) {
			limit = len(data)
		}
		for n := limit; n > best; n-- {
			if strings.HasSuffix(string(data), secret[:n]) {
				best = n
				break
			}
		}
	}
	return best
}
