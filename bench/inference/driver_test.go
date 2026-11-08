//go:build inferencedriver

// Package inference_test holds the measure-only probes of the inference
// driver behind Chapter 9 (Treat Inference Like a Device Driver), at
// v0.1.6 (8d5ff6e). Each probe D1..D7 asks one question of the model
// client, its parser and its retries, against fake servers or the local
// records of live6, and logs what it ran and what it saw as "FINDING:"
// lines. A probe fails only when it cannot run: a defect is recorded as
// observed behavior. They are kept out of go test ./... by the
// inferencedriver tag:
//
//	go test -tags inferencedriver -count=1 -v ./bench/inference/
//
// D6 reads reports/ipc-live/runs/live6, which is gitignored; it is skipped
// where that record is absent.
package inference_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"aos/internal/agentflow"
	"aos/internal/artifact"
	"aos/internal/inference"
	"aos/internal/ipc"
	"aos/internal/kernel"
	"aos/internal/runlog"
	"aos/internal/runtime"
	"aos/internal/tools"
)

func finding(t *testing.T, format string, args ...any) {
	t.Helper()
	t.Logf("FINDING: "+format, args...)
}

// describe is an action in one line, for the tables.
func describe(result runtime.GenerateResult, err error) string {
	if err != nil {
		return "error: " + err.Error()
	}
	a := result.Action
	switch a.Type {
	case "message":
		return fmt.Sprintf("message %q", a.Message)
	case "exit":
		if a.ExitStatus == nil {
			return "exit (no status)"
		}
		return fmt.Sprintf("exit code=%d reason=%q", a.ExitStatus.Code, a.ExitStatus.Reason)
	case "tool":
		if a.ToolCall == nil {
			return "tool (no call)"
		}
		return fmt.Sprintf("tool %s %v", a.ToolCall.Name, a.ToolCall.Args)
	case "delegate":
		if a.Delegate == nil {
			return "delegate (no call)"
		}
		return fmt.Sprintf("delegate to=%s goal=%q", a.Delegate.AgentID, a.Delegate.Goal)
	default:
		return a.Type
	}
}

// generate runs one step of a prompt backend over completer, as an agent
// step of aos run or aos-tui does.
func generate(completer agentflow.JSONCompleter) (runtime.GenerateResult, error) {
	backend := agentflow.PromptBackend{
		Completer: completer,
		Workflow:  agentflow.WorkflowSpec{Name: "probe", EntryAgent: "solo"},
		Agent:     agentflow.AgentSpec{ID: "solo", SystemPrompt: "Work."},
	}
	request := runtime.GenerateRequest{Process: kernel.Process{PID: "pid-000002", Role: "solo"}}
	return backend.Generate(context.Background(), request)
}

// chatServer answers chat completions with reasoning and content, streamed
// or not as the request asks, and the repair call with repair. It counts
// the calls.
type chatServer struct {
	reasoning, content, repair string

	mu    sync.Mutex
	calls int
}

