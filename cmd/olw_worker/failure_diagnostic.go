package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"strings"

	"github.com/rayer/llm-wiki-bff/internal/pipelinediagnostic"
)

type failureStage = pipelinediagnostic.Stage

const (
	failureStageInputMaterialization     = pipelinediagnostic.StageInputMaterialization
	failureStageSyntoMigration           = pipelinediagnostic.StageSyntoMigration
	failureStageSyntoConfigNormalization = pipelinediagnostic.StageSyntoConfigNormalization
	failureStageSyntoConfigValidation    = pipelinediagnostic.StageSyntoConfigValidation
	failureStageSyntoRun                 = pipelinediagnostic.StageSyntoRun
	failureStageSyntoIndexExport         = pipelinediagnostic.StageSyntoIndexExport
	failureStageSourceReconciliation     = pipelinediagnostic.StageSourceReconciliation
	failureStageConceptReconciliation    = pipelinediagnostic.StageConceptReconciliation
	failureStagePostprocess              = pipelinediagnostic.StagePostprocess
	failureStageGenerationPublish        = pipelinediagnostic.StageGenerationPublish
	failureStageReceiptRecording         = pipelinediagnostic.StageReceiptRecording
	failureStageLeaseCleanup             = pipelinediagnostic.StageLeaseCleanup
	failureStageUnknown                  = pipelinediagnostic.StageUnknown
)

type failureErrorClass = pipelinediagnostic.ErrorClass

const (
	failureClassValidation       = pipelinediagnostic.ErrorClassValidation
	failureClassChildExit        = pipelinediagnostic.ErrorClassChildExit
	failureClassTimeout          = pipelinediagnostic.ErrorClassTimeout
	failureClassCancelled        = pipelinediagnostic.ErrorClassCancellation
	failureClassIO               = pipelinediagnostic.ErrorClassIO
	failureClassStateInvalid     = pipelinediagnostic.ErrorClassStateInvalid
	failureClassPublishConflict  = pipelinediagnostic.ErrorClassPublishConflict
	failureClassRecordingFailure = pipelinediagnostic.ErrorClassRecordingFailure
	failureClassUnknown          = pipelinediagnostic.ErrorClassUnknown
)

type failureChildCommand = pipelinediagnostic.ChildCommand

const (
	failureChildMigrateOLW = pipelinediagnostic.ChildCommandMigrateOLW
	failureChildRun        = pipelinediagnostic.ChildCommandRun
	failureChildPackExport = pipelinediagnostic.ChildCommandPackExport
)

type conceptReconcileDetailCode = pipelinediagnostic.DetailCode

const (
	conceptDetailGeneratedMapReadDecode                 = pipelinediagnostic.DetailGeneratedMapReadDecode
	conceptDetailSyntoIndexTruth                        = pipelinediagnostic.DetailSyntoIndexTruth
	conceptDetailEntityMapping                          = pipelinediagnostic.DetailEntityMapping
	conceptDetailEntityMappingIndexTruth                = pipelinediagnostic.DetailEntityMappingIndexTruth
	conceptDetailEntityMappingSourceConceptIdentity     = pipelinediagnostic.DetailEntityMappingSourceConceptIdentity
	conceptDetailEntityMappingArticleIdentity           = pipelinediagnostic.DetailEntityMappingArticleIdentity
	conceptDetailEntityMappingArticlePath               = pipelinediagnostic.DetailEntityMappingArticlePath
	conceptDetailEntityMappingArticleSourceAmbiguity    = pipelinediagnostic.DetailEntityMappingArticleSourceAmbiguity
	conceptDetailEntityMappingArticleSourceMissing      = pipelinediagnostic.DetailEntityMappingArticleSourceMissing
	conceptDetailEntityMappingArticleSourceDisagreement = pipelinediagnostic.DetailEntityMappingArticleSourceDisagreement
	conceptDetailEntityMappingDuplicateArticleID        = pipelinediagnostic.DetailEntityMappingDuplicateArticleID
	conceptDetailEntityMappingDuplicateArticlePath      = pipelinediagnostic.DetailEntityMappingDuplicateArticlePath
	conceptDetailEntityMappingDuplicateEntityID         = pipelinediagnostic.DetailEntityMappingDuplicateEntityID
	conceptDetailEntityMappingActiveEntityUnknown       = pipelinediagnostic.DetailEntityMappingActiveEntityUnknown
	conceptDetailEntityMappingConceptSlugCase           = pipelinediagnostic.DetailEntityMappingConceptSlugCase
	conceptDetailEntityMappingConceptIDPathDisagreement = pipelinediagnostic.DetailEntityMappingConceptIDPathDisagreement
	conceptDetailEntityMappingConceptMissingMapping     = pipelinediagnostic.DetailEntityMappingConceptMissingMapping
	conceptDetailEntityMappingConceptEntityCollision    = pipelinediagnostic.DetailEntityMappingConceptEntityCollision
	conceptDetailEntityMerge                            = pipelinediagnostic.DetailEntityMerge
	conceptDetailIdentityReconciliation                 = pipelinediagnostic.DetailIdentityReconciliation
	conceptDetailLifecyclePlanning                      = pipelinediagnostic.DetailLifecyclePlanning
	conceptDetailConceptPageRewrite                     = pipelinediagnostic.DetailConceptPageRewrite
	conceptDetailLinkRewrite                            = pipelinediagnostic.DetailLinkRewrite
	conceptDetailCacheRewrite                           = pipelinediagnostic.DetailCacheRewrite
	conceptDetailArtifactWrite                          = pipelinediagnostic.DetailArtifactWrite
	conceptDetailArtifactRemove                         = pipelinediagnostic.DetailArtifactRemove
)

