package v1

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"

	fm "github.com/adrg/frontmatter"
	"github.com/gin-gonic/gin"
	"github.com/rayer/llm-wiki-bff/internal/handler"
	"github.com/rayer/llm-wiki-bff/internal/jsonutil"
	"github.com/rayer/llm-wiki-bff/internal/search"
)

func buildSystemPrompt(mode string) string {
	base := "CRITICAL: If the user asks about a specific location (city, district, area), ONLY include results relevant to that location. Ignore results from other locations even if they match on topic keywords." +
		"\n\nCITATION FORMAT RULES (mandatory):" +
		"\n- Each wiki block includes one server-issued internal reference in brackets. Use that exact reference in brackets when citing the block; the server will replace it with the canonical title." +
		"\n- Never invent, alter, or reuse a citation reference for a different wiki block" +
		"\n- EVERY factual claim from wiki content MUST have a bracketed citation: [Exact Source Name]" +
		"\n- Use the EXACT full title from the wiki content inside brackets" +
		"\n- Never use **bold** instead of brackets" +
		"\n- Never append source names as plain text without brackets" +
		"\n- Correct example: 「...適合親子放電。[中和員山公園遊逸之丘]」" +
		"\n- Wrong example: 「...適合親子放電。中和員山公園遊逸之丘」" +
		"\n- Each paragraph referencing a source MUST end with its bracketed citation. "
	if mode == "full" {
		return "You are a knowledgeable assistant with access to a personal wiki. Treat the wiki as supplementary reference material — NOT as a constraint." +
			"\n- If the wiki content is RELEVANT to the user's question (same location, topic, or category), use it and cite with [Source Name]." +
			"\n- If the wiki content is NOT relevant (wrong city, different topic, etc.), IGNORE it completely and answer from your own knowledge — exactly as if you were asked this question directly with no wiki." +
			"\n- NEVER say 'I cannot find this in the wiki' or apologize for missing information. Just answer the question." +
			"\n- When mixing wiki and general knowledge, make it seamless — don't call out which is which in the text." +
			"\n\nCITATION FORMAT RULES (mandatory):" +
			"\n- Each wiki block includes one server-issued internal reference in brackets. Use that exact reference in brackets when citing the block; the server will replace it with the canonical title." +
			"\n- Never invent, alter, or reuse a citation reference for a different wiki block" +
			"\n- EVERY factual claim from wiki content MUST have a bracketed citation: [Exact Source Name]" +
			"\n- Use the EXACT full title from the wiki content inside brackets" +
			"\n- Never use **bold** instead of brackets" +
			"\n- Correct example: 「...適合親子放電。[中和員山公園遊逸之丘]」" +
			"\n- Wrong example: 「...適合親子放電。中和員山公園遊逸之丘」"
	}
	return base + "You are a wiki Q&A assistant. Answer ONLY using the wiki content provided below. Do not use external knowledge. Cite every claim using [Source Name]."
}

func buildUserPrompt(query string, contexts []string) string {
	var sb strings.Builder
	sb.WriteString("User question: ")
	sb.WriteString(search.NeutralizeCitationReferences(query))
	sb.WriteString("\n\nWiki content:\n")
	for _, ctx := range contexts {
		sb.WriteString("\n---\n")
		sb.WriteString(ctx)
	}
	return sb.String()
}

func parseFrontmatter(md string) (map[string]interface{}, string) {
	result := make(map[string]interface{})
	if !strings.HasPrefix(md, "---") {
		return result, md
	}

	body, err := fm.MustParse(strings.NewReader(md), &result)
	if err != nil {
		return make(map[string]interface{}), md
	}
	return result, string(body)
}

// parseFrontmatterJSON parses YAML frontmatter and normalizes nested maps so the
// result is safe for encoding/json (yaml.v2 emits map[interface{}]interface{}).
func parseFrontmatterJSON(md string) (map[string]interface{}, string, error) {
	frontmatter, body := parseFrontmatter(md)
	normalized, err := jsonutil.NormalizeMap(frontmatter)
	if err != nil {
		return nil, body, err
	}
	return normalized, body, nil
}

// writeJSON marshals before writing so a marshal failure never yields HTTP 200
// with Content-Type application/json and an empty body (LWC-219).
func writeJSON(c *gin.Context, status int, payload interface{}) {
	data, err := json.Marshal(payload)
	if err != nil {
		log.Printf("response serialization failed: %v", err)
		c.JSON(http.StatusInternalServerError, handler.ErrorResponse{Error: "response serialization failed"})
		return
	}
	c.Data(status, "application/json; charset=utf-8", data)
}

func writeFrontmatterSerializeError(c *gin.Context, err error) {
	log.Printf("frontmatter JSON normalization failed: %v", err)
	c.JSON(http.StatusInternalServerError, handler.ErrorResponse{Error: "response serialization failed"})
}