func (s *chatServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Stream bool `json:"stream"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	s.mu.Lock()
	s.calls++
	call := s.calls
	s.mu.Unlock()
	reasoning, content := s.reasoning, s.content
	if call > 1 {
		reasoning, content = "", s.repair
	}
	if !body.Stream {
		message := map[string]string{"role": "assistant", "content": content}
		if reasoning != "" {
			message["reasoning_content"] = reasoning
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": message}}})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	chunk := func(delta map[string]string) {
		data, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": delta}}})
		fmt.Fprintf(w, "data: %s\n\n", data)
	}
	if reasoning != "" {
		chunk(map[string]string{"reasoning_content": reasoning})
	}
	if content != "" {
		chunk(map[string]string{"content": content})
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
}

// D1: the same server reply, read streaming (as aos-tui runs, with
// OnStreamEvent) and not (as aos run does).
func TestD1StreamingAndNot(t *testing.T) {
	const exitInReasoning = `{"action":{"type":"exit","code":0,"reason":"from reasoning"}}`
	const repair = `{"action":{"type":"message","message":"from repair"}}`
	cases := []struct{ name, reasoning, content string }{
		{"valid action in content", exitInReasoning, `{"action":{"type":"message","message":"from content"}}`},
		{"prose in content", exitInReasoning, "I am done with the task."},
		{"content one brace short", exitInReasoning, `{"action":{"type":"message","message":"from content"}`},
		{"empty content", exitInReasoning, ""},
		{"reasoning prose with braces, valid content", "I could call {read_file} here.", `{"action":{"type":"message","message":"from content"}}`},
	}
	for _, c := range cases {
		row := map[bool]string{}
		calls := map[bool]int{}
		for _, stream := range []bool{false, true} {
			server := &chatServer{reasoning: c.reasoning, content: c.content, repair: repair}
			srv := httptest.NewServer(server)
			config := inference.VLLMConfig{BaseURL: srv.URL, Model: "probe", Timeout: 5 * time.Second}
			var events []inference.StreamEvent
			if stream {
				config.OnStreamEvent = func(e inference.StreamEvent) { events = append(events, e) }
			}
			result, err := generate(inference.NewVLLMBackend(config))
			srv.Close()
			row[stream] = describe(result, err)
			calls[stream] = server.calls
		}
		finding(t, "D1 %-48s not streaming (aos run): %s [%d call(s)] | streaming (aos-tui): %s [%d call(s)]",
			c.name+":", row[false], calls[false], row[true], calls[true])
	}
	finding(t, "D1 vllm.go writes every reasoning delta into the text it returns when streaming; without streaming it reads only message.content, so reasoning_content is dropped")
}

// scriptCompleter answers a transcript with replies[0], and a repair with
// replies[1], recording each call's transcript.
type scriptCompleter struct {
	replies []string
	seen    [][]runtime.Message
}

func (s *scriptCompleter) CompleteJSON(context.Context, string, string, any) error {
	return errors.New("not used")
}

func (s *scriptCompleter) CompleteMessages(_ context.Context, messages []runtime.Message) (string, error) {
	s.seen = append(s.seen, messages)
	reply := s.replies[min(len(s.seen), len(s.replies))-1]
	return reply, nil
}

// D3: a corpus of replies, one per stage of the tolerance ladder, and the
// replies that pick the wrong object.
func TestD3ToleranceLadder(t *testing.T) {
	const unrepairable = `{"action": {"type": "tool", "tool_call": {`
	cases := []struct {
		class, first, repair, want string
	}{
		{"clean nested action", `{"action":{"type":"message","message":"hi"}}`, "", `message "hi"`},
		{"trailing JSON after prose", `Sure, here it is: {"action":{"type":"exit","code":0,"reason":"done"}}`, "", `exit code=0 reason="done"`},
		{"bare keys", `{action:{type:"message",message:"hi"}}`, "", `message "hi"`},
		{"half-quoted keys", `{action":{type":"message",message":"hi"}}`, "", `message "hi"`},
		{"trailing commas", `{"action":{"type":"message","message":"hi",},}`, "", `message "hi"`},
		{"generic shape (tool name beside args)", `{"action":{"type":"tool","tool":"read_file","path":"a.txt"}}`, "", `tool read_file map[path:a.txt]`},
		{"flat shape (no action wrapper)", `{"type":"message","message":"hi"}`, "", `message "hi"`},
		{"flat shape the generic stage refuses (delegate without agent)", `{"type":"delegate","delegate":{"goal":"build it"}}`, "", `error: unable to repair malformed action JSON`},
		{"legacy shape (action is a string)", `{"action":"exit","code":0,"reason":"done"}`, "", `exit code=0 reason="done"`},
		{"in-transcript repair", `I will read the file {read_file a.txt}`, `{"action":{"type":"message","message":"repaired"}}`, `message "repaired"`},
		{"fallbackMessage (prose twice)", "All done, the file is written.", "Done.", `message "All done, the file is written."`},
		{"notesAsMessage (empty message twice)", `{"action":{"type":"message","notes":"wrote a.txt","message":""}}`, `{"action":{"type":"message","notes":"wrote a.txt","message":""}}`, `message "wrote a.txt"`},
		{"unrepairable", unrepairable, unrepairable, "error: unable to repair malformed action JSON"},
		{"example quoted after the real action", `{"action":{"type":"exit","code":0,"reason":"done"}} For example, you could also send {"action":{"type":"message","message":"example"}}`, "", `exit code=0 reason="done"`},
		{"one brace short (live6 seq 28 shape)", `{"action":{"type":"delegate","notes":"n","delegate":{"agent_id":"writer","goal":"build it"}}`, "", `delegate to=writer goal="build it"`},
		{"two braces short (args left open)", `{"action":{"type":"tool","tool_call":{"name":"run","args":{"command":"ls"}}`, "", `tool run map[command:ls]`},
		{"<think> JSON, then a real action", `<think>maybe {"action":{"type":"exit","code":0,"reason":"thinking"}}</think>{"action":{"type":"message","message":"real"}}`, "", `message "real"`},
		{"<think> JSON, then prose", `<think>{"action":{"type":"exit","code":0,"reason":"thinking"}}</think>Here you go.`, "Here you go.", `message "Here you go."`},
	}
	correct := 0
	for _, c := range cases {
		replies := []string{c.first}
		if c.repair != "" {
			replies = append(replies, c.repair)
		}
		completer := &scriptCompleter{replies: replies}
		result, err := generate(completer)
		got := describe(result, err)
		verdict := "wrong action"
		switch {
		case got == c.want:
			verdict = "correct"
			correct++
		case err != nil:
			verdict = "no action"
		}
		repairNote := "no repair call"
		if len(completer.seen) > 1 {
			last := completer.seen[1]
			repairNote = fmt.Sprintf("repair call with the raw reply as the assistant turn (%d messages)", len(last))
		}
		finding(t, "D3 %-42s -> %-44s %s; %s", c.class+":", got, verdict, repairNote)
	}
	finding(t, "D3 %d of %d replies read as intended", correct, len(cases))
}

// stateFixture is a state directory laid out as aos run lays it out.
type stateFixture struct {
	kernel *kernel.Kernel
	store  *artifact.Store
	bus    *ipc.Bus
}

func newStateFixture(t *testing.T) stateFixture {
	t.Helper()
	root := t.TempDir()
	k, err := kernel.New(filepath.Join(root, "kernel"))
	if err != nil {
		t.Fatal(err)
	}
	store, err := artifact.New(filepath.Join(root, "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	bus, err := ipc.New(filepath.Join(root, "bus"), k)
	if err != nil {
		t.Fatal(err)
	}
	return stateFixture{kernel: k, store: store, bus: bus}
}

// runOneAgent runs a one-agent workflow on completer with the default
// retry policy, sleeping on a recording clock instead of the wall clock.
func runOneAgent(t *testing.T, completer agentflow.JSONCompleter) (kernel.ExitStatus, []time.Duration) {
	t.Helper()
	f := newStateFixture(t)
	var waits []time.Duration
	engine := agentflow.Engine{Kernel: f.kernel, Artifacts: f.store, Bus: f.bus, Tools: tools.NewRuntime(),
		Factory: agentflow.StaticBackendFactory{Completer: completer},
		Sleep:   func(_ context.Context, d time.Duration) { waits = append(waits, d) }}
	spec := agentflow.WorkflowSpec{Name: "probe", EntryAgent: "solo", Agents: []agentflow.AgentSpec{{ID: "solo", SystemPrompt: "Work."}}}
	summary, err := engine.Run(context.Background(), spec, agentflow.RunRequest{Goal: "go"})
	if err != nil && summary.EntryPID == "" {
		t.Fatalf("run: %v", err)
	}
	return summary.ExitStatus, waits
}

// D2: a hung endpoint and a 5xx endpoint, through the driver alone and
// through the runtime's retry.
func TestD2Retries(t *testing.T) {
	type endpoint struct {
		name    string
		handler func(hits *int, mu *sync.Mutex) http.HandlerFunc
	}
	hang := func(hits *int, mu *sync.Mutex) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			*hits++
			mu.Unlock()
			select {
			case <-r.Context().Done():
			case <-time.After(2 * time.Second):
			}
		}
	}
	status := func(code int) func(hits *int, mu *sync.Mutex) http.HandlerFunc {
		return func(hits *int, mu *sync.Mutex) http.HandlerFunc {
			return func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				*hits++
				mu.Unlock()
				w.WriteHeader(code)
			}
		}
	}
	for _, e := range []endpoint{{"hung (client timeout 50ms)", hang}, {"503", status(http.StatusServiceUnavailable)}, {"404", status(http.StatusNotFound)}} {
		var mu sync.Mutex
		hits := 0
		srv := httptest.NewServer(e.handler(&hits, &mu))
		backend := inference.NewVLLMBackend(inference.VLLMConfig{BaseURL: srv.URL, Model: "probe", Timeout: 50 * time.Millisecond})
		_, err := backend.CompleteText(context.Background(), "system", "user")
		mu.Lock()
		driverHits := hits
		hits = 0
		mu.Unlock()
		exit, waits := runOneAgent(t, backend)
		srv.Close()
		var total time.Duration
		for _, w := range waits {
			total += w
		}
		mu.Lock()
		finding(t, "D2 %-26s driver alone: %d request(s), error %q | runtime: %d request(s), %d waits %v totalling %s; exit code=%d retryable=%t reason=%q",
			e.name+":", driverHits, err, hits, len(waits), waits, total, exit.Code, exit.Retryable, exit.Reason)
		mu.Unlock()
	}
	effective := runtime.BackendRetryPolicy{}.Effective()
	finding(t, "D2 the default policy: %d attempts, base %s, max %s; MaxRetries is never set by NewBackendFromConfig, so the driver makes one attempt per call", effective.Attempts, effective.Base, effective.Max)
}

// D4: the three provider kinds send the same body for the same call, and a
// server that speaks only Anthropic Messages answers 404.
func TestD4ProviderKinds(t *testing.T) {
	type capture struct {
		path   string
		body   []byte
		header http.Header
	}
	var mu sync.Mutex
	var captured []capture
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		captured = append(captured, capture{path: r.URL.Path, body: body, header: r.Header.Clone()})
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"role": "assistant", "content": "ok"}}}})
	}))
	defer srv.Close()
	for _, name := range []string{"AOS_LLM_BASE_URL", "AOS_VLLM_BASE_URL", "AOS_LLM_MODEL", "AOS_VLLM_MODEL", "AOS_LLM_API_KEY", "AOS_VLLM_API_KEY"} {
		t.Setenv(name, "")
	}
	kinds := []string{"vllm", "ollama", "openai-compatible", "anthropic"}
	for _, kind := range kinds {
		t.Setenv("AOS_LLM_BACKEND", kind)
		defaults := inference.LoadBackendConfig(time.Minute)
		parsed, _ := url.Parse(defaults.BaseURL)
		host := parsed.Hostname()
		hostNote := "localhost"
		if host != "localhost" && host != "127.0.0.1" {
			hostNote = "a hostname (not printed)"
		}
		finding(t, "D4 kind %-18s defaults: base URL is %s, model %q, key set=%t", kind, hostNote, defaults.Model, defaults.APIKey != "")
		config := defaults
		config.BaseURL, config.Model = srv.URL, "probe"
		backend := inference.NewBackendFromConfig(config, inference.BackendOptions{})
		if _, err := backend.CompleteText(context.Background(), "system", "user"); err != nil {
			t.Fatal(err)
		}
	}
	for i, c := range captured {
		finding(t, "D4 kind %-18s POST %s body %s authorization=%q", kinds[i], c.path, c.body, redactBearer(c.header.Get("Authorization")))
	}
	same := true
	for _, c := range captured[1:] {
		same = same && bytes.Equal(c.body, captured[0].body)
	}
	finding(t, "D4 request bodies byte-identical across the four kinds: %t; NewBackendFromConfig ignores Kind, so \"anthropic\" builds the same chat-completions client", same)

	anthropicOnly := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/messages" {
			_ = json.NewEncoder(w).Encode(map[string]any{"type": "message", "content": []any{map[string]string{"type": "text", "text": "ok"}}})
			return
		}
		http.NotFound(w, r)
	}))
	defer anthropicOnly.Close()
	t.Setenv("AOS_LLM_BACKEND", "anthropic")
	config := inference.LoadBackendConfig(time.Minute)
	config.BaseURL, config.Model = anthropicOnly.URL, "probe"
	_, err := inference.NewBackendFromConfig(config, inference.BackendOptions{}).CompleteText(context.Background(), "system", "user")
	finding(t, "D4 a server that speaks only /v1/messages, with AOS_LLM_BACKEND=anthropic: %v", err)
}

