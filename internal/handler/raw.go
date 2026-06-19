package handler

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/net/html"
)

// maxUploadSize limits raw file uploads to 10MB.
const maxUploadSize = 10 << 20

// safeHTTPClient is an HTTP client with SSRF protection (blocks private IPs).
var safeHTTPClient = &http.Client{
	Timeout: 30 * time.Second,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return fmt.Errorf("too many redirects")
		}
		if isPrivateHost(req.URL.Host) {
			return fmt.Errorf("redirect to private host blocked: %s", req.URL.Host)
		}
		return nil
	},
}

// isPrivateHost checks if a host resolves to a private/internal IP.
func isPrivateHost(host string) bool {
	// Strip port
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	// Block known metadata endpoints and localhost
	lower := strings.ToLower(host)
	if lower == "localhost" || lower == "127.0.0.1" || lower == "::1" ||
		lower == "metadata.google.internal" ||
		lower == "169.254.169.254" ||
		strings.HasSuffix(lower, ".local") ||
		strings.HasSuffix(lower, ".internal") {
		return true
	}
	// Resolve and check IP ranges
	ips, err := net.LookupIP(host)
	if err != nil {
		return true // block unresolved hosts (conservative)
	}
	for _, ip := range ips {
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
			ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
			return true
		}
	}
	return false
}

// ═══════════════ RAW UPLOAD ═══════════════

// RawUploadResponse is the response for POST /api/raw/upload.
type RawUploadResponse struct {
	Message  string `json:"message"`
	Filename string `json:"filename"`
	Path     string `json:"path"`
	Digest   string `json:"digest"`
	Bytes    int64  `json:"bytes"`
}

// UploadRaw handles POST /api/raw/upload — multipart file upload to GCS raw/.
func (h *Handler) UploadRaw(c *gin.Context) {
	// Extract user/project from context (set by auth middleware on v1 routes)
	userID, projectID := getUserProject(c)
	log.Printf("UploadRaw [user=%s project=%s]", userID, projectID)

	// Enforce request body size limit (10MB)
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxUploadSize)

	file, header, err := c.Request.FormFile("file")
	if err != nil {
		if strings.Contains(err.Error(), "request body too large") {
			c.JSON(http.StatusRequestEntityTooLarge, ErrorResponse{Error: "file too large (max 10MB)"})
			return
		}
		c.JSON(http.StatusBadRequest, ErrorResponse{Error: "file field is required (multipart form)"})
		return
	}
	defer file.Close()

	// Only accept .md files and sanitize filename to prevent path traversal
	safeFilename := filepath.Base(header.Filename)
	if !strings.HasSuffix(strings.ToLower(safeFilename), ".md") {
		c.JSON(http.StatusBadRequest, ErrorResponse{Error: "only .md files are accepted"})
		return
	}
	if safeFilename == "." || safeFilename == ".." || safeFilename == ".md" {
		c.JSON(http.StatusBadRequest, ErrorResponse{Error: "invalid filename"})
		return
	}

	data, err := io.ReadAll(file)
	if err != nil {
		c.JSON(http.StatusInternalServerError, ErrorResponse{Error: fmt.Sprintf("read file: %v", err)})
		return
	}

	if len(data) == 0 {
		c.JSON(http.StatusBadRequest, ErrorResponse{Error: "empty file"})
		return
	}

	ctx := context.Background()
	gcsRelPath := fmt.Sprintf("raw/%s", safeFilename)
	digest, err := h.gcs.WriteBytes(ctx, data, gcsRelPath)
	if err != nil {
		c.JSON(http.StatusInternalServerError, ErrorResponse{Error: fmt.Sprintf("upload to GCS: %v", err)})
		return
	}

	log.Printf("Raw uploaded: %s (digest=%s, bytes=%d)", safeFilename, digest, len(data))

	c.JSON(http.StatusOK, RawUploadResponse{
		Message:  "File uploaded to raw/. It will be processed on the next pipeline run.",
		Filename: safeFilename,
		Path:     gcsRelPath,
		Digest:   digest,
		Bytes:    int64(len(data)),
	})
}

