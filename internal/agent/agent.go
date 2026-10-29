// Package agent runs the tool-use loop behind the chat endpoint: stream a
// model turn, execute the requested catalog tools, feed the results back,
// repeat until the model answers.
//
// The loop is provider-neutral. Two backends implement the model side:
// Claude through the Anthropic Go SDK (claude.go) and any OpenAI-compatible
// Chat Completions API such as DeepSeek or OpenRouter (openai.go).
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/anthropics/anthropic-sdk-go/config"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// Providers.
const (
	ProviderAnthropic        = "anthropic"
	ProviderDeepSeek         = "deepseek"
	ProviderOpenRouter       = "openrouter"
	ProviderOpenAICompatible = "openai-compatible"
)

// Config controls the model call. Every field has an env var (see FromEnv).
type Config struct {
	Provider string // LLM_PROVIDER; empty when no credentials are configured
	Model    string // AGENT_MODEL (default depends on the provider)
	BaseURL  string // LLM_BASE_URL, for OpenAI-compatible providers
	APIKey   string `json:"-"` // DEEPSEEK_API_KEY / OPENROUTER_API_KEY / LLM_API_KEY; never logged

	Effort    string // AGENT_EFFORT: Claude output_config.effort, DeepSeek reasoning_effort
	Thinking  string // LLM_THINKING: enabled | disabled (DeepSeek thinking mode)
	MaxTokens int64  // AGENT_MAX_TOKENS, per model response
	MaxTurns  int    // AGENT_MAX_TURNS, tool-loop iterations per question

	// Claude only.
	ThinkingDisplay string // AGENT_THINKING_DISPLAY: updates | summarized | omitted
	Fallbacks       bool   // AGENT_FALLBACKS: server-side refusal fallbacks (default on)
}

var providerDefaults = map[string]struct{ model, baseURL, keyEnv string }{
	ProviderAnthropic:        {model: "claude-opus-5-5"},
	ProviderDeepSeek:         {model: "deepseek-flash", baseURL: "https://api.deepseek.com", keyEnv: "DEEPSEEK_API_KEY"},
	ProviderOpenRouter:       {model: "deepseek/deepseek-v4.1-flash", baseURL: "https://openrouter.ai/api/v1", keyEnv: "OPENROUTER_API_KEY"},
	ProviderOpenAICompatible: {keyEnv: "LLM_API_KEY"},
}

// FromEnv reads the configuration. Without LLM_PROVIDER it picks the first
// provider that has credentials: DeepSeek, OpenRouter, a generic
// OpenAI-compatible endpoint, then Anthropic.
func FromEnv() Config {
	cfg := Config{
		Provider:        strings.ToLower(strings.TrimSpace(os.Getenv("LLM_PROVIDER"))),
		Effort:          envOr("AGENT_EFFORT", "medium"),
		Thinking:        envOr("LLM_THINKING", "enabled"),
		ThinkingDisplay: envOr("AGENT_THINKING_DISPLAY", "updates"),
		MaxTokens:       16000,
		MaxTurns:        8,
		Fallbacks:       os.Getenv("AGENT_FALLBACKS") != "false",
	}
	if cfg.Provider == "" {
		switch {
		case os.Getenv("DEEPSEEK_API_KEY") != "":
			cfg.Provider = ProviderDeepSeek
		case os.Getenv("OPENROUTER_API_KEY") != "":
			cfg.Provider = ProviderOpenRouter
		case os.Getenv("LLM_API_KEY") != "" && os.Getenv("LLM_BASE_URL") != "":
			cfg.Provider = ProviderOpenAICompatible
		case CredentialsConfigured():
			cfg.Provider = ProviderAnthropic
		}
	}
	if d, ok := providerDefaults[cfg.Provider]; ok {
		cfg.Model = envOr("AGENT_MODEL", d.model)
		cfg.BaseURL = strings.TrimRight(envOr("LLM_BASE_URL", d.baseURL), "/")
		if d.keyEnv != "" {
			cfg.APIKey = envOr(d.keyEnv, os.Getenv("LLM_API_KEY"))
		}
	}
	if n, err := strconv.ParseInt(os.Getenv("AGENT_MAX_TOKENS"), 10, 64); err == nil && n > 0 {
		cfg.MaxTokens = n
	}
	if n, err := strconv.Atoi(os.Getenv("AGENT_MAX_TURNS")); err == nil && n > 0 {
		cfg.MaxTurns = n
	}
	return cfg
}

