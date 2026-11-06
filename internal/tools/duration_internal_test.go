package tools

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"aos/internal/capability"
)

func runWith(t *testing.T, command, timeout string, maxSeconds int) (Result, error) {
	t.Helper()
	root := t.TempDir()
	return NewRuntime().Execute(Request{
		Tool:          "run",
		Args:          map[string]string{"command": command, "timeout": timeout},
		Mounts:        []capability.Mount{{Name: "workspace", HostPath: root}},
		CapabilitySet: capability.Set{Tools: []string{"run"}, ReadPaths: []string{root}, WritePaths: []string{root}, UnconfinedRun: true, MaxRunSeconds: maxSeconds},
	})
}

func TestRunReadsItsTimeout(t *testing.T) {
	for raw, want := range map[string]string{
		"":      "1m0s",
		"300":   "5m0s",
		" 300 ": "5m0s",
		"1":     "1s",
		"5m":    "5m0s",
		"300s":  "5m0s",
		"1m30s": "1m30s",
	} {
		t.Run("value="+raw, func(t *testing.T) {
			result, err := runWith(t, "true", raw, 0)
			if err != nil {
				t.Fatal(err)
			}
			if got := result.Output["timeout"]; got != want || result.Output["timeout_clamped"] != "" {
				t.Fatalf("ran with %q (clamped %q), want %q", got, result.Output["timeout_clamped"], want)
			}
		})
	}
}

// A timeout that cannot be read refuses the call and says what would do. Taking
// the default instead would have the model believe it had asked for something.
func TestRunRefusesATimeoutItCannotRead(t *testing.T) {
	for _, raw := range []string{"soon", "5 minutes", "2.5", "1e3", "0x10", "0", "0s", "-5", "-5s", "99999999999999999999", "9223372036854775807", "18446744074"} {
		t.Run("value="+raw, func(t *testing.T) {
			marker := t.TempDir() + "/ran"
			result, err := runWith(t, "touch "+marker, raw, 0)
			if err == nil {
				t.Fatalf("ran: %+v", result.Output)
			}
			for _, want := range []string{"run timeout", "whole number of seconds", "5m"} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error %q lacks %q", err, want)
				}
			}
			if _, statErr := os.Stat(marker); statErr == nil {
				t.Fatal("the command ran anyway")
			}
		})
	}
}

// Above the limit a bare number is cut like any other, and the result says so.
func TestRunReportsATimeoutOverItsLimit(t *testing.T) {
	for _, raw := range []string{"9999", "1h"} {
		t.Run("value="+raw, func(t *testing.T) {
			result, err := runWith(t, "true", raw, 0)
			if err != nil {
				t.Fatal(err)
			}
			if result.Output["timeout"] != "10m0s" || result.Output["timeout_bound"] != "10m0s" || result.Output["timeout_clamped"] != "true" {
				t.Fatalf("%v", result.Output)
			}
		})
	}
	at, err := runWith(t, "true", "600", 0)
	if err != nil || at.Output["timeout"] != "10m0s" || at.Output["timeout_clamped"] != "" {
		t.Fatalf("at the limit: %v, %v", at.Output, err)
	}
}

// A command stopped at the limit has no result to carry timeout_clamped, so its
// error says what was asked for.
func TestRunTimedOutAtItsLimitSaysWhatWasAsked(t *testing.T) {
	_, err := runWith(t, "sleep 20", "5", 1)
	if err == nil || !strings.Contains(err.Error(), "timed out after 1s") || !strings.Contains(err.Error(), "asked for 5s") {
		t.Fatalf("err = %v", err)
	}
	_, err = runWith(t, "sleep 20", "1", 1)
	if err == nil || !strings.Contains(err.Error(), "timed out after 1s") || strings.Contains(err.Error(), "asked for") {
		t.Fatalf("within the limit: %v", err)
	}
}

func slowServer(t *testing.T, delay time.Duration) *httptest.Server {
	t.Helper()
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		select {
		case <-release:
		case <-time.After(delay):
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("late"))
	}))
	t.Cleanup(func() {
		close(release)
		server.Close()
	})
	return server
}

// A bare number bounds the wait for both network tools, and the old fallback
// (30 or 60 s) would have let this slow server answer.
func TestNetworkToolsReadABareNumberAsSeconds(t *testing.T) {
	server := slowServer(t, 3*time.Second)
	prevAnswer, prevHTML := instantAnswerEndpoint, htmlSearchEndpoint
	instantAnswerEndpoint, htmlSearchEndpoint = server.URL+"/answer", server.URL+"/html"
	defer func() { instantAnswerEndpoint, htmlSearchEndpoint = prevAnswer, prevHTML }()

	for _, request := range []Request{
		{Tool: "web_search", Args: map[string]string{"query": "x", "timeout": "1"}, CapabilitySet: capability.Set{Tools: []string{"web_search"}, AllowNetwork: true}},
		{Tool: "web_fetch", Args: map[string]string{"url": server.URL, "timeout": "1"}, CapabilitySet: capability.Set{Tools: []string{"web_fetch"}, AllowNetwork: true, NetworkAllow: []string{"127.0.0.1"}}},
	} {
		start := time.Now()
		result, err := NewRuntime().Execute(request)
		if err == nil {
			t.Fatalf("%s came back: %v", request.Tool, result.Output)
		}
		if elapsed := time.Since(start); elapsed > 2500*time.Millisecond {
			t.Fatalf("%s waited %s on a timeout of 1", request.Tool, elapsed)
		}
	}
}

// Zero and negative matter more here than for run: an http.Client with no
// timeout waits for ever.
func TestNetworkToolsRefuseATimeoutTheyCannotRead(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		_, _ = w.Write([]byte("reached"))
	}))
	defer server.Close()
	prevAnswer, prevHTML := instantAnswerEndpoint, htmlSearchEndpoint
	instantAnswerEndpoint, htmlSearchEndpoint = server.URL+"/answer", server.URL+"/html"
	defer func() { instantAnswerEndpoint, htmlSearchEndpoint = prevAnswer, prevHTML }()

	for _, raw := range []string{"soon", "0", "0s", "-1s"} {
		for _, request := range []Request{
			{Tool: "web_search", Args: map[string]string{"query": "x", "timeout": raw}, CapabilitySet: capability.Set{Tools: []string{"web_search"}, AllowNetwork: true}},
			{Tool: "web_fetch", Args: map[string]string{"url": server.URL, "timeout": raw}, CapabilitySet: capability.Set{Tools: []string{"web_fetch"}, AllowNetwork: true, NetworkAllow: []string{"127.0.0.1"}}},
		} {
			_, err := NewRuntime().Execute(request)
			if err == nil || !strings.Contains(err.Error(), request.Tool+" timeout") {
				t.Fatalf("%s timeout %q: %v", request.Tool, raw, err)
			}
		}
	}
	if calls != 0 {
		t.Fatalf("a refused timeout still reached the server %d times", calls)
	}
}
