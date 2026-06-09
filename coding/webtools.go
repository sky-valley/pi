package coding

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/sky-valley/pi/agent"
	"github.com/sky-valley/pi/ai"
)

// NOTE: web_fetch is an addition beyond pi's core coding-agent tool set (pi
// provides web access via extensions / Claude-Code tool names). It's included
// because code-research needs to pull external docs/RFCs/issues. It is NOT in
// the default coding tool set; opt in via ToolNames or CreateTool.

const (
	webFetchMaxBytes = 200 * 1024 // cap downloaded body
	webFetchTimeout  = 30 * time.Second
)

func webFetchTool(cwd string) agent.AgentTool {
	return agent.AgentTool{
		Name:        "web_fetch",
		Label:       "web_fetch",
		Description: "Fetch the contents of an HTTP(S) URL and return readable text (HTML is stripped to text). Use for documentation, RFCs, issue pages, or any web resource. Output is truncated.",
		Parameters: ai.Object(
			ai.Prop("url", ai.String("The http(s) URL to fetch")),
		),
		ExecutionMode: agent.ToolParallel,
		Execute: func(ctx context.Context, id string, params map[string]any, onUpdate agent.ToolUpdateFunc) (agent.AgentToolResult, error) {
			url := argStr(params, "url")
			if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
				return agent.AgentToolResult{}, fmt.Errorf("web_fetch requires an http(s) URL, got %q", url)
			}
			reqCtx, cancel := context.WithTimeout(ctx, webFetchTimeout)
			defer cancel()
			req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
			if err != nil {
				return agent.AgentToolResult{}, err
			}
			req.Header.Set("user-agent", "pi-go/web_fetch")
			req.Header.Set("accept", "text/html,text/plain,*/*")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return agent.AgentToolResult{}, fmt.Errorf("fetch failed: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode < 200 || resp.StatusCode >= 400 {
				return agent.AgentToolResult{}, fmt.Errorf("fetch %s returned HTTP %d", url, resp.StatusCode)
			}
			body, _ := io.ReadAll(io.LimitReader(resp.Body, webFetchMaxBytes))

			contentType := resp.Header.Get("Content-Type")
			text := string(body)
			if strings.Contains(contentType, "html") || looksLikeHTML(text) {
				text = htmlToText(text)
			}
			tr := TruncateHead(strings.TrimSpace(text), 0, 0)
			out := tr.Content
			if tr.Truncated {
				out += fmt.Sprintf("\n\n[Truncated: showing %d of %d lines from %s]", tr.OutputLines, tr.TotalLines, url)
			}
			if out == "" {
				out = "(empty response)"
			}
			return textResult(fmt.Sprintf("Fetched %s (%s):\n\n%s", url, contentType, out)), nil
		},
	}
}

func looksLikeHTML(s string) bool {
	head := s
	if len(head) > 512 {
		head = head[:512]
	}
	lower := strings.ToLower(head)
	return strings.Contains(lower, "<!doctype html") || strings.Contains(lower, "<html") || strings.Contains(lower, "<body")
}

var (
	htmlScriptStyle = regexp.MustCompile(`(?is)<(script|style)\b[^>]*>.*?</(script|style)>`)
	htmlTag         = regexp.MustCompile(`(?s)<[^>]+>`)
	htmlWhitespace  = regexp.MustCompile(`[ \t]+`)
	htmlBlankLines  = regexp.MustCompile(`\n{3,}`)
)

// htmlToText strips tags/script/style and decodes a few common entities, leaving
// readable text. Intentionally minimal and dependency-free.
func htmlToText(html string) string {
	s := htmlScriptStyle.ReplaceAllString(html, " ")
	// Turn block-ish tags into newlines before stripping.
	s = regexp.MustCompile(`(?i)</(p|div|br|li|tr|h[1-6]|section|article|header|footer)>`).ReplaceAllString(s, "\n")
	s = regexp.MustCompile(`(?i)<br\s*/?>`).ReplaceAllString(s, "\n")
	s = htmlTag.ReplaceAllString(s, "")
	s = decodeEntities(s)
	s = htmlWhitespace.ReplaceAllString(s, " ")
	// Trim each line, drop runs of blank lines.
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimSpace(line)
	}
	s = strings.Join(lines, "\n")
	s = htmlBlankLines.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}

func decodeEntities(s string) string {
	r := strings.NewReplacer(
		"&amp;", "&", "&lt;", "<", "&gt;", ">", "&quot;", "\"", "&#39;", "'",
		"&apos;", "'", "&nbsp;", " ", "&mdash;", "—", "&ndash;", "–", "&hellip;", "…",
	)
	return r.Replace(s)
}