// Validate reports configuration that cannot work.
func (c Config) Validate() error {
	switch c.Provider {
	case "":
		return errors.New("agent: no LLM credentials (set DEEPSEEK_API_KEY, OPENROUTER_API_KEY, LLM_API_KEY + LLM_BASE_URL, or ANTHROPIC_API_KEY)")
	case ProviderAnthropic:
		return nil
	case ProviderDeepSeek, ProviderOpenRouter, ProviderOpenAICompatible:
		if c.APIKey == "" || c.BaseURL == "" || c.Model == "" {
			return fmt.Errorf("agent: provider %s needs an API key, a base URL and a model", c.Provider)
		}
		return nil
	default:
		return fmt.Errorf("agent: unknown LLM_PROVIDER %q", c.Provider)
	}
}

// CredentialsConfigured reports whether the Anthropic SDK's default
// credential chain has something to use: ANTHROPIC_API_KEY,
// ANTHROPIC_AUTH_TOKEN, a profile from `ant auth login`, or workload
// identity federation.
func CredentialsConfigured() bool {
	for _, key := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_PROFILE", "ANTHROPIC_FEDERATION_RULE_ID"} {
		if os.Getenv(key) != "" {
			return true
		}
	}
	_, err := config.LoadConfig()
	return err == nil
}

// Message is one turn of the chat history sent by the client.
type Message struct {
	Role    string `json:"role"` // "user" or "assistant"
	Content string `json:"content"`
}

// Event is streamed to the UI as the agent works.
type Event struct {
	Type string `json:"type"` // thinking | text | tool_call | tool_result | fallback | refusal | done | error

	Text string `json:"text,omitempty"`

	ID       string          `json:"id,omitempty"`
	Name     string          `json:"name,omitempty"`
	Input    json.RawMessage `json:"input,omitempty"`
	Summary  string          `json:"summary,omitempty"`
	Datasets []DatasetRef    `json:"datasets,omitempty"`
	IsError  bool            `json:"is_error,omitempty"`

	Done *Result `json:"done,omitempty"`
}

// ToolCall records one executed tool call (used by evals and the UI).
type ToolCall struct {
	Name     string          `json:"name"`
	Input    json.RawMessage `json:"input"`
	Datasets []DatasetRef    `json:"datasets,omitempty"`
	IsError  bool            `json:"is_error,omitempty"`
}

// Citation is a dataset link found in the answer, checked against the catalog.
type Citation struct {
	DatasetRef
	Verified bool `json:"verified"` // false = the model linked an api_uri that is not in the catalog
}

// Usage sums token usage over every model call of one question.
type Usage struct {
	InputTokens     int64 `json:"input_tokens"`
	OutputTokens    int64 `json:"output_tokens"`
	CacheReadTokens int64 `json:"cache_read_tokens"`
}

// Result is the outcome of answering one question.
type Result struct {
	Answer     string     `json:"answer"`
	StopReason string     `json:"stop_reason"`
	Provider   string     `json:"provider"`
	Model      string     `json:"model"`
	Turns      int        `json:"turns"`
	ToolCalls  []ToolCall `json:"tool_calls"`
	Citations  []Citation `json:"citations"`
	Usage      Usage      `json:"usage"`
	DurationMS int64      `json:"duration_ms"`
}

// ErrRefused is returned when the model (and any fallback model) declined.
var ErrRefused = errors.New("agent: the request was declined")

// Agent answers catalog questions with an LLM and the catalog tools.
type Agent struct {
	cfg   Config
	tools *Tools
	llm   backend
}

// New builds an agent for cfg.Provider (Anthropic when empty). opts are
// passed to the Anthropic client and ignored by other providers.
func New(cfg Config, tools *Tools, opts ...option.RequestOption) *Agent {
	a := &Agent{cfg: cfg, tools: tools}
	switch cfg.Provider {
	case ProviderDeepSeek, ProviderOpenRouter, ProviderOpenAICompatible:
		a.llm = newOpenAIBackend(cfg, tools.Specs())
	default:
		a.cfg.Provider = ProviderAnthropic
		a.llm = newClaudeBackend(cfg, tools.Specs(), opts...)
	}
	return a
}

// Config returns the agent's configuration.
func (a *Agent) Config() Config { return a.cfg }