// redactBearer keeps the scheme and whether a key was sent, never the key.
func redactBearer(value string) string {
	if value == "" {
		return ""
	}
	scheme, _, _ := strings.Cut(value, " ")
	return scheme + " <redacted>"
}

// D5: a workflow spec that names a model or a profile per agent.
func TestD5PerAgentModel(t *testing.T) {
	raw := []byte(`{"name":"probe","entry_agent":"solo","model":"workflow-model",
"agents":[{"id":"solo","system_prompt":"Work.","model":"agent-model","profile":"agent-profile","backend":"anthropic"}]}`)
	var spec agentflow.WorkflowSpec
	unmarshalErr := json.Unmarshal(raw, &spec)
	validateErr := spec.Validate()
	again, _ := json.Marshal(spec)
	kept := []string{}
	for _, field := range []string{`"model"`, `"profile"`, `"backend"`} {
		if bytes.Contains(again, []byte(field)) {
			kept = append(kept, field)
		}
	}
	finding(t, "D5 json.Unmarshal as aos run decodes a workflow: err=%v; Validate: err=%v; fields left after a round trip: %v", unmarshalErr, validateErr, kept)
	strict := json.NewDecoder(bytes.NewReader(raw))
	strict.DisallowUnknownFields()
	var strictSpec agentflow.WorkflowSpec
	finding(t, "D5 a strict decoder would refuse it: %v", strict.Decode(&strictSpec))
	finding(t, "D5 StaticBackendFactory.BackendFor hands every agent the one Completer; the agent spec has no model, profile or backend field")
}

