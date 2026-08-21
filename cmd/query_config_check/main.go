package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"

	"github.com/rayer/llm-wiki-bff/internal/queryconfig"
)

func main() {
	path := flag.String("path", "", "sealed query config path")
	revision := flag.String("revision", "", "expected config revision")
	digest := flag.String("digest", "", "expected config digest")
	flag.Parse()
	if *path == "" || *revision == "" || *digest == "" {
		fail("path, revision, and digest are required")
	}
	config, raw, err := queryconfig.LoadFileCanonicalBytes(*path)
	if err != nil {
		fail("load artifact: %v", err)
	}
	if config.ConfigRevision != *revision || config.ConfigDigest != *digest {
		fail("artifact identity mismatch: revision=%q digest=%q", config.ConfigRevision, config.ConfigDigest)
	}
	canonical, err := queryconfig.CanonicalJSON(config)
	if err != nil {
		fail("canonicalize artifact: %v", err)
	}
	if bytes.HasSuffix(raw, []byte{'\n'}) || !bytes.Equal(raw, canonical) {
		fail("artifact is not canonical newline-free JSON")
	}
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "query config check failed: "+format+"\n", args...)
	os.Exit(1)
}
