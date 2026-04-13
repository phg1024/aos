package tools

import (
	"strings"
	"testing"
)

func TestSimpleHTMLToTextHandlesOutOfOrderClosingTags(t *testing.T) {
	html := strings.Join([]string{
		"</style>",
		"<html><head>",
		"<style>.hidden { display:none; }</style>",
		"<script>console.log('ignore');</script>",
		"</head><body><h1>Gemma 4</h1><p>Google model overview</p></body></html>",
	}, "")

	text := simpleHTMLToText(html)
	if !strings.Contains(text, "Gemma 4") {
		t.Fatalf("expected visible body text, got %q", text)
	}
	if strings.Contains(text, "console.log") || strings.Contains(text, "display:none") {
		t.Fatalf("expected script/style content to be removed, got %q", text)
	}
}

func TestRemoveTagDropsBrokenTrailingScriptSafely(t *testing.T) {
	html := "<div>before</div><script>let x = 1;"
	text := simpleHTMLToText(html)
	if !strings.Contains(text, "before") {
		t.Fatalf("expected visible text to remain, got %q", text)
	}
}

func TestExtractHTMLSearchResultsParsesDuckDuckGoHTML(t *testing.T) {
	html := `
<html><body>
  <a class="result__a" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fai.google.dev%2Fgemma">Gemma | Google AI</a>
  <div class="result__snippet">Gemma is a family of lightweight open models from Google.</div>
  <a class="result__a" href="https://blog.google/technology/developers/gemma/">Gemma updates</a>
  <div class="result__snippet">Latest developer news for the Gemma model family.</div>
</body></html>`

	results := extractHTMLSearchResults(html)
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	if results[0].URL != "https://ai.google.dev/gemma" {
		t.Fatalf("unexpected first url %q", results[0].URL)
	}
	if !strings.Contains(results[0].Snippet, "lightweight open models") {
		t.Fatalf("unexpected first snippet %q", results[0].Snippet)
	}
}

func TestSummarizeInstantAnswerReturnsEmptyWhenNoUsefulFields(t *testing.T) {
	got := summarizeInstantAnswer(map[string]interface{}{
		"Heading": "Nothing useful",
	})
	if got != "" {
		t.Fatalf("expected empty summary, got %q", got)
	}
}
