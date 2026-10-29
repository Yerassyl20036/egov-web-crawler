// Package agent runs the Claude tool-use loop behind the chat endpoint:
// stream a turn, execute the requested catalog tools, feed results back,
// repeat until Claude answers.
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

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/config"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/shared/constant"
)

// Config controls the model call. Every field has an env var (see FromEnv).
type Config struct {
	Model           string // AGENT_MODEL
	Effort          string // AGENT_EFFORT: low | medium | high | xhigh | max
	ThinkingDisplay string // AGENT_THINKING_DISPLAY: updates | summarized | omitted
	MaxTokens       int64  // AGENT_MAX_TOKENS, per model response
	MaxTurns        int    // AGENT_MAX_TURNS, tool-loop iterations per question
	Fallbacks       bool   // AGENT_FALLBACKS: server-side refusal fallbacks (default on)
}

// FromEnv reads the configuration, falling back to defaults.
func FromEnv() Config {
	cfg := Config{
		Model:           envOr("AGENT_MODEL", "claude-opus-5-5"),
		Effort:          envOr("AGENT_EFFORT", "medium"),
		ThinkingDisplay: envOr("AGENT_THINKING_DISPLAY", "updates"),
		MaxTokens:       16000,
		MaxTurns:        8,
		Fallbacks:       os.Getenv("AGENT_FALLBACKS") != "false",
	}
	if n, err := strconv.ParseInt(os.Getenv("AGENT_MAX_TOKENS"), 10, 64); err == nil && n > 0 {
		cfg.MaxTokens = n
	}
	if n, err := strconv.Atoi(os.Getenv("AGENT_MAX_TURNS")); err == nil && n > 0 {
		cfg.MaxTurns = n
	}
	return cfg
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
	Model      string     `json:"model"`
	Turns      int        `json:"turns"`
	ToolCalls  []ToolCall `json:"tool_calls"`
	Citations  []Citation `json:"citations"`
	Usage      Usage      `json:"usage"`
	DurationMS int64      `json:"duration_ms"`
}

// ErrRefused is returned when Claude (and every fallback model) declined.
var ErrRefused = errors.New("agent: the request was declined")

// Agent answers catalog questions with Claude and the catalog tools.
type Agent struct {
	client anthropic.Client
	cfg    Config
	tools  *Tools
}

// CredentialsConfigured reports whether the SDK's default credential chain
// has something to use: ANTHROPIC_API_KEY, ANTHROPIC_AUTH_TOKEN, a profile
// from `ant auth login`, or workload identity federation.
func CredentialsConfigured() bool {
	for _, key := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_PROFILE", "ANTHROPIC_FEDERATION_RULE_ID"} {
		if os.Getenv(key) != "" {
			return true
		}
	}
	_, err := config.LoadConfig()
	return err == nil
}

// New builds an agent. The SDK resolves credentials itself (see
// CredentialsConfigured).
func New(cfg Config, tools *Tools, opts ...option.RequestOption) *Agent {
	return &Agent{client: anthropic.NewClient(opts...), cfg: cfg, tools: tools}
}

// Config returns the agent's configuration.
func (a *Agent) Config() Config { return a.cfg }

func (a *Agent) params(messages []anthropic.BetaMessageParam) anthropic.BetaMessageNewParams {
	p := anthropic.BetaMessageNewParams{
		Model:     anthropic.Model(a.cfg.Model),
		MaxTokens: a.cfg.MaxTokens,
		System:    []anthropic.BetaTextBlockParam{{Text: systemPrompt}},
		Messages:  messages,
		Tools:     a.tools.Definitions(),
		Thinking: anthropic.BetaThinkingConfigParamUnion{OfAdaptive: &anthropic.BetaThinkingConfigAdaptiveParam{
			Display: anthropic.BetaThinkingConfigAdaptiveDisplay(a.cfg.ThinkingDisplay),
		}},
		OutputConfig: anthropic.BetaOutputConfigParam{Effort: anthropic.BetaOutputConfigEffort(a.cfg.Effort)},
		// Automatic caching: each loop iteration re-sends the growing history.
		CacheControl: anthropic.NewBetaCacheControlEphemeralParam(),
	}
	if a.cfg.ThinkingDisplay == string(anthropic.BetaThinkingConfigAdaptiveDisplayUpdates) {
		p.Betas = append(p.Betas, anthropic.AnthropicBetaThinkingDisplayUpdates2026_08_18)
	}
	if a.cfg.Fallbacks {
		// On a safety-classifier decline the API re-runs the request on the
		// model Anthropic recommends for that refusal category.
		p.Fallbacks = anthropic.BetaFallbacksParamUnion{OfDefault: constant.ValueOf[constant.Default]()}
		p.Betas = append(p.Betas, anthropic.AnthropicBetaServerSideFallback2026_07_01)
	}
	return p
}