// ═══════════════ RAW SCRAPE ═══════════════

// ScrapeRequest is the body for POST /api/raw/scrape.
type ScrapeRequest struct {
	URL      string `json:"url" binding:"required"`
	Filename string `json:"filename"` // optional; derived from URL title if empty
}

// ScrapeResponse is the response for POST /api/raw/scrape.
type ScrapeResponse struct {
	Message  string `json:"message"`
	Filename string `json:"filename"`
	Path     string `json:"path"`
	Title    string `json:"title"`
	Digest   string `json:"digest"`
	Bytes    int64  `json:"bytes"`
}

// ScrapeRaw handles POST /api/raw/scrape — scrape a URL and save as raw markdown.
func (h *Handler) ScrapeRaw(c *gin.Context) {
	var req ScrapeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, ErrorResponse{Error: "url field is required"})
		return
	}

	userID, projectID := getUserProject(c)
	log.Printf("ScrapeRaw [user=%s project=%s]: url=%s", userID, projectID, req.URL)

	if !strings.HasPrefix(req.URL, "http://") && !strings.HasPrefix(req.URL, "https://") {
		c.JSON(http.StatusBadRequest, ErrorResponse{Error: "url must start with http:// or https://"})
		return
	}

	// Fetch and extract content
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()

	title, textContent, err := fetchAndExtract(ctx, req.URL)
	if err != nil {
		c.JSON(http.StatusInternalServerError, ErrorResponse{Error: fmt.Sprintf("scrape failed: %v", err)})
		return
	}

	// Determine filename (sanitize to prevent path traversal)
	filename := filepath.Base(req.Filename)
	if filename == "" || filename == "." {
		filename = sanitizeTitleForFilename(title)
	}
	if !strings.HasSuffix(filename, ".md") {
		filename += ".md"
	}

	// Build markdown with frontmatter — escape URL for YAML safety
	now := time.Now().Format("2006-01-02")
	markdown := fmt.Sprintf("---\ntitle: \"%s\"\nsource: \"%s\"\ndate: %s\n---\n\n# %s\n\n%s",
		escapeYAML(title), escapeYAML(req.URL), now, title, textContent)

	data := []byte(markdown)
	gcsRelPath := fmt.Sprintf("raw/%s", filename)
	digest, err := h.gcs.WriteBytes(ctx, data, gcsRelPath)
	if err != nil {
		c.JSON(http.StatusInternalServerError, ErrorResponse{Error: fmt.Sprintf("upload to GCS: %v", err)})
		return
	}

	log.Printf("Scraped and uploaded: %s → %s (title=%s, bytes=%d)", req.URL, filename, title, len(data))

	c.JSON(http.StatusOK, ScrapeResponse{
		Message:  "URL scraped and saved to raw/. It will be processed on the next pipeline run.",
		Filename: filename,
		Path:     gcsRelPath,
		Title:    title,
		Digest:   digest,
		Bytes:    int64(len(data)),
	})
}

// ═══════════════ PIPELINE TRIGGER ═══════════════

// PipelineRunResponse is the response for POST /api/pipeline/run.
type PipelineRunResponse struct {
	Message   string `json:"message"`
	RawFiles  int    `json:"raw_files"`
	Scheduled bool   `json:"scheduled"`
}

