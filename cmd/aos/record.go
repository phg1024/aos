package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime/debug"
	"strconv"
	"syscall"

	"aos/internal/inference"
	"aos/internal/runtime"
)

// version is the release this binary was built from; builds set it with
// -ldflags "-X main.version=...".
var version = "dev"

// runRecord is what a workflow run's run.started event records beyond the
// engine's settings: the build and the backend. run fills it.
var runRecord map[string]string

// buildRecord identifies this binary: its version, its VCS revision when
// the build stamped one, and the SHA-256 of the executable, which
// identifies it even when neither was set.
func buildRecord() map[string]string {
	record := map[string]string{"build_version": version}
	if info, ok := debug.ReadBuildInfo(); ok {
		record["go_version"] = info.GoVersion
		for _, setting := range info.Settings {
			switch setting.Key {
			case "vcs.revision":
				record["build_revision"] = setting.Value
			case "vcs.modified":
				record["build_modified"] = setting.Value
			}
		}
	}
	if self, err := os.Executable(); err == nil {
		if file, err := os.Open(self); err == nil {
			digest := sha256.New()
			if _, err := io.Copy(digest, file); err == nil {
				record["build_sha256"] = hex.EncodeToString(digest.Sum(nil))
			}
			file.Close()
		}
	}
	return record
}

// backendRecord is the backend's identity and driver settings, never its
// key, as they take effect: a sampling field left at zero is not sent, so
// the server's own default applies, and a context window of zero is
// unknown.
func backendRecord(config inference.BackendConfig) map[string]string {
	orServer := func(value string, set bool) string {
		if !set {
			return "server default"
		}
		return value
	}
	window := "unknown"
	if config.ContextWindow > 0 {
		window = strconv.Itoa(config.ContextWindow)
	}
	if config.Kind == inference.BackendAnthropic {
		// The Anthropic driver sends no sampling field, and max_tokens
		// always: the profile's value or its default.
		maxTokens := config.Sampling.MaxTokens
		if maxTokens <= 0 {
			maxTokens = inference.DefaultAnthropicMaxTokens
		}
		return map[string]string{
			"backend":         string(config.Kind),
			"base_url":        config.BaseURL,
			"model":           config.Model,
			"temperature":     "not sent",
			"top_p":           "not sent",
			"max_tokens":      strconv.Itoa(maxTokens),
			"thinking_budget": "not sent (adaptive thinking)",
			"context_window":  window,
			"timeout":         config.Timeout.String(),
		}
	}
	return map[string]string{
		"backend":         string(config.Kind),
		"base_url":        config.BaseURL,
		"model":           config.Model,
		"temperature":     strconv.FormatFloat(config.Sampling.Temperature, 'g', -1, 64),
		"top_p":           orServer(strconv.FormatFloat(config.Sampling.TopP, 'g', -1, 64), config.Sampling.TopP > 0),
		"max_tokens":      orServer(strconv.Itoa(config.Sampling.MaxTokens), config.Sampling.MaxTokens > 0),
		"thinking_budget": orServer(strconv.Itoa(config.Sampling.ThinkingBudget), config.Sampling.ThinkingBudget > 0),
		"context_window":  window,
		"timeout":         config.Timeout.String(),
	}
}

// terminationContext is ctx cancelled, with runtime.ErrTerminated as its
// cause, on the first SIGTERM or SIGINT: the run then ends each process
// with a recorded, retryable exit and a final checkpoint, as a spent
// budget does. A second signal gets the default handling and kills.
func terminationContext(ctx context.Context) (context.Context, func()) {
	ctx, cancel := context.WithCancelCause(ctx)
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, os.Interrupt)
	done := make(chan struct{})
	go func() {
		select {
		case sig := <-signals:
			signal.Reset(syscall.SIGTERM, os.Interrupt)
			cancel(fmt.Errorf("%w (%s)", runtime.ErrTerminated, signalName(sig)))
		case <-done:
		}
	}()
	return ctx, func() {
		close(done)
		signal.Stop(signals)
		cancel(nil)
	}
}

func signalName(sig os.Signal) string {
	switch sig {
	case syscall.SIGTERM:
		return "SIGTERM"
	case os.Interrupt:
		return "SIGINT"
	}
	return sig.String()
}
