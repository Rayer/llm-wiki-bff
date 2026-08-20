package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"strconv"
	"time"

	"github.com/rayer/llm-wiki-bff/internal/config"
	"github.com/rayer/llm-wiki-bff/internal/queryquality"
)

func main() {
	var options experimentOptions
	flag.StringVar(&options.snapshotPath, "snapshot", "", "frozen Project snapshot directory")
	flag.StringVar(&options.gcsBucket, "gcs-bucket", "", "explicit GCS bucket")
	flag.StringVar(&options.gcsUserID, "gcs-user-id", "", "explicit GCS user ID")
	flag.StringVar(&options.projectID, "project-id", "", "explicit GCS project ID")
	flag.StringVar(&options.casesPath, "cases", "", "strict JSONL case file")
	flag.StringVar(&options.suggestedQueryMode, "suggested-query-mode", "", "append published suggested queries in wiki or full mode")
	flag.IntVar(&options.runs, "runs", 0, "positive number of runs per case (maximum 100)")
	flag.StringVar(&options.outputPath, "output", "", "JSONL output file; stdout when omitted")
	flag.StringVar(&options.configDir, "config-dir", ".", "directory for optional config.toml")
	flag.StringVar(&options.service, "service", serviceProduction, "query service: production or query-retrieval")
	flag.IntVar(&options.selectionLimit, "selection-limit", defaultLimit, "query-retrieval maximum selected concepts")
	flag.IntVar(&options.explorationSlots, "exploration-slots", 1, "query-retrieval selected concepts reserved for exploration")
	options.explorationSlotsSet = true
	flag.Func("evidence-threshold", "query-retrieval minimum independent evidence dimensions; explicit zero is trusted-local legacy control", func(value string) error {
		parsed, err := strconv.Atoi(value)
		if err != nil {
			return err
		}
		options.evidenceThreshold = parsed
		options.evidenceThresholdSet = true
		return nil
	})
	flag.IntVar(&options.keywordsPerAttempt, "keywords-per-attempt", queryquality.DefaultKeywordsPerAttempt, "maximum normalized positive expansion keywords per attempt")
	flag.IntVar(&options.expansionAttempts, "expansion-attempts", queryquality.DefaultExpansionAttempts, "bounded parallel expansion attempts")
	flag.IntVar(&options.rareDocumentFrequency, "rare-keyword-max-document-frequency", queryquality.DefaultRareDocumentFrequency, "maximum corpus document frequency for rare lexical qualification")
	var seed int64
	var seedSet bool
	flag.Func("seed", "query-retrieval signed selection seed; query-derived when omitted", func(value string) error {
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return err
		}
		seed = parsed
		seedSet = true
		return nil
	})
	flag.StringVar(&options.modelFixturePath, "model-fixture", "", "trusted-local strict JSON model fixture")
	flag.StringVar(&options.models, "models", "", "comma-separated model fixture IDs")
	flag.StringVar(&options.profileFixturePath, "profile-fixture", "", "trusted-local strict JSON criterion profile fixture")
	flag.StringVar(&options.profiles, "profiles", "", "comma-separated profile fixture IDs")
	flag.StringVar(&options.promptFixturePath, "prompt-fixture", "", "trusted-local strict JSON expansion prompt fixture")
	flag.StringVar(&options.prompts, "prompts", "", "comma-separated prompt fixture IDs")
	flag.StringVar(&options.artifactsDir, "artifacts-dir", "", "trusted-local per-attempt receipt directory")
	flag.StringVar(&options.summaryPath, "summary", "", "trusted-local quantitative JSON summary")
	flag.StringVar(&options.stageConfigOutput, "stage-config-output", "", "sealed canonical query stage config regular file")
	flag.StringVar(&options.configRevision, "config-revision", "", "immutable operator config revision required for stage config output")
	flag.StringVar(&options.generationID, "generation-id", "", "explicit frozen local snapshot generation identity")
	flag.StringVar(&options.conceptsDigest, "concepts-digest", "", "expected frozen concepts sha256 digest")
	flag.Parse()
	if seedSet {
		options.seed = &seed
	}
	log.SetOutput(io.Discard)

	if err := runExperiment(context.Background(), options, dependencies{
		loadConfig:                config.Load,
		newExecutor:               newProductionExecutor,
		newQueryRetrievalExecutor: newQueryRetrievalExecutor,
		now:                       time.Now,
		stdout:                    os.Stdout,
	}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
