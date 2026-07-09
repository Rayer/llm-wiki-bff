package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"cloud.google.com/go/storage"
	"github.com/rayer/llm-wiki-bff/internal/config"
	wikistorage "github.com/rayer/llm-wiki-bff/internal/storage"
	"google.golang.org/api/iterator"
)

type objectFile struct {
	name       string
	relPath    string
	generation int64
}

func main() {
	var dryRun bool
	var uid string
	var pid string
	var rebuildURL string
	var token string

	flag.BoolVar(&dryRun, "dry-run", false, "Print changes without writing")
	flag.StringVar(&uid, "uid", "", "Override config user_id")
	flag.StringVar(&pid, "pid", "", "Override config project_id")
	flag.StringVar(&rebuildURL, "rebuild-url", "", "Rebuild endpoint URL")
	flag.StringVar(&token, "token", "", "Bearer token for rebuild endpoint")
	flag.Parse()

	cfg, err := config.Load(".")
	if err != nil {
		log.Fatalf("load config: %v", err)
	}
	if uid != "" {
		cfg.UserID = uid
	}
	if pid != "" {
		cfg.ProjectID = pid
	}
	if cfg.Bucket == "" || cfg.UserID == "" || cfg.ProjectID == "" {
		log.Fatal("config.toml must set bucket, user_id, and project_id (use --uid and --pid to override)")
	}
	if rebuildURL == "" {
		rebuildURL = fmt.Sprintf("http://localhost:%s/api/v1/pipeline/rebuild-index", cfg.Port)
	}

	ctx := context.Background()
	storageClient, err := storage.NewClient(ctx)
	if err != nil {
		log.Fatalf("storage client: %v", err)
	}
	defer storageClient.Close()

	bucket := storageClient.Bucket(cfg.Bucket)
	projectPrefix := wikistorage.ProjectPrefixWithSlash(cfg.UserID, cfg.ProjectID)

	log.Printf("GCS: gs://%s/%s", cfg.Bucket, projectPrefix)
	if dryRun {
		log.Println("Mode: DRY RUN (no writes, no rebuild)")
	}

	files, err := listMarkdownObjects(ctx, bucket, projectPrefix, []string{"wiki/", "wiki/sources/"})
	if err != nil {
		log.Fatalf("list markdown objects: %v", err)
	}

	var changed int
	for _, file := range files {
		data, err := readObject(ctx, bucket.Object(file.name))
		if err != nil {
			log.Fatalf("read %s: %v", file.name, err)
		}

		id, err := randomHexID()
		if err != nil {
			log.Fatalf("generate id for %s: %v", file.relPath, err)
		}
		next, didChange, err := addIDToFrontmatter(data, id)
		if err != nil {
			log.Printf("skip %s: %v", file.relPath, err)
			continue
		}
		if !didChange {
			log.Printf("skip %s: id already present", file.relPath)
			continue
		}

		changed++
		if dryRun {
			log.Printf("would update %s with id: %s", file.relPath, id)
			continue
		}
		if err := writeObjectAtomic(ctx, bucket, file, next); err != nil {
			log.Fatalf("write %s: %v", file.relPath, err)
		}
		log.Printf("updated %s with id: %s", file.relPath, id)
	}

	log.Printf("Scanned: %d  Changed: %d", len(files), changed)
	if dryRun {
		return
	}

	if err := rebuildIndex(ctx, rebuildURL, token, cfg.UserID, cfg.ProjectID); err != nil {
		log.Fatalf("rebuild index: %v", err)
	}
	log.Printf("rebuild-index completed: %s", rebuildURL)
}

func listMarkdownObjects(ctx context.Context, bucket *storage.BucketHandle, projectPrefix string, dirs []string) ([]objectFile, error) {
	var files []objectFile
	for _, dir := range dirs {
		prefix := projectPrefix + dir
		it := bucket.Objects(ctx, &storage.Query{Prefix: prefix})
		for {
			attrs, err := it.Next()
			if err != nil {
				if errors.Is(err, iterator.Done) {
					break
				}
				return nil, err
			}
			if !strings.HasSuffix(attrs.Name, ".md") {
				continue
			}
			files = append(files, objectFile{
				name:       attrs.Name,
				relPath:    strings.TrimPrefix(attrs.Name, projectPrefix),
				generation: attrs.Generation,
			})
		}
	}
	return files, nil
}