// D6: prompt-cache reuse in live6's local call ledger.
func TestD6CacheReuse(t *testing.T) {
	path := filepath.Join("..", "..", "reports", "ipc-live", "runs", "live6", "state", "run", runlog.CallsFile)
	if _, err := os.Stat(path); err != nil {
		t.Skipf("live6 record not here: %v", err)
	}
	records, err := runlog.Read[inference.CallRecord](path)
	if err != nil {
		t.Fatal(err)
	}
	outcomes := map[int]inference.CallRecord{}
	for _, r := range records {
		if r.Phase == inference.PhaseOutcome {
			outcomes[r.Start] = r
		}
	}
	type step struct {
		pid             string
		repair          bool
		messages        []inference.ChatMessage
		prompt, cached  int
		completion      int
		strictExtension bool
		prevPrompt      int
		prevCompletion  int
	}
	var steps []step
	last := map[string]step{}
	for i, r := range records {
		if r.Phase != inference.PhaseStart {
			continue
		}
		out, ok := outcomes[r.Seq]
		if !ok {
			continue
		}
		s := step{pid: r.PID, repair: r.Repair, messages: inference.RequestMessages(records, i),
			prompt: out.PromptTokens, cached: out.CachedTokens, completion: out.CompletionTokens}
		if prev, ok := last[r.PID]; ok {
			s.strictExtension = len(s.messages) > len(prev.messages) && slices.Equal(s.messages[:len(prev.messages)], prev.messages)
			s.prevPrompt, s.prevCompletion = prev.prompt, prev.completion
		}
		steps = append(steps, s)
		if !r.Repair {
			last[r.PID] = s
		}
	}
	var prompt, cached, firstOfPID, extensions, repairs int
	buckets := map[string]int{}
	order := []string{"0%", "1-49%", "50-89%", "90-98%", "99-100%"}
	var reuse []float64
	var short []string
	for _, s := range steps {
		prompt += s.prompt
		cached += s.cached
		if s.repair {
			repairs++
		}
		ratio := 0.0
		if s.prompt > 0 {
			ratio = float64(s.cached) / float64(s.prompt)
		}
		switch {
		case s.cached == 0:
			buckets["0%"]++
		case ratio < 0.5:
			buckets["1-49%"]++
		case ratio < 0.9:
			buckets["50-89%"]++
		case ratio < 0.99:
			buckets["90-98%"]++
		default:
			buckets["99-100%"]++
		}
		if s.prevPrompt == 0 {
			firstOfPID++
			continue
		}
		if s.strictExtension {
			extensions++
			// What the previous step's prompt makes cacheable, at best.
			share := float64(s.cached) / float64(s.prevPrompt)
			reuse = append(reuse, share)
			if share < 0.99 {
				short = append(short, fmt.Sprintf("%s repair=%t cached=%d prev_prompt=%d", s.pid, s.repair, s.cached, s.prevPrompt))
			}
		}
	}
	sort.Float64s(reuse)
	quantile := func(q float64) float64 {
		if len(reuse) == 0 {
			return 0
		}
		return reuse[int(q*float64(len(reuse)-1))]
	}
	finding(t, "D6 live6: %d calls with an outcome (%d repairs), %d processes' first calls; prompt %d tokens, cached %d (%.1f%%), as the server reported them",
		len(steps), repairs, firstOfPID, prompt, cached, 100*float64(cached)/float64(max(prompt, 1)))
	var dist []string
	for _, b := range order {
		dist = append(dist, fmt.Sprintf("%s: %d", b, buckets[b]))
	}
	finding(t, "D6 cached share of each call's prompt: %s", strings.Join(dist, ", "))
	finding(t, "D6 %d of %d later calls are a strict prefix extension of the same process's previous call", extensions, len(steps)-firstOfPID)
	finding(t, "D6 on those, cached tokens over the previous call's prompt tokens: min %.3f, p10 %.3f, median %.3f, p90 %.3f, max %.3f",
		quantile(0), quantile(0.1), quantile(0.5), quantile(0.9), quantile(1))
	finding(t, "D6 extensions that reused under 99%% of the previous prompt: %d %v", len(short), short)
	var breaks []string
	for _, s := range steps {
		if s.prevPrompt != 0 && !s.strictExtension {
			breaks = append(breaks, fmt.Sprintf("%s repair=%t cached=%d/%d", s.pid, s.repair, s.cached, s.prompt))
		}
	}
	finding(t, "D6 later calls that do not extend the previous one: %d %v", len(breaks), breaks)
}