// Run answers the last user message of history, streaming events to emit.
// emit is called from a single goroutine.
func (a *Agent) Run(ctx context.Context, history []Message, emit func(Event)) (*Result, error) {
	start := time.Now()
	messages, err := toParams(history)
	if err != nil {
		return nil, err
	}
	res := &Result{Model: a.cfg.Model}
	defer func() { res.DurationMS = time.Since(start).Milliseconds() }()

	for res.Turns < a.cfg.MaxTurns {
		res.Turns++
		msg, err := a.streamTurn(ctx, messages, emit)
		if err != nil {
			return res, err
		}
		messages = append(messages, msg.ToParam())
		res.Model = string(msg.Model)
		res.StopReason = string(msg.StopReason)
		res.Usage.InputTokens += msg.Usage.InputTokens
		res.Usage.OutputTokens += msg.Usage.OutputTokens
		res.Usage.CacheReadTokens += msg.Usage.CacheReadInputTokens

		switch msg.StopReason {
		case anthropic.BetaStopReasonRefusal:
			// A refusal can cut a tool_use block off mid-input: never run it.
			emit(Event{Type: "refusal", Text: refusalText(msg)})
			return res, ErrRefused
		case anthropic.BetaStopReasonToolUse:
			messages = append(messages, a.runTools(ctx, msg, res, emit))
			continue
		case anthropic.BetaStopReasonPauseTurn:
			continue
		case anthropic.BetaStopReasonMaxTokens:
			if hasToolUse(msg) {
				// The tool input is probably truncated; don't execute it.
				return res, fmt.Errorf("agent: response hit max_tokens (%d) inside a tool call", a.cfg.MaxTokens)
			}
		}
		res.Answer = answerText(msg)
		res.Citations = a.citations(ctx, res.Answer)
		return res, nil
	}
	return res, fmt.Errorf("agent: no answer after %d steps", a.cfg.MaxTurns)
}

// streamTurn streams one model response, forwarding text and thinking
// deltas, and returns the accumulated message.
func (a *Agent) streamTurn(ctx context.Context, messages []anthropic.BetaMessageParam, emit func(Event)) (anthropic.BetaMessage, error) {
	stream := a.client.Beta.Messages.NewStreaming(ctx, a.params(messages))
	defer stream.Close()

	var msg anthropic.BetaMessage
	for stream.Next() {
		event := stream.Current()
		if err := msg.Accumulate(event); err != nil {
			return msg, fmt.Errorf("agent: accumulate stream: %w", err)
		}
		switch e := event.AsAny().(type) {
		case anthropic.BetaRawContentBlockStartEvent:
			if fb, ok := e.ContentBlock.AsAny().(anthropic.BetaFallbackBlock); ok {
				emit(Event{Type: "fallback", Text: fmt.Sprintf("%s declined; %s continued", fb.From.Model, fb.To.Model)})
			}
		case anthropic.BetaRawContentBlockDeltaEvent:
			switch d := e.Delta.AsAny().(type) {
			case anthropic.BetaTextDelta:
				emit(Event{Type: "text", Text: d.Text})
			case anthropic.BetaThinkingDelta:
				if d.Thinking != "" {
					emit(Event{Type: "thinking", Text: d.Thinking})
				}
			}
		}
	}
	if err := stream.Err(); err != nil {
		return msg, fmt.Errorf("agent: %w", err)
	}
	return msg, nil
}

// runTools executes every tool_use block of msg concurrently and returns the
// results as a single user message, in the original order.
func (a *Agent) runTools(ctx context.Context, msg anthropic.BetaMessage, res *Result, emit func(Event)) anthropic.BetaMessageParam {
	type call struct {
		id, name string
		input    json.RawMessage
	}
	var calls []call
	for _, block := range msg.Content {
		if tu, ok := block.AsAny().(anthropic.BetaToolUseBlock); ok {
			input := json.RawMessage(tu.JSON.Input.Raw())
			calls = append(calls, call{tu.ID, tu.Name, input})
			emit(Event{Type: "tool_call", ID: tu.ID, Name: tu.Name, Input: input})
		}
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

	results := make([]anthropic.BetaContentBlockParamUnion, len(calls))
	for i, c := range calls {
		o := outcomes[i]
		emit(Event{Type: "tool_result", ID: c.id, Name: c.name, Summary: o.Summary, Datasets: o.Datasets, IsError: o.IsError})
		res.ToolCalls = append(res.ToolCalls, ToolCall{Name: c.name, Input: c.input, Datasets: o.Datasets, IsError: o.IsError})
		results[i] = anthropic.NewBetaToolResultBlock(c.id, o.Content, o.IsError)
	}
	return anthropic.NewBetaUserMessage(results...)
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

func toParams(history []Message) ([]anthropic.BetaMessageParam, error) {
	if len(history) == 0 || history[len(history)-1].Role != "user" {
		return nil, errors.New("agent: history must end with a user message")
	}
	out := make([]anthropic.BetaMessageParam, 0, len(history))
	for i, m := range history {
		text := strings.TrimSpace(m.Content)
		if text == "" {
			return nil, fmt.Errorf("agent: message %d is empty", i)
		}
		switch m.Role {
		case "user":
			out = append(out, anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock(text)))
		case "assistant":
			out = append(out, anthropic.BetaMessageParam{
				Role:    anthropic.BetaMessageParamRoleAssistant,
				Content: []anthropic.BetaContentBlockParamUnion{anthropic.NewBetaTextBlock(text)},
			})
		default:
			return nil, fmt.Errorf("agent: message %d has unknown role %q", i, m.Role)
		}
	}
	return out, nil
}

func answerText(msg anthropic.BetaMessage) string {
	var b strings.Builder
	for _, block := range msg.Content {
		if t, ok := block.AsAny().(anthropic.BetaTextBlock); ok {
			b.WriteString(t.Text)
		}
	}
	return strings.TrimSpace(b.String())
}

func hasToolUse(msg anthropic.BetaMessage) bool {
	for _, block := range msg.Content {
		if _, ok := block.AsAny().(anthropic.BetaToolUseBlock); ok {
			return true
		}
	}
	return false
}

func refusalText(msg anthropic.BetaMessage) string {
	text := "The model declined to answer this request."
	if c := string(msg.StopDetails.Category); c != "" {
		text += " Category: " + c + "."
	}
	if msg.StopDetails.Explanation != "" {
		text += " " + msg.StopDetails.Explanation
	}
	return text
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}
