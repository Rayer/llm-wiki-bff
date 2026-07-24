package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
)

type failureStage string

const (
	failureStageInputMaterialization     failureStage = "input_materialization"
	failureStageSyntoMigration           failureStage = "synto_migration"
	failureStageSyntoConfigNormalization failureStage = "synto_config_normalization"
	failureStageSyntoConfigValidation    failureStage = "synto_config_validation"
	failureStageSyntoRun                 failureStage = "synto_run"
	failureStageSyntoIndexExport         failureStage = "synto_index_export"
	failureStageSourceReconciliation     failureStage = "source_reconciliation"
	failureStageConceptReconciliation    failureStage = "concept_reconciliation"
	failureStagePostprocess              failureStage = "postprocess"
	failureStageGenerationPublish        failureStage = "generation_publish"
	failureStageReceiptRecording         failureStage = "receipt_recording"
	failureStageLeaseCleanup             failureStage = "lease_cleanup"
	failureStageUnknown                  failureStage = "unknown"
)

type failureErrorClass string

const (
	failureClassValidation       failureErrorClass = "validation"
	failureClassChildExit        failureErrorClass = "child_exit"
	failureClassTimeout          failureErrorClass = "timeout"
	failureClassCancelled        failureErrorClass = "cancelled"
	failureClassIO               failureErrorClass = "io"
	failureClassStateInvalid     failureErrorClass = "state_invalid"
	failureClassPublishConflict  failureErrorClass = "publish_conflict"
	failureClassRecordingFailure failureErrorClass = "recording_failure"
	failureClassUnknown          failureErrorClass = "unknown"
)

type failureChildCommand string

const (
	failureChildMigrateOLW failureChildCommand = "migrate-olw"
	failureChildRun        failureChildCommand = "run"
	failureChildPackExport failureChildCommand = "pack-export"
)

var (
	knownFailureStages = map[failureStage]struct{}{
		failureStageInputMaterialization: {}, failureStageSyntoMigration: {},
		failureStageSyntoConfigNormalization: {}, failureStageSyntoConfigValidation: {},
		failureStageSyntoRun: {}, failureStageSyntoIndexExport: {},
		failureStageSourceReconciliation: {}, failureStageConceptReconciliation: {},
		failureStagePostprocess:       {},
		failureStageGenerationPublish: {}, failureStageReceiptRecording: {},
		failureStageLeaseCleanup: {}, failureStageUnknown: {},
	}
	knownFailureClasses = map[failureErrorClass]struct{}{
		failureClassValidation: {}, failureClassChildExit: {}, failureClassTimeout: {},
		failureClassCancelled: {}, failureClassIO: {}, failureClassStateInvalid: {},
		failureClassPublishConflict: {}, failureClassRecordingFailure: {},
		failureClassUnknown: {},
	}
	knownFailureChildren = map[failureChildCommand]struct{}{
		failureChildMigrateOLW: {}, failureChildRun: {}, failureChildPackExport: {},
	}
)

// workerFailure keeps the production boundary's finite diagnostic facts while
// preserving the original error for internal errors.Is/errors.As callers.
type workerFailure struct {
	cause    error
	Stage    failureStage
	Class    failureErrorClass
	Child    failureChildCommand
	ExitCode *int
}

const maxWorkerExitCode = 255

func (e *workerFailure) Error() string {
	if e == nil || e.cause == nil {
		return "worker failure"
	}
	return e.cause.Error()
}
func (e *workerFailure) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func newWorkerFailure(ctx context.Context, stage failureStage, class failureErrorClass, child failureChildCommand, err error) error {
	if err == nil {
		return nil
	}
	var exitErr *exec.ExitError
	hasExitError := child != "" && errors.As(err, &exitErr)
	if hasExitError && exitErr == nil {
		err = errors.New("child process failed")
	}
	contextClass := failureErrorClass("")
	causeContextClass := failureErrorClass("")
	if ctx != nil {
		switch ctx.Err() {
		case context.DeadlineExceeded:
			contextClass = failureClassTimeout
		case context.Canceled:
			contextClass = failureClassCancelled
		}
	}
	if contextClass != "" {
		class = contextClass
	} else if errors.Is(err, context.DeadlineExceeded) {
		class = failureClassTimeout
		causeContextClass = failureClassTimeout
	} else if errors.Is(err, context.Canceled) {
		class = failureClassCancelled
		causeContextClass = failureClassCancelled
	}
	var exitCode *int
	if child != "" {
		if !hasExitError || exitErr == nil {
			if contextClass == "" && causeContextClass == "" {
				class = failureClassUnknown
			}
		} else {
			code := exitErr.ExitCode()
			if code >= 0 && code <= maxWorkerExitCode {
				exitCode = &code
			}
			if class != failureClassTimeout && class != failureClassCancelled {
				class = failureClassChildExit
			}
		}
	}
	return &workerFailure{cause: err, Stage: stage, Class: class, Child: child, ExitCode: exitCode}
}

