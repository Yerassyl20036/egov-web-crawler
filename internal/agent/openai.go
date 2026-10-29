package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// openAIBackend speaks the OpenAI Chat Completions streaming protocol, which
// DeepSeek, OpenRouter and most other providers implement. Plain HTTP keeps
// provider quirks visible: DeepSeek's thinking mode wants the previous
// turns' reasoning_content sent back when tools are in play.
type openAIBackend struct {
	cfg   Config
	tools []oaTool
	http  *http.Client
}

func newOpenAIBackend(cfg Config, specs []ToolSpec) *openAIBackend {
	tools := make([]oaTool, len(specs))
	for i, s := range specs {
		params := map[string]any{"type": "object", "properties": s.Properties}
		if len(s.Required) > 0 {
			params["required"] = s.Required
		}
		tools[i] = oaTool{Type: "function", Function: oaFunction{Name: s.Name, Description: s.Description, Parameters: params}}
	}
	// No client timeout: answers stream for as long as the request context allows.
	return &openAIBackend{cfg: cfg, tools: tools, http: &http.Client{}}
}

type oaTool struct {
	Type     string     `json:"type"`
	Function oaFunction `json:"function"`
}

type oaFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters"`
}

type oaMessage struct {
	Role             string       `json:"role"`
	Content          string       `json:"content"`
	ReasoningContent string       `json:"reasoning_content,omitempty"`
	ToolCalls        []oaToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string       `json:"tool_call_id,omitempty"`
}

type oaToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type oaRequest struct {
	Model           string          `json:"model"`
	Messages        []oaMessage     `json:"messages"`
	Tools           []oaTool        `json:"tools,omitempty"`
	Stream          bool            `json:"stream"`
	StreamOptions   map[string]bool `json:"stream_options,omitempty"`
	MaxTokens       int64           `json:"max_tokens,omitempty"`
	ReasoningEffort string          `json:"reasoning_effort,omitempty"`
	Thinking        map[string]any  `json:"thinking,omitempty"`
}