// D7: max_tokens is clamped to what is left of the token budget.
func TestD7MaxTokensClamp(t *testing.T) {
	var mu sync.Mutex
	var sent []int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			MaxTokens int `json:"max_tokens"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		sent = append(sent, body.MaxTokens)
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"role": "assistant", "content": "ok"}}}})
	}))
	defer srv.Close()
	cases := []struct{ configured, left int }{{12000, 500}, {12000, 20000}, {0, 500}, {12000, 0}}
	for _, c := range cases {
		backend := inference.NewVLLMBackend(inference.VLLMConfig{BaseURL: srv.URL, Model: "probe", Sampling: inference.Sampling{MaxTokens: c.configured}})
		ctx := runtime.WithCallInfo(context.Background(), runtime.CallInfo{PID: "pid-000002", MaxTokens: c.left})
		if _, err := backend.CompleteText(ctx, "system", "user"); err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		finding(t, "D7 configured max_tokens %5d, budget left %5d -> sent max_tokens %d (0 = not sent)", c.configured, c.left, sent[len(sent)-1])
		mu.Unlock()
	}
	finding(t, "D7 the cap is the budget left in prompt plus completion tokens (runtime.go sets CallInfo.MaxTokens = tokens.Left); it does not subtract the prompt the call is about to send")
}

