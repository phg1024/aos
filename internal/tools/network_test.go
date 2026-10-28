package tools_test

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"aos/internal/capability"
	"aos/internal/tools"
)

func fetch(runtime *tools.Runtime, set capability.Set, target string) (tools.Result, error) {
	set.Tools = []string{"web_fetch"}
	return runtime.Execute(tools.Request{Tool: "web_fetch", CapabilitySet: set, Args: map[string]string{"url": target, "timeout": "10s"}})
}

func TestWebToolsNeedAllowNetwork(t *testing.T) {
	if _, err := fetch(tools.NewRuntime(), capability.Set{}, "https://example.com/"); !errors.Is(err, tools.ErrNetworkDenied) {
		t.Fatalf("web_fetch without allow_network: %v", err)
	}
	_, err := tools.NewRuntime().Execute(tools.Request{Tool: "web_search", CapabilitySet: capability.Set{Tools: []string{"web_search"}}, Args: map[string]string{"query": "x"}})
	if !errors.Is(err, tools.ErrNetworkDenied) {
		t.Fatalf("web_search without allow_network: %v", err)
	}
}

// With network, loopback and private targets, the model endpoint and
// redirects into them are refused unless allowlisted.
func TestWebFetchRefusesPrivateTargetsUnlessAllowlisted(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
			return
		}
		w.Write([]byte("internal page"))
	}))
	defer server.Close()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
	runtime := tools.NewRuntime()
	network := capability.Set{AllowNetwork: true}

	for _, target := range []string{server.URL, "http://localhost:" + port + "/", "http://10.255.255.1:" + port + "/", "http://169.254.169.254/latest/meta-data/"} {
		if _, err := fetch(runtime, network, target); !errors.Is(err, tools.ErrNetworkDenied) {
			t.Fatalf("%s should be refused: %v", target, err)
		}
	}
	for _, allow := range [][]string{{"127.0.0.1"}, {"127.0.0.0/8"}} {
		set := network
		set.NetworkAllow = allow
		result, err := fetch(runtime, set, server.URL)
		if err != nil || result.Output["text"] != "internal page" {
			t.Fatalf("allowlisted %v: %+v %v", allow, result.Output, err)
		}
	}
	// Allowing the first hop does not allow where it redirects: here the
	// cloud metadata address.
	allowed := network
	allowed.NetworkAllow = []string{"127.0.0.1"}
	if _, err := fetch(runtime, allowed, server.URL+"/redirect"); !errors.Is(err, tools.ErrNetworkDenied) {
		t.Fatalf("redirect to the metadata address should be refused: %v", err)
	}
	// The model endpoint is refused by name, before any address check.
	runtime.ModelHosts = []string{"localhost"}
	if _, err := fetch(runtime, network, "http://localhost:"+port+"/"); err == nil || !strings.Contains(err.Error(), "model endpoint") {
		t.Fatalf("model endpoint: %v", err)
	}
	modelAllowed := network
	modelAllowed.NetworkAllow = []string{"localhost"}
	if result, err := fetch(runtime, modelAllowed, "http://localhost:"+port+"/"); err != nil || result.Output["text"] != "internal page" {
		t.Fatalf("an allowlisted model endpoint: %+v %v", result.Output, err)
	}
}

// A body that never ends is cut at MaxFetchBytes while it streams.
func TestWebFetchCapsTheBodyWhileReading(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		chunk := []byte(strings.Repeat("x", 64<<10))
		for {
			if _, err := w.Write(chunk); err != nil {
				return
			}
			select {
			case <-r.Context().Done():
				return
			default:
			}
		}
	}))
	defer server.Close()
	start := time.Now()
	result, err := fetch(tools.NewRuntime(), capability.Set{AllowNetwork: true, NetworkAllow: []string{"127.0.0.1"}}, server.URL)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if result.Output["body_truncated"] == "" || result.Output["text_bytes"] != "4194304" || time.Since(start) > 5*time.Second {
		t.Fatalf("endless body: truncated=%q text_bytes=%s after %s", result.Output["body_truncated"], result.Output["text_bytes"], time.Since(start))
	}
}