// RunPipeline handles POST /api/pipeline/run — triggers the OLW pipeline.
func (h *Handler) RunPipeline(c *gin.Context) {
	ctx := c.Request.Context()

	userID, projectID := getUserProject(c)
	log.Printf("RunPipeline [user=%s project=%s]", userID, projectID)

	// Count raw files in GCS for reporting
	sources, _ := h.gcs.ListSources(ctx)
	rawCount := len(sources)

	// Write a trigger file to raw/ to signal the pipeline worker
	timestamp := time.Now().Format("2006-01-02T15:04:05")
	triggerContent := fmt.Sprintf("---\ntrigger: true\ntimestamp: %s\n---\n\n# Pipeline Trigger\n\nThis file was created to request a pipeline run.\n", timestamp)
	_, err := h.gcs.WriteBytes(ctx, []byte(triggerContent), "raw/_pipeline_trigger.md")
	if err != nil {
		c.JSON(http.StatusInternalServerError, ErrorResponse{Error: fmt.Sprintf("trigger write failed: %v", err)})
		return
	}

	log.Printf("Pipeline trigger written: raw/_pipeline_trigger.md at %s (raw files: %d)", timestamp, rawCount)

	c.JSON(http.StatusOK, PipelineRunResponse{
		Message:   "Pipeline trigger created. The Cloud Run worker will process on its next scheduled run. Raw files in GCS will be ingested and compiled.",
		RawFiles:  rawCount,
		Scheduled: true,
	})
}

// ═══════════════ LLM TITLE GENERATION ═══════════════

