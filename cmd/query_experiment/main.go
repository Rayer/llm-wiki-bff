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
)

func main() {
	var options experimentOptions
	flag.StringVar(&options.snapshotPath, "snapshot", "", "frozen Project snapshot directory")
	flag.StringVar(&options.casesPath, "cases", "", "strict JSONL case file")
	flag.IntVar(&options.runs, "runs", 0, "positive number of runs per case (maximum 100)")
	flag.StringVar(&options.outputPath, "output", "", "JSONL output file; stdout when omitted")
	flag.StringVar(&options.configDir, "config-dir", ".", "directory for optional config.toml")
	flag.StringVar(&options.service, "service", serviceProduction, "query service: production or three-host")
	flag.IntVar(&options.selectionLimit, "selection-limit", defaultLimit, "three-host maximum selected concepts")
	flag.IntVar(&options.explorationSlots, "exploration-slots", 1, "three-host selected concepts reserved for exploration")
	options.explorationSlotsSet = true
	var seed int64
	var seedSet bool
	flag.Func("seed", "three-host signed selection seed; query-derived when omitted", func(value string) error {
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
	flag.Parse()
	if seedSet {
		options.seed = &seed
	}
	log.SetOutput(io.Discard)

	if err := runExperiment(context.Background(), options, dependencies{
		loadConfig:           config.Load,
		newExecutor:          newProductionExecutor,
		newThreeHostExecutor: newThreeHostExecutor,
		now:                  time.Now,
		stdout:               os.Stdout,
	}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
