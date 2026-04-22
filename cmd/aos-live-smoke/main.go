package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"aos/internal/artifact"
	"aos/internal/inference"
	"aos/internal/kernel"
	"aos/internal/runtime"
	"aos/internal/tools"
)

func main() {
	llmConfig := inference.LoadBackendConfig(30 * time.Second)

	root, err := os.MkdirTemp("", "aos-live-smoke-*")
	if err != nil {
		fail(err)
	}
	defer os.RemoveAll(root)

	k, err := kernel.New(filepath.Join(root, "kernel"))
	if err != nil {
		fail(err)
	}
	store, err := artifact.New(filepath.Join(root, "artifacts"))
	if err != nil {
		fail(err)
	}
	process, err := k.Spawn(kernel.ProcessSpec{Role: "live-smoke"})
	if err != nil {
		fail(err)
	}

	backend := inference.NewBackendFromConfig(llmConfig, inference.BackendOptions{
		SystemPrompt: "Return only compact JSON matching the GenerateResult schema. " +
			"Prefer an exit action with reason 'live-smoke-ok'.",
	})

	agentRuntime := &runtime.AgentRuntime{
		Kernel:    k,
		Artifacts: store,
		Tools:     tools.NewRuntime(),
		Backend:   backend,
	}

	result, err := agentRuntime.Run(context.Background(), process.PID, []runtime.Message{
		{
			Role:    "user",
			Content: "Respond with JSON for an exit action whose reason is live-smoke-ok and retryable is false.",
		},
	})
	if err != nil {
		fail(err)
	}

	bytes, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		fail(err)
	}
	fmt.Println(string(bytes))
}

func getenv(key, fallback string) string {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	return value
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
