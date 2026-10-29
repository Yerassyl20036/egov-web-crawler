package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeChatCompletions serves scripted OpenAI-style SSE streams for
// /chat/completions. A turn of nil answers 503 (to exercise retries).
type fakeChatCompletions struct {
	mu       sync.Mutex
	turns    [][]string
	requests []map[string]any
	auth     []string
}

func (f *fakeChatCompletions) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/chat/completions" {
		http.NotFound(w, r)
		return
	}
	body, _ := io.ReadAll(r.Body)
	var req map[string]any
	json.Unmarshal(body, &req)
	f.mu.Lock()
	i := len(f.requests)
	f.requests = append(f.requests, req)
	f.auth = append(f.auth, r.Header.Get("Authorization"))
	f.mu.Unlock()
	if i >= len(f.turns) {
		http.Error(w, `{"error":{"message":"unexpected call"}}`, 400)
		return
	}
	if f.turns[i] == nil {
		http.Error(w, `{"error":{"message":"overloaded"}}`, 503)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	fmt.Fprint(w, ": keep-alive\n\n")
	for _, data := range f.turns[i] {
		fmt.Fprintf(w, "data: %s\n\n", data)
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
}

func chunk(delta string, finish string) string {
	f := "null"
	if finish != "" {
		f = fmt.Sprintf("%q", finish)
	}
	return fmt.Sprintf(`{"model":"deepseek-flash","choices":[{"index":0,"delta":%s,"finish_reason":%s}]}`, delta, f)
}

const usageChunk = `{"model":"deepseek-flash","choices":[],"usage":{"prompt_tokens":900,"completion_tokens":60,"prompt_cache_hit_tokens":512}}`

func deepseekConfig(url string) Config {
	return Config{Provider: ProviderDeepSeek, Model: "deepseek-flash", BaseURL: url, APIKey: "sk-test",
		Effort: "low", Thinking: "enabled", MaxTokens: 2000, MaxTurns: 4}
}

func TestOpenAICompatibleToolLoop(t *testing.T) {
	api := &fakeChatCompletions{turns: [][]string{
		nil, // first attempt fails with 503 and is retried
		{
			chunk(`{"role":"assistant","reasoning_content":"Need Russian keywords. "}`, ""),
			chunk(`{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"search_datasets","arguments":""}}]}`, ""),
			chunk(`{"tool_calls":[{"index":0,"function":{"arguments":"{\"query\": \"больницы"}}]}`, ""),
			chunk(`{"tool_calls":[{"index":0,"function":{"arguments":" Алматы\"}"}}]}`, ""),
			chunk(`{}`, "tool_calls"),
			usageChunk,
		},
		{
			chunk(`{"content":"See [Больницы](https://data.egov.kz/datasets/view?index=hospitals_almaty)"}`, ""),
			chunk(`{"content":"."}`, "stop"),
			usageChunk,
		},
	}}
	srv := httptest.NewServer(api)
	defer srv.Close()

	cat := newFakeCatalog()
	a := New(deepseekConfig(srv.URL), &Tools{Catalog: cat})
	var types []string
	res, err := a.Run(context.Background(), []Message{{Role: "user", Content: "hospitals in Almaty?"}}, func(e Event) {
		types = append(types, e.Type)
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(cat.queries) != 1 || cat.queries[0].Query != "больницы Алматы" {
		t.Fatalf("tool input not reassembled from deltas: %+v", cat.queries)
	}
	if got := strings.Join(types, ","); got != "thinking,tool_call,tool_result,text,text" {
		t.Fatalf("events = %s", got)
	}
	if res.Provider != ProviderDeepSeek || res.Turns != 2 || res.Usage.InputTokens != 1800 || res.Usage.CacheReadTokens != 1024 {
		t.Fatalf("result = %+v", res)
	}
	if len(res.Citations) != 1 || !res.Citations[0].Verified {
		t.Fatalf("citations = %+v", res.Citations)
	}
	if len(api.requests) != 3 || api.auth[1] != "Bearer sk-test" {
		t.Fatalf("expected a retried first request with bearer auth, got %d requests", len(api.requests))
	}

	first := api.requests[1]
	if first["stream"] != true || first["model"] != "deepseek-flash" || first["reasoning_effort"] != "low" {
		t.Errorf("request = %v", first)
	}
	if th := first["thinking"].(map[string]any); th["type"] != "enabled" {
		t.Errorf("thinking = %v", th)
	}
	fn := first["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "search_datasets" || fn["parameters"].(map[string]any)["type"] != "object" {
		t.Errorf("tool = %v", fn)
	}
	msgs := first["messages"].([]any)
	if msgs[0].(map[string]any)["role"] != "system" {
		t.Errorf("first message should be the system prompt")
	}

	// Second request: assistant tool call (with reasoning passed back, as
	// DeepSeek's thinking mode requires) followed by the tool result.
	msgs = api.requests[2]["messages"].([]any)
	asst := msgs[2].(map[string]any)
	if asst["role"] != "assistant" || asst["reasoning_content"] != "Need Russian keywords. " {
		t.Fatalf("assistant message = %v", asst)
	}
	call := asst["tool_calls"].([]any)[0].(map[string]any)
	if call["id"] != "call_1" || call["function"].(map[string]any)["arguments"] != `{"query": "больницы Алматы"}` {
		t.Fatalf("tool call = %v", call)
	}
	tool := msgs[3].(map[string]any)
	if tool["role"] != "tool" || tool["tool_call_id"] != "call_1" || !strings.Contains(tool["content"].(string), "hospitals_almaty") {
		t.Fatalf("tool message = %v", tool)
	}
}

func TestOpenAICompatibleInvalidToolInput(t *testing.T) {
	api := &fakeChatCompletions{turns: [][]string{
		{
			chunk(`{"tool_calls":[{"index":0,"id":"c1","function":{"name":"search_datasets","arguments":"{\"query\": "}}]}`, "tool_calls"),
		},
		{chunk(`{"content":"Sorry."}`, "stop")},
	}}
	srv := httptest.NewServer(api)
	defer srv.Close()
	cat := newFakeCatalog()
	var results []Event
	_, err := New(deepseekConfig(srv.URL), &Tools{Catalog: cat}).Run(context.Background(),
		[]Message{{Role: "user", Content: "q"}}, func(e Event) {
			if e.Type == "tool_result" {
				results = append(results, e)
			}
		})
	if err != nil {
		t.Fatal(err)
	}
	if len(cat.queries) != 0 || len(results) != 1 || !results[0].IsError {
		t.Fatalf("truncated JSON must come back as an error result, not run: %+v", results)
	}
}

func TestOpenAICompatibleContentFilter(t *testing.T) {
	api := &fakeChatCompletions{turns: [][]string{
		{
			chunk(`{"content":"partial"}`, ""),
			chunk(`{"tool_calls":[{"index":0,"id":"c1","function":{"name":"search_datasets","arguments":"{\"query\":\"x\"}"}}]}`, ""),
			chunk(`{}`, "content_filter"),
		},
	}}
	srv := httptest.NewServer(api)
	defer srv.Close()
	cat := newFakeCatalog()
	_, err := New(deepseekConfig(srv.URL), &Tools{Catalog: cat}).Run(context.Background(),
		[]Message{{Role: "user", Content: "q"}}, func(Event) {})
	if err != ErrRefused || len(cat.queries) != 0 {
		t.Fatalf("err = %v, queries = %d", err, len(cat.queries))
	}
}

func TestOpenAICompatibleStreamCutOff(t *testing.T) {
	api := &fakeChatCompletions{turns: [][]string{{chunk(`{"content":"half an ans"}`, "")}}}
	srv := httptest.NewServer(api)
	defer srv.Close()
	_, err := New(deepseekConfig(srv.URL), &Tools{Catalog: newFakeCatalog()}).Run(context.Background(),
		[]Message{{Role: "user", Content: "q"}}, func(Event) {})
	if err == nil || !strings.Contains(err.Error(), "ended before") {
		t.Fatalf("err = %v", err)
	}
}

func TestFromEnvPicksProvider(t *testing.T) {
	clear := func() {
		for _, k := range []string{"LLM_PROVIDER", "DEEPSEEK_API_KEY", "OPENROUTER_API_KEY", "LLM_API_KEY", "LLM_BASE_URL",
			"AGENT_MODEL", "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_PROFILE", "ANTHROPIC_FEDERATION_RULE_ID"} {
			t.Setenv(k, "")
		}
		t.Setenv("ANTHROPIC_CONFIG_DIR", t.TempDir()) // no ant profile
	}

	clear()
	t.Setenv("DEEPSEEK_API_KEY", "sk-ds")
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant") // DeepSeek wins when both are set
	cfg := FromEnv()
	if cfg.Provider != ProviderDeepSeek || cfg.Model != "deepseek-flash" || cfg.BaseURL != "https://api.deepseek.com" || cfg.APIKey != "sk-ds" {
		t.Fatalf("deepseek: %+v", cfg)
	}

	clear()
	t.Setenv("OPENROUTER_API_KEY", "sk-or")
	t.Setenv("AGENT_MODEL", "openrouter/free")
	if cfg := FromEnv(); cfg.Provider != ProviderOpenRouter || cfg.Model != "openrouter/free" || cfg.Validate() != nil {
		t.Fatalf("openrouter: %+v", cfg)
	}

	clear()
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant")
	if cfg := FromEnv(); cfg.Provider != ProviderAnthropic || cfg.Model != "claude-opus-5-5" {
		t.Fatalf("anthropic: %+v", cfg)
	}

	clear()
	if cfg := FromEnv(); cfg.Provider != "" || cfg.Validate() == nil {
		t.Fatalf("no credentials should disable the agent: %+v", cfg)
	}

	clear()
	t.Setenv("LLM_PROVIDER", "openai-compatible")
	t.Setenv("LLM_API_KEY", "k")
	if err := FromEnv().Validate(); err == nil {
		t.Fatal("openai-compatible without base URL and model must not validate")
	}
}

func TestOpenAICompatibleDailyQuotaIsErrQuota(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		http.Error(w, `{"error":{"message":"Rate limit exceeded: free-models-per-day","code":429}}`, http.StatusTooManyRequests)
	}))
	defer srv.Close()
	_, err := New(deepseekConfig(srv.URL), &Tools{Catalog: newFakeCatalog()}).Run(context.Background(),
		[]Message{{Role: "user", Content: "q"}}, func(Event) {})
	if !errors.Is(err, ErrQuota) || calls != 1 {
		t.Fatalf("err = %v after %d calls; a daily quota should fail fast with ErrQuota", err, calls)
	}
}

func TestOpenAICompatibleMidStreamRateLimitIsErrQuota(t *testing.T) {
	api := &fakeChatCompletions{turns: [][]string{{
		`{"error":{"message":"some-model:free is temporarily rate-limited upstream. Please retry shortly"}}`,
	}}}
	srv := httptest.NewServer(api)
	defer srv.Close()
	_, err := New(deepseekConfig(srv.URL), &Tools{Catalog: newFakeCatalog()}).Run(context.Background(),
		[]Message{{Role: "user", Content: "q"}}, func(Event) {})
	if !errors.Is(err, ErrQuota) {
		t.Fatalf("err = %v, want ErrQuota", err)
	}
}
