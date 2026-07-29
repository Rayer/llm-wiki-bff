// Package pipelinediagnostic defines the worker/BFF diagnostic wire schema.
package pipelinediagnostic

const (
	MaxPipelineLogBytes         = 4 << 20
	PipelineLogTruncationMarker = "\n[output truncated at 4194304 bytes]\n"
)

type Stage string

const (
	StageInputMaterialization     Stage = "input_materialization"
	StageSyntoMigration           Stage = "synto_migration"
	StageSyntoConfigNormalization Stage = "synto_config_normalization"
	StageSyntoConfigValidation    Stage = "synto_config_validation"
	StageSyntoRun                 Stage = "synto_run"
	StageSyntoIndexExport         Stage = "synto_index_export"
	StageSourceReconciliation     Stage = "source_reconciliation"
	StageConceptReconciliation    Stage = "concept_reconciliation"
	StagePostprocess              Stage = "postprocess"
	StageGenerationPublish        Stage = "generation_publish"
	StageReceiptRecording         Stage = "receipt_recording"
	StageLeaseCleanup             Stage = "lease_cleanup"
	StageUnknown                  Stage = "unknown"
)

var ValidStages = map[Stage]struct{}{
	StageInputMaterialization: {}, StageSyntoMigration: {},
	StageSyntoConfigNormalization: {}, StageSyntoConfigValidation: {},
	StageSyntoRun: {}, StageSyntoIndexExport: {},
	StageSourceReconciliation: {}, StageConceptReconciliation: {},
	StagePostprocess: {}, StageGenerationPublish: {},
	StageReceiptRecording: {}, StageLeaseCleanup: {}, StageUnknown: {},
}

type ErrorClass string

const (
	ErrorClassValidation       ErrorClass = "validation"
	ErrorClassChildExit        ErrorClass = "child_exit"
	ErrorClassTimeout          ErrorClass = "timeout"
	ErrorClassCancellation     ErrorClass = "cancelled"
	ErrorClassIO               ErrorClass = "io"
	ErrorClassStateInvalid     ErrorClass = "state_invalid"
	ErrorClassPublishConflict  ErrorClass = "publish_conflict"
	ErrorClassRecordingFailure ErrorClass = "recording_failure"
	ErrorClassUnknown          ErrorClass = "unknown"
)

var ValidErrorClasses = map[ErrorClass]struct{}{
	ErrorClassValidation: {}, ErrorClassChildExit: {}, ErrorClassTimeout: {},
	ErrorClassCancellation: {}, ErrorClassIO: {}, ErrorClassStateInvalid: {},
	ErrorClassPublishConflict: {}, ErrorClassRecordingFailure: {}, ErrorClassUnknown: {},
}

type ChildCommand string

const (
	ChildCommandMigrateOLW ChildCommand = "migrate-olw"
	ChildCommandRun        ChildCommand = "run"
	ChildCommandPackExport ChildCommand = "pack-export"
)

var ValidChildCommands = map[ChildCommand]struct{}{
	ChildCommandMigrateOLW: {}, ChildCommandRun: {}, ChildCommandPackExport: {},
}

type DetailCode string

const (
	DetailGeneratedMapReadDecode                 DetailCode = "generated_map_read_decode"
	DetailSyntoIndexTruth                        DetailCode = "synto_index_truth"
	DetailEntityMapping                          DetailCode = "entity_mapping"
	DetailEntityMappingIndexTruth                DetailCode = "entity_mapping_index_truth"
	DetailEntityMappingSourceConceptIdentity     DetailCode = "entity_mapping_source_concept_identity"
	DetailEntityMappingArticleIdentity           DetailCode = "entity_mapping_article_identity"
	DetailEntityMappingArticlePath               DetailCode = "entity_mapping_article_path"
	DetailEntityMappingArticleSourceAmbiguity    DetailCode = "entity_mapping_article_source_ambiguity"
	DetailEntityMappingArticleSourceMissing      DetailCode = "entity_mapping_article_source_missing"
	DetailEntityMappingArticleSourceDisagreement DetailCode = "entity_mapping_article_source_disagreement"
	DetailEntityMappingDuplicateArticleID        DetailCode = "entity_mapping_duplicate_article_id"
	DetailEntityMappingDuplicateArticlePath      DetailCode = "entity_mapping_duplicate_article_path"
	DetailEntityMappingDuplicateEntityID         DetailCode = "entity_mapping_duplicate_entity_id"
	DetailEntityMappingActiveEntityUnknown       DetailCode = "entity_mapping_active_entity_unknown"
	DetailEntityMappingConceptSlugCase           DetailCode = "entity_mapping_concept_slug_case"
	DetailEntityMappingConceptIDPathDisagreement DetailCode = "entity_mapping_concept_id_path_disagreement"
	DetailEntityMappingConceptMissingMapping     DetailCode = "entity_mapping_concept_missing_mapping"
	DetailEntityMappingConceptEntityCollision    DetailCode = "entity_mapping_concept_entity_collision"
	DetailEntityMerge                            DetailCode = "entity_merge"
	DetailIdentityReconciliation                 DetailCode = "identity_reconciliation"
	DetailLifecyclePlanning                      DetailCode = "lifecycle_planning"
	DetailConceptPageRewrite                     DetailCode = "concept_page_rewrite"
	DetailLinkRewrite                            DetailCode = "link_rewrite"
	DetailCacheRewrite                           DetailCode = "cache_rewrite"
	DetailArtifactWrite                          DetailCode = "artifact_write"
	DetailArtifactRemove                         DetailCode = "artifact_remove"
)

var ValidDetailCodes = map[DetailCode]struct{}{
	DetailGeneratedMapReadDecode: {}, DetailSyntoIndexTruth: {}, DetailEntityMapping: {},
	DetailEntityMappingIndexTruth: {}, DetailEntityMappingSourceConceptIdentity: {},
	DetailEntityMappingArticleIdentity: {}, DetailEntityMappingArticlePath: {},
	DetailEntityMappingArticleSourceAmbiguity: {}, DetailEntityMappingArticleSourceMissing: {},
	DetailEntityMappingArticleSourceDisagreement: {}, DetailEntityMappingDuplicateArticleID: {},
	DetailEntityMappingDuplicateArticlePath: {}, DetailEntityMappingDuplicateEntityID: {},
	DetailEntityMappingActiveEntityUnknown: {}, DetailEntityMappingConceptSlugCase: {},
	DetailEntityMappingConceptIDPathDisagreement: {}, DetailEntityMappingConceptMissingMapping: {},
	DetailEntityMappingConceptEntityCollision: {}, DetailEntityMerge: {},
	DetailIdentityReconciliation: {}, DetailLifecyclePlanning: {}, DetailConceptPageRewrite: {},
	DetailLinkRewrite: {}, DetailCacheRewrite: {}, DetailArtifactWrite: {}, DetailArtifactRemove: {},
}
