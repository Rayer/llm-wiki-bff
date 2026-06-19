package v1

import (
	"strings"

	"github.com/rayer/llm-wiki-bff/internal/search"
)

func buildSystemPrompt(mode string) string {
	base := "CRITICAL: If the user asks about a specific location (city, district, area), ONLY include results relevant to that location. Ignore results from other locations even if they match on topic keywords." +
		"\n\nCITATION FORMAT RULES (mandatory):" +
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
	sb.WriteString(query)
	sb.WriteString("\n\nWiki content:\n")
	for _, ctx := range contexts {
		sb.WriteString("\n---\n")
		sb.WriteString(ctx)
	}
	return sb.String()
}

func ensureBrackets(text string, results []search.Result) string {
	names := make(map[string]bool)
	var sorted []string
	for _, result := range results {
		if !names[result.Title] {
			names[result.Title] = true
			sorted = append(sorted, result.Title)
		}
	}
	for i := 0; i < len(sorted); i++ {
		for j := i + 1; j < len(sorted); j++ {
			if len(sorted[j]) > len(sorted[i]) {
				sorted[i], sorted[j] = sorted[j], sorted[i]
			}
		}
	}

	for _, name := range sorted {
		if len(name) < 3 {
			continue
		}
		bracketed := "[" + name + "]"
		if strings.Contains(text, bracketed) {
			continue
		}
		idx := 0
		for {
			pos := strings.Index(text[idx:], name)
			if pos < 0 {
				break
			}
			absPos := idx + pos
			before := ""
			if absPos > 0 {
				before = text[absPos-1 : absPos]
			}
			after := ""
			if absPos+len(name) < len(text) {
				after = text[absPos+len(name) : absPos+len(name)+1]
			}
			if before == "[" && after == "]" {
				idx = absPos + len(name)
				continue
			}
			text = text[:absPos] + bracketed + text[absPos+len(name):]
			idx = absPos + len(bracketed)
		}
	}
	return text
}

func parseFrontmatter(md string) (map[string]interface{}, string) {
	frontmatter := make(map[string]interface{})
	if !strings.HasPrefix(md, "---") {
		return frontmatter, md
	}

	end := strings.Index(md[3:], "\n---")
	if end < 0 {
		return frontmatter, md
	}
	end += 3

	for _, line := range strings.Split(md[3:end], "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) == 2 {
			key := strings.TrimSpace(parts[0])
			value := strings.Trim(strings.TrimSpace(parts[1]), "\"'")
			frontmatter[key] = value
		}
	}

	return frontmatter, md[end+3:]
}