func preserveWorkerFailure(err error, stage failureStage, class failureErrorClass) error {
	if err == nil {
		return nil
	}
	var failure *workerFailure
	if errors.As(err, &failure) && failure != nil {
		return err
	}
	return newWorkerFailure(nil, stage, class, "", err)
}

type failureDiagnostic struct {
	Version    int                 `json:"version"`
	Status     string              `json:"status"`
	Stage      failureStage        `json:"stage"`
	ErrorClass failureErrorClass   `json:"error_class"`
	Child      failureChildCommand `json:"child_command,omitempty"`
	ExitCode   *int                `json:"exit_code,omitempty"`
}

func diagnosticForError(err error) failureDiagnostic {
	diagnostic := failureDiagnostic{
		Version:    1,
		Status:     "failed",
		Stage:      failureStageUnknown,
		ErrorClass: failureClassUnknown,
	}
	var failure *workerFailure
	if !errors.As(err, &failure) || failure == nil {
		return diagnostic
	}
	if _, ok := knownFailureStages[failure.Stage]; ok {
		diagnostic.Stage = failure.Stage
	}
	if _, ok := knownFailureClasses[failure.Class]; ok {
		diagnostic.ErrorClass = failure.Class
	}
	if _, ok := knownFailureChildren[failure.Child]; ok {
		diagnostic.Child = failure.Child
		if failure.ExitCode != nil && *failure.ExitCode >= 0 && *failure.ExitCode <= maxWorkerExitCode {
			code := *failure.ExitCode
			diagnostic.ExitCode = &code
		}
	}
	return diagnostic
}

func decodeFailureDiagnostic(data []byte) (failureDiagnostic, error) {
	var diagnostic failureDiagnostic
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&diagnostic); err != nil {
		return failureDiagnostic{}, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return failureDiagnostic{}, errors.New("failure diagnostic has trailing data")
		}
		return failureDiagnostic{}, err
	}
	if diagnostic.Version != 1 || diagnostic.Status != "failed" {
		return failureDiagnostic{}, errors.New("invalid failure diagnostic version or status")
	}
	if _, ok := knownFailureStages[diagnostic.Stage]; !ok {
		return failureDiagnostic{}, errors.New("invalid failure diagnostic stage")
	}
	if _, ok := knownFailureClasses[diagnostic.ErrorClass]; !ok {
		return failureDiagnostic{}, errors.New("invalid failure diagnostic error class")
	}
	if diagnostic.Child != "" {
		if _, ok := knownFailureChildren[diagnostic.Child]; !ok {
			return failureDiagnostic{}, errors.New("invalid failure diagnostic child command")
		}
	} else if diagnostic.ExitCode != nil {
		return failureDiagnostic{}, errors.New("failure diagnostic exit code has no child command")
	}
	if diagnostic.ExitCode != nil && (*diagnostic.ExitCode < 0 || *diagnostic.ExitCode > maxWorkerExitCode) {
		return failureDiagnostic{}, errors.New("invalid failure diagnostic exit code")
	}
	return diagnostic, nil
}

func marshalFailureDiagnostic(err error) ([]byte, error) {
	data, err := json.Marshal(diagnosticForError(err))
	if err != nil {
		return nil, err
	}
	if len(data) > 4<<10 {
		return nil, errors.New("failure diagnostic exceeds size limit")
	}
	return data, nil
}