func readObject(ctx context.Context, obj *storage.ObjectHandle) ([]byte, error) {
	r, err := obj.NewReader(ctx)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(r)
}

func addIDToFrontmatter(data []byte, id string) ([]byte, bool, error) {
	if !bytes.HasPrefix(data, []byte("---")) {
		return data, false, fmt.Errorf("missing YAML frontmatter")
	}

	lineEnd := bytes.IndexByte(data, '\n')
	if lineEnd < 0 || strings.TrimSpace(string(data[:lineEnd])) != "---" {
		return data, false, fmt.Errorf("missing YAML frontmatter opening marker")
	}

	closeStart, _, err := frontmatterClose(data[lineEnd+1:])
	if err != nil {
		return data, false, err
	}
	frontmatterBody := data[lineEnd+1 : lineEnd+1+closeStart]
	if hasIDField(frontmatterBody) {
		return data, false, nil
	}

	insertAt := lineEnd + 1
	next := make([]byte, 0, len(data)+len(id)+5)
	next = append(next, data[:insertAt]...)
	next = append(next, "id: "...)
	next = append(next, id...)
	next = append(next, '\n')
	next = append(next, data[insertAt:]...)
	return next, true, nil
}

func frontmatterClose(data []byte) (int, int, error) {
	offset := 0
	for {
		lineEnd := bytes.IndexByte(data[offset:], '\n')
		if lineEnd < 0 {
			if strings.TrimSpace(string(data[offset:])) == "---" {
				return offset, len(data), nil
			}
			return 0, 0, fmt.Errorf("missing YAML frontmatter closing marker")
		}

		lineStart := offset
		lineStop := offset + lineEnd
		if strings.TrimSpace(string(data[lineStart:lineStop])) == "---" {
			return lineStart, lineStop + 1, nil
		}
		offset = lineStop + 1
	}
}

func hasIDField(frontmatter []byte) bool {
	lines := bytes.Split(frontmatter, []byte{'\n'})
	for _, line := range lines {
		trimmed := strings.TrimSpace(string(line))
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if len(line) > 0 && (line[0] == ' ' || line[0] == '\t') {
			continue
		}
		key, _, ok := strings.Cut(trimmed, ":")
		if ok && strings.TrimSpace(key) == "id" {
			return true
		}
	}
	return false
}

func randomHexID() (string, error) {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func writeObjectAtomic(ctx context.Context, bucket *storage.BucketHandle, file objectFile, data []byte) error {
	tmpName := fmt.Sprintf("%s.tmp.%d", file.name, time.Now().UnixNano())
	tmpObj := bucket.Object(tmpName)

	w := tmpObj.NewWriter(ctx)
	w.ContentType = "text/markdown; charset=utf-8"
	if _, err := w.Write(data); err != nil {
		w.Close()
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	tmpAttrs := w.Attrs()
	tmpSource := tmpObj
	if tmpAttrs != nil && tmpAttrs.Generation > 0 {
		tmpSource = tmpObj.Generation(tmpAttrs.Generation)
	}
	defer func() {
		_ = tmpObj.Delete(context.Background())
	}()

	finalObj := bucket.Object(file.name).If(storage.Conditions{GenerationMatch: file.generation})
	_, err := finalObj.CopierFrom(tmpSource).Run(ctx)
	return err
}

func rebuildIndex(ctx context.Context, rebuildURL, token, userID, projectID string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rebuildURL, nil)
	if err != nil {
		return err
	}
	if token == "" {
		token = strings.TrimSpace(os.Getenv("BACKFILL_REBUILD_TOKEN"))
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("X-User-ID", userID)
	req.Header.Set("X-Project-ID", projectID)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("%s: %s", resp.Status, string(body))
	}
	if len(body) > 0 {
		log.Printf("rebuild-index response: %s", strings.TrimSpace(string(body)))
	}
	return nil
}