// Run answers the last user message of history, streaming events to emit.
// emit is called from a single goroutine.
func (a *Agent) Run(ctx context.Context, history []Message, emit func(Event)) (*Result, error) {
	start := time.Now()
	if err := validateHistory(history); err != nil {
		return nil, err
	}
	conv := a.llm.start(history)
	res := &Result{Provider: a.cfg.Provider, Model: a.cfg.Model}
	defer func() { res.DurationMS = time.Since(start).Milliseconds() }()

	for res.Turns < a.cfg.MaxTurns {
		res.Turns++
		t, err := conv.next(ctx, emit)
		if err != nil {
			return res, err
		}
		if t.model != "" {
			res.Model = t.model
		}
		res.StopReason = string(t.stop)
		res.Usage.InputTokens += t.usage.InputTokens
		res.Usage.OutputTokens += t.usage.OutputTokens
		res.Usage.CacheReadTokens += t.usage.CacheReadTokens

		switch t.stop {
		case stopRefusal:
			// A refusal can cut a tool call off mid-input: never run it.
			emit(Event{Type: "refusal", Text: t.refusal})
			return res, ErrRefused
		case stopToolUse:
			conv.addToolResults(a.runTools(ctx, t.calls, res, emit))
			continue
		case stopPause:
			continue
		case stopMaxTokens:
			if len(t.calls) > 0 {
				// The tool input is probably truncated; don't execute it.
				return res, fmt.Errorf("agent: response hit max_tokens (%d) inside a tool call", a.cfg.MaxTokens)
			}
		}
		res.Answer = strings.TrimSpace(t.text)
		res.Citations = a.citations(ctx, res.Answer)
		return res, nil
	}
	return res, fmt.Errorf("agent: no answer after %d steps", a.cfg.MaxTurns)
}

// runTools executes the requested tool calls concurrently and returns their
// results in the original order.
func (a *Agent) runTools(ctx context.Context, calls []pendingCall, res *Result, emit func(Event)) []toolResult {
	for _, c := range calls {
		emit(Event{Type: "tool_call", ID: c.id, Name: c.name, Input: displayInput(c.input)})
	}
	outcomes := make([]ToolOutcome, len(calls))
	var wg sync.WaitGroup
	for i, c := range calls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			toolCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			outcomes[i] = a.tools.Execute(toolCtx, c.name, c.input)
		}()
	}
	wg.Wait()

	results := make([]toolResult, len(calls))
	for i, c := range calls {
		o := outcomes[i]
		emit(Event{Type: "tool_result", ID: c.id, Name: c.name, Summary: o.Summary, Datasets: o.Datasets, IsError: o.IsError})
		res.ToolCalls = append(res.ToolCalls, ToolCall{Name: c.name, Input: displayInput(c.input), Datasets: o.Datasets, IsError: o.IsError})
		results[i] = toolResult{id: c.id, content: o.Content, isError: o.IsError}
	}
	return results
}

// displayInput keeps invalid JSON from breaking the event stream: the raw
// text is wrapped in a JSON string.
func displayInput(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage("{}")
	}
	if json.Valid(raw) {
		return raw
	}
	quoted, _ := json.Marshal(string(raw))
	return quoted
}

var datasetLink = regexp.MustCompile(`data\.egov\.kz/datasets/view\?index=([A-Za-z0-9_\-]+)`)

// CitedAPIURIs extracts dataset api_uris linked in an answer, in order.
func CitedAPIURIs(answer string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range datasetLink.FindAllStringSubmatch(answer, -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			out = append(out, m[1])
		}
	}
	return out
}

// citations resolves every linked dataset against the catalog, so the UI can
// flag links the model made up.
func (a *Agent) citations(ctx context.Context, answer string) []Citation {
	uris := CitedAPIURIs(answer)
	found, err := a.tools.Catalog.GetMany(ctx, uris)
	if err != nil {
		found = nil
	}
	out := make([]Citation, 0, len(uris))
	for _, uri := range uris {
		if d, ok := found[uri]; ok {
			out = append(out, Citation{DatasetRef: refOf(d), Verified: true})
		} else {
			out = append(out, Citation{DatasetRef: DatasetRef{APIURI: uri, URL: "https://data.egov.kz/datasets/view?index=" + uri}})
		}
	}
	return out
}

func validateHistory(history []Message) error {
	if len(history) == 0 || history[len(history)-1].Role != "user" {
		return errors.New("agent: history must end with a user message")
	}
	for i, m := range history {
		if strings.TrimSpace(m.Content) == "" {
			return fmt.Errorf("agent: message %d is empty", i)
		}
		if m.Role != "user" && m.Role != "assistant" {
			return fmt.Errorf("agent: message %d has unknown role %q", i, m.Role)
		}
	}
	return nil
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}