// GenerateTitle handles POST /api/raw/generate-title — generates a wiki-style title from content.
func (h *Handler) GenerateTitle(c *gin.Context) {
	var req struct {
		Content string `json:"content" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, ErrorResponse{Error: "content field is required"})
		return
	}

	userID, projectID := getUserProject(c)
	log.Printf("GenerateTitle [user=%s project=%s]: contentLen=%d", userID, projectID, len(req.Content))

	if h.llm == nil {
		c.JSON(http.StatusOK, gin.H{"title": "Untitled"})
		return
	}

	title, err := h.llm.Chat(
		"Generate a concise, descriptive title for a wiki article based on its content. Return ONLY the title, nothing else. Use the same language as the content. Max 80 characters.",
		req.Content[:min(len(req.Content), 2000)],
	)

	if err != nil || title == "" {
		c.JSON(http.StatusOK, gin.H{"title": "Untitled"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"title": strings.TrimSpace(title)})
}

// ═══════════════ Scraper helpers ═══════════════

// fetchAndExtract fetches a URL and extracts title + text content.
func fetchAndExtract(ctx context.Context, rawURL string) (string, string, error) {
	// Validate URL and check for private hosts
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "", "", fmt.Errorf("invalid URL: %w", err)
	}
	if isPrivateHost(parsed.Host) {
		return "", "", fmt.Errorf("URL resolves to private/internal host (blocked)")
	}

	req, err := http.NewRequestWithContext(ctx, "GET", rawURL, nil)
	if err != nil {
		return "", "", fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; LLMWikiBot/1.0)")

	resp, err := safeHTTPClient.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("fetch: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return "", "", fmt.Errorf("http status %d", resp.StatusCode)
	}

	// Validate Content-Type is HTML
	contentType := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(contentType, "text/html") && !strings.Contains(contentType, "text/plain") {
		return "", "", fmt.Errorf("unsupported content type: %s (only HTML/text supported)", contentType)
	}

	// Read body (limit to 2MB)
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return "", "", fmt.Errorf("read body: %w", err)
	}

	doc, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		return "", "", fmt.Errorf("parse html: %w", err)
	}

	title, text := extractText(doc)
	if title == "" {
		title = "Untitled Page"
	}
	if text == "" {
		return "", "", fmt.Errorf("no extractable text content")
	}

	return title, text, nil
}

// extractText walks the HTML tree and extracts title + visible text.
func extractText(n *html.Node) (title string, text string) {
	var sb strings.Builder
	var titleFound bool

	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if node.Type == html.ElementNode {
			switch node.Data {
			case "title":
				if !titleFound && node.FirstChild != nil {
					title = strings.TrimSpace(node.FirstChild.Data)
					titleFound = true
				}
				return // don't recurse into title
			case "script", "style", "noscript", "nav", "footer", "header", "aside":
				return // skip non-content elements
			case "h1", "h2", "h3", "h4", "h5", "h6":
				if sb.Len() > 0 {
					sb.WriteString("\n\n")
				}
				level := node.Data[1] - '0'
				prefix := strings.Repeat("#", int(level))
				text := extractInlineText(node)
				if text != "" {
					sb.WriteString(prefix + " " + text + "\n")
				}
				return
			case "p", "div", "section", "article", "main":
				if sb.Len() > 0 {
					sb.WriteString("\n\n")
				}
			case "br":
				sb.WriteString("\n")
			case "li":
				if parentIsList(node) {
					text := extractInlineText(node)
					if text != "" {
						sb.WriteString("- " + text + "\n")
					}
				} else {
					text := extractInlineText(node)
					if text != "" && sb.Len() > 0 {
						sb.WriteString(" " + text)
					} else if text != "" {
						sb.WriteString(text)
					}
				}
				return
			}
		}
		if node.Type == html.TextNode {
			text := strings.TrimSpace(node.Data)
			if text != "" {
				sb.WriteString(text)
			}
		}
		for c := range node.ChildNodes() {
			walk(c)
		}
		// Add newline after block elements
		if node.Type == html.ElementNode {
			switch node.Data {
			case "p", "div", "section", "article", "blockquote":
				sb.WriteString("\n")
			}
		}
	}

	walk(n)
	return title, strings.TrimSpace(sb.String())
}

func parentIsList(n *html.Node) bool {
	if n.Parent == nil {
		return false
	}
	return n.Parent.Data == "ul" || n.Parent.Data == "ol"
}

func extractInlineText(n *html.Node) string {
	var sb strings.Builder
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if node.Type == html.ElementNode {
			switch node.Data {
			case "script", "style", "noscript":
				return
			case "strong", "b":
				sb.WriteString("**")
				for c := range node.ChildNodes() {
					walk(c)
				}
				sb.WriteString("**")
				return
			case "em", "i":
				sb.WriteString("*")
				for c := range node.ChildNodes() {
					walk(c)
				}
				sb.WriteString("*")
				return
			case "code":
				sb.WriteString("`")
				for c := range node.ChildNodes() {
					walk(c)
				}
				sb.WriteString("`")
				return
			case "a":
				href := ""
				for _, attr := range node.Attr {
					if attr.Key == "href" {
						href = attr.Val
						break
					}
				}
				sb.WriteString("[")
				for c := range node.ChildNodes() {
					walk(c)
				}
				sb.WriteString("](" + href + ")")
				return
			case "img":
				alt := ""
				src := ""
				for _, attr := range node.Attr {
					if attr.Key == "alt" {
						alt = attr.Val
					}
					if attr.Key == "src" {
						src = attr.Val
					}
				}
				sb.WriteString("![" + alt + "](" + src + ")")
				return
			}
		}
		if node.Type == html.TextNode {
			text := strings.TrimSpace(node.Data)
			if text != "" {
				if sb.Len() > 0 && !strings.HasSuffix(sb.String(), " ") {
					sb.WriteString(" ")
				}
				sb.WriteString(text)
			}
		}
		for c := range node.ChildNodes() {
			walk(c)
		}
	}
	walk(n)
	return strings.TrimSpace(sb.String())
}

// sanitizeTitleForFilename creates a safe filename from a title.
func sanitizeTitleForFilename(title string) string {
	// Remove unsafe chars, limit length
	name := strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == ' ' {
			return r
		}
		// Keep CJK characters
		if r >= 0x4E00 && r <= 0x9FFF || r >= 0x3400 && r <= 0x4DBF || r >= 0x3040 && r <= 0x309F || r >= 0x30A0 && r <= 0x30FF {
			return r
		}
		return '-'
	}, title)
	name = strings.TrimSpace(name)
	if len(name) > 80 {
		name = name[:80]
	}
	// Collapse spaces/dashes
	for strings.Contains(name, "  ") {
		name = strings.ReplaceAll(name, "  ", " ")
	}
	for strings.Contains(name, "--") {
		name = strings.ReplaceAll(name, "--", "-")
	}
	name = strings.Trim(name, "- ")
	if name == "" {
		name = "untitled"
	}
	return name
}

// escapeYAML escapes a string for YAML double-quoted value.
func escapeYAML(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return s
}
