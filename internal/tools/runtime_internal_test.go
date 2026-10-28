package tools

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"aos/internal/capability"
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

func TestWebSearchReturnsResultsWithoutRawPayload(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/answer" {
			_, _ = w.Write([]byte(`{"AbstractText":"","RelatedTopics":[]}`))
			return
		}
		_, _ = w.Write([]byte(`<html><body>` + strings.Repeat("<!-- padding -->", 2000) +
			`<a class="result__a" href="https://example.com/megakernel">Megakernel <b>notes</b></a>` +
			`<a class="result__snippet" href="#">Fuse a whole decode step into one kernel launch.</a>` +
			`</body></html>`))
	}))
	defer server.Close()
	prevAnswer, prevHTML := instantAnswerEndpoint, htmlSearchEndpoint
	instantAnswerEndpoint, htmlSearchEndpoint = server.URL+"/answer", server.URL+"/html"
	defer func() { instantAnswerEndpoint, htmlSearchEndpoint = prevAnswer, prevHTML }()

	result, err := NewRuntime().Execute(Request{
		Tool:          "web_search",
		Args:          map[string]string{"query": "megakernel"},
		CapabilitySet: capability.Set{Tools: []string{"web_search"}},
	})
	if err != nil {
		t.Fatalf("web_search: %v", err)
	}
	if _, ok := result.Output["raw"]; ok {
		t.Fatalf("expected no raw payload in output, got %d bytes", len(result.Output["raw"]))
	}
	got := result.Output["result"]
	for _, want := range []string{"Megakernel notes", "https://example.com/megakernel", "one kernel launch"} {
		if !strings.Contains(got, want) {
			t.Fatalf("expected %q in result:\n%s", want, got)
		}
	}
}

func TestWebFetchReturnsPagedTextOnly(t *testing.T) {
	paragraph := strings.Repeat("Tile binning keeps triangles near their pixels. ", 2000)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<html><head><script>ignored()</script></head><body><p>" + paragraph + "</p></body></html>"))
	}))
	defer server.Close()
	fetch := func(args map[string]string) Result {
		t.Helper()
		args["url"] = server.URL
		result, err := NewRuntime().Execute(Request{Tool: "web_fetch", Args: args, CapabilitySet: capability.Set{Tools: []string{"web_fetch"}}})
		if err != nil {
			t.Fatalf("web_fetch: %v", err)
		}
		return result
	}

	first := fetch(map[string]string{})
	if _, ok := first.Output["content"]; ok {
		t.Fatal("expected raw page content to be omitted")
	}
	if strings.Contains(first.Output["text"], "ignored()") || first.Output["truncated"] != "true" {
		t.Fatalf("expected extracted, truncated text; truncated=%q", first.Output["truncated"])
	}
	if len(first.Output["text"]) > MaxToolOutputBytes+200 {
		t.Fatalf("expected text capped near %d bytes, got %d", MaxToolOutputBytes, len(first.Output["text"]))
	}
	total, _ := strconv.Atoi(first.Output["text_bytes"])
	if total <= MaxToolOutputBytes {
		t.Fatalf("expected total text size above the cap, got %d", total)
	}

	rest := fetch(map[string]string{"offset": strconv.Itoa(MaxToolOutputBytes)})
	if rest.Output["truncated"] != "" || !strings.Contains(rest.Output["text"], "Tile binning") {
		t.Fatalf("expected the remaining text without truncation, got truncated=%q", rest.Output["truncated"])
	}
}