type oaChunk struct {
	Model   string `json:"model"`
	Choices []struct {
		Delta struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"` // DeepSeek
			Reasoning        string `json:"reasoning"`         // OpenRouter
			ToolCalls        []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens         int64 `json:"prompt_tokens"`
		CompletionTokens     int64 `json:"completion_tokens"`
		PromptCacheHitTokens int64 `json:"prompt_cache_hit_tokens"` // DeepSeek
		PromptTokensDetails  *struct {
			CachedTokens int64 `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func (b *openAIBackend) start(history []Message) conversation {
	msgs := []oaMessage{{Role: "system", Content: systemPrompt}}
	for _, m := range history {
		msgs = append(msgs, oaMessage{Role: m.Role, Content: strings.TrimSpace(m.Content)})
	}
	return &openAIConversation{b: b, messages: msgs}
}

type openAIConversation struct {
	b        *openAIBackend
	messages []oaMessage
}

func (b *openAIBackend) request(messages []oaMessage) oaRequest {
	req := oaRequest{
		Model:         b.cfg.Model,
		Messages:      messages,
		Tools:         b.tools,
		Stream:        true,
		StreamOptions: map[string]bool{"include_usage": true},
		MaxTokens:     b.cfg.MaxTokens,
	}
	if b.cfg.Provider == ProviderDeepSeek {
		req.Thinking = map[string]any{"type": b.cfg.Thinking}
		if b.cfg.Thinking != "disabled" {
			req.ReasoningEffort = b.cfg.Effort
		}
	}
	return req
}

func (c *openAIConversation) next(ctx context.Context, emit func(Event)) (turn, error) {
	body, err := c.b.open(ctx, c.b.request(c.messages))
	if err != nil {
		return turn{}, err
	}
	defer body.Close()

	type partial struct {
		id, name string
		args     strings.Builder
	}
	var (
		t         turn
		text      strings.Builder
		reasoning strings.Builder
		calls     = map[int]*partial{}
		finish    string
		sawFinish bool
	)
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || strings.HasPrefix(line, ":") {
			continue // blank separators and keep-alive comments
		}
		data, ok := strings.CutPrefix(line, "data:")
		if !ok {
			continue
		}
		data = strings.TrimSpace(data)
		if data == "[DONE]" {
			break
		}
		var ch oaChunk
		if err := json.Unmarshal([]byte(data), &ch); err != nil {
			return turn{}, fmt.Errorf("agent: bad stream chunk: %w", err)
		}
		if ch.Error != nil {
			err := fmt.Errorf("agent: %s: %s", c.b.cfg.Provider, ch.Error.Message)
			if isRateLimitMessage(ch.Error.Message) {
				err = fmt.Errorf("%w: %v", ErrQuota, err) // e.g. a free model busy upstream
			}
			return turn{}, err
		}
		if ch.Model != "" {
			t.model = ch.Model
		}
		if u := ch.Usage; u != nil {
			t.usage = Usage{InputTokens: u.PromptTokens, OutputTokens: u.CompletionTokens, CacheReadTokens: u.PromptCacheHitTokens}
			if u.PromptTokensDetails != nil && t.usage.CacheReadTokens == 0 {
				t.usage.CacheReadTokens = u.PromptTokensDetails.CachedTokens
			}
		}
		for _, choice := range ch.Choices {
			d := choice.Delta
			if r := d.ReasoningContent + d.Reasoning; r != "" {
				reasoning.WriteString(r)
				emit(Event{Type: "thinking", Text: r})
			}
			if d.Content != "" {
				text.WriteString(d.Content)
				emit(Event{Type: "text", Text: d.Content})
			}
			for _, tc := range d.ToolCalls {
				p := calls[tc.Index]
				if p == nil {
					p = &partial{}
					calls[tc.Index] = p
				}
				if tc.ID != "" {
					p.id = tc.ID
				}
				if tc.Function.Name != "" {
					p.name = tc.Function.Name
				}
				p.args.WriteString(tc.Function.Arguments)
			}
			if choice.FinishReason != nil && *choice.FinishReason != "" {
				finish, sawFinish = *choice.FinishReason, true
			}
		}
	}
	if err := sc.Err(); err != nil {
		return turn{}, fmt.Errorf("agent: read stream: %w", err)
	}
	if !sawFinish {
		return turn{}, errors.New("agent: the model stream ended before the response finished")
	}

	indexes := make([]int, 0, len(calls))
	for i := range calls {
		indexes = append(indexes, i)
	}
	sort.Ints(indexes)
	assistant := oaMessage{Role: "assistant", Content: text.String()}
	if c.b.cfg.Provider == ProviderDeepSeek {
		// DeepSeek thinking mode with tools: reasoning must be passed back.
		assistant.ReasoningContent = reasoning.String()
	}
	for n, i := range indexes {
		p := calls[i]
		if p.id == "" {
			p.id = fmt.Sprintf("call_%d", n)
		}
		var tc oaToolCall
		tc.ID, tc.Type = p.id, "function"
		tc.Function.Name, tc.Function.Arguments = p.name, p.args.String()
		assistant.ToolCalls = append(assistant.ToolCalls, tc)
		t.calls = append(t.calls, pendingCall{id: p.id, name: p.name, input: json.RawMessage(p.args.String())})
	}
	c.messages = append(c.messages, assistant)
	t.text = text.String()

	switch {
	case finish == "content_filter":
		t.stop = stopRefusal
		t.refusal = "The provider's content filter blocked this response."
	case finish == "length":
		t.stop = stopMaxTokens
	case len(t.calls) > 0:
		t.stop = stopToolUse
	default:
		t.stop = stopEnd
	}
	return t, nil
}

// addToolResults appends one "tool" message per call, in call order.
func (c *openAIConversation) addToolResults(results []toolResult) {
	for _, r := range results {
		c.messages = append(c.messages, oaMessage{Role: "tool", ToolCallID: r.id, Content: r.content})
	}
}

// open POSTs the request and returns the SSE body, retrying rate limits
// and server errors that happen before the stream starts.
func (b *openAIBackend) open(ctx context.Context, req oaRequest) (io.ReadCloser, error) {
	payload, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(attempt*attempt) * time.Second):
			}
		}
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, b.cfg.BaseURL+"/chat/completions", bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Accept", "text/event-stream")
		httpReq.Header.Set("Authorization", "Bearer "+b.cfg.APIKey)
		if b.cfg.Provider == ProviderOpenRouter {
			httpReq.Header.Set("X-Title", "Ask data.egov.kz")
		}
		resp, err := b.http.Do(httpReq)
		if err != nil {
			lastErr = fmt.Errorf("agent: %s: %w", b.cfg.Provider, err)
			continue
		}
		if resp.StatusCode == http.StatusOK {
			return resp.Body, nil
		}
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		resp.Body.Close()
		lastErr = fmt.Errorf("agent: %s returned %d: %s", b.cfg.Provider, resp.StatusCode, strings.TrimSpace(string(msg)))
		switch {
		case resp.StatusCode == http.StatusPaymentRequired:
			return nil, fmt.Errorf("%w: %v", ErrQuota, lastErr) // empty balance
		case resp.StatusCode == http.StatusTooManyRequests:
			if bytes.Contains(bytes.ToLower(msg), []byte("per-day")) {
				return nil, fmt.Errorf("%w: %v", ErrQuota, lastErr) // daily quota: retrying won't help
			}
			lastErr = fmt.Errorf("%w: %v", ErrQuota, lastErr)
		case resp.StatusCode < 500:
			return nil, lastErr // other 4xx won't get better on retry
		}
	}
	return nil, lastErr
}

func isRateLimitMessage(msg string) bool {
	msg = strings.ToLower(msg)
	for _, marker := range []string{"rate-limit", "rate limit", "quota", "insufficient balance"} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}