var knownConceptDetailCodes = pipelinediagnostic.ValidDetailCodes

type conceptReconciliationDetail interface {
	ConceptReconciliationDetail() conceptReconcileDetailCode
}

type conceptReconciliationFailure struct {
	detail conceptReconcileDetailCode
	cause  error
}

func (e *conceptReconciliationFailure) Error() string {
	if e == nil || e.cause == nil {
		return "concept reconciliation failed"
	}
	return e.cause.Error()
}

func (e *conceptReconciliationFailure) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func (e *conceptReconciliationFailure) ConceptReconciliationDetail() conceptReconcileDetailCode {
	if e == nil {
		return ""
	}
	return e.detail
}

func wrapConceptReconciliationError(detail conceptReconcileDetailCode, err error) error {
	if err == nil {
		return nil
	}
	var nestedDetail conceptReconciliationDetail
	if errors.As(err, &nestedDetail) && nestedDetail != nil {
		if code := nestedDetail.ConceptReconciliationDetail(); code != "" {
			if _, ok := knownConceptDetailCodes[code]; ok {
				return &conceptReconciliationFailure{detail: code, cause: err}
			}
		}
	}
	return &conceptReconciliationFailure{detail: detail, cause: err}
}

var (
	knownFailureStages   = pipelinediagnostic.ValidStages
	knownFailureClasses  = pipelinediagnostic.ValidErrorClasses
	knownFailureChildren = pipelinediagnostic.ValidChildCommands
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

const maxFailureDiagnosticMessage = 512

type failureDiagnostic struct {
	Version    int                        `json:"version"`
	Status     string                     `json:"status"`
	Stage      failureStage               `json:"stage"`
	ErrorClass failureErrorClass          `json:"error_class"`
	DetailCode conceptReconcileDetailCode `json:"detail_code,omitempty"`
	Child      failureChildCommand        `json:"child_command,omitempty"`
	ExitCode   *int                       `json:"exit_code,omitempty"`
	Execution  string                     `json:"execution,omitempty"`
	Message    string                     `json:"message,omitempty"`
}

func diagnosticForError(err error) failureDiagnostic {
	diagnostic := failureDiagnostic{
		Version:    1,
		Status:     "failed",
		Stage:      failureStageUnknown,
		ErrorClass: failureClassUnknown,
	}
	var failure *workerFailure
	if errors.As(err, &failure) && failure != nil {
		if _, ok := knownFailureStages[failure.Stage]; ok {
			diagnostic.Stage = failure.Stage
		}
		if _, ok := knownFailureClasses[failure.Class]; ok {
			diagnostic.ErrorClass = failure.Class
		}
	}
	var detail conceptReconciliationDetail
	if failure != nil && failure.Stage == failureStageConceptReconciliation && errors.As(err, &detail) && detail != nil {
		code := detail.ConceptReconciliationDetail()
		if _, ok := knownConceptDetailCodes[code]; ok {
			diagnostic.DetailCode = code
		}
	}
	if failure != nil {
		if _, ok := knownFailureChildren[failure.Child]; ok {
			diagnostic.Child = failure.Child
			if failure.ExitCode != nil && *failure.ExitCode >= 0 && *failure.ExitCode <= maxWorkerExitCode {
				code := *failure.ExitCode
				diagnostic.ExitCode = &code
			}
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
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return failureDiagnostic{}, err
	}
	if raw, ok := fields["detail_code"]; ok && bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return failureDiagnostic{}, errors.New("invalid failure diagnostic detail code")
	}
	if _, present := fields["detail_code"]; present {
		if diagnostic.DetailCode == "" || diagnostic.Stage != failureStageConceptReconciliation {
			return failureDiagnostic{}, errors.New("invalid failure diagnostic detail code")
		}
		if _, ok := knownConceptDetailCodes[diagnostic.DetailCode]; !ok {
			return failureDiagnostic{}, errors.New("invalid failure diagnostic detail code")
		}
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
	if raw, ok := fields["execution"]; ok {
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || diagnostic.Execution == "" || !validPipelineExecutionID(diagnostic.Execution) {
			return failureDiagnostic{}, errors.New("invalid failure diagnostic execution")
		}
	}
	if raw, ok := fields["message"]; ok {
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || diagnostic.Message == "" || len(diagnostic.Message) > maxFailureDiagnosticMessage {
			return failureDiagnostic{}, errors.New("invalid failure diagnostic message")
		}
	}
	return diagnostic, nil
}

func marshalFailureDiagnostic(err error) ([]byte, error) {
	return marshalFailureDiagnosticMeta(err, "", nil)
}

func marshalFailureDiagnosticMeta(err error, execution string, secrets []string) ([]byte, error) {
	diagnostic := diagnosticForError(err)
	if execution = strings.TrimSpace(execution); validPipelineExecutionID(execution) {
		diagnostic.Execution = execution
	}
	if message := diagnosticMessage(err, secrets); message != "" {
		diagnostic.Message = message
	}
	data, err := json.Marshal(diagnostic)
	if err != nil {
		return nil, err
	}
	if len(data) > 4<<10 {
		return nil, errors.New("failure diagnostic exceeds size limit")
	}
	return data, nil
}

func diagnosticMessage(err error, secrets []string) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	if msg == "" {
		return ""
	}
	msg = string(redactDiagnosticBytes([]byte(msg), secrets))
	if len(msg) > maxFailureDiagnosticMessage {
		msg = msg[:maxFailureDiagnosticMessage]
	}
	return msg
}
