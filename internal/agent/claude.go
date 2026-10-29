package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/shared/constant"
)

// claudeBackend calls Claude through the Anthropic Go SDK: streaming,
// adaptive thinking, effort, eager tool-input streaming and server-side
// refusal fallbacks.
type claudeBackend struct {
	client anthropic.Client
	cfg    Config
	tools  []anthropic.BetaToolUnionParam
}

func newClaudeBackend(cfg Config, specs []ToolSpec, opts ...option.RequestOption) *claudeBackend {
	tools := make([]anthropic.BetaToolUnionParam, len(specs))
	for i, s := range specs {
		tools[i] = anthropic.BetaToolUnionParam{OfTool: &anthropic.BetaToolParam{
			Name:        s.Name,
			Description: anthropic.String(s.Description),
			InputSchema: anthropic.BetaToolInputSchemaParam{Properties: s.Properties, Required: s.Required},
			// Inputs stream as generated; Tools.Execute validates them.
			EagerInputStreaming: anthropic.Bool(true),
		}}
	}
	// The SDK resolves credentials itself (see CredentialsConfigured).
	return &claudeBackend{client: anthropic.NewClient(opts...), cfg: cfg, tools: tools}
}

func (b *claudeBackend) start(history []Message) conversation {
	msgs := make([]anthropic.BetaMessageParam, 0, len(history))
	for _, m := range history {
		text := strings.TrimSpace(m.Content)
		if m.Role == "assistant" {
			msgs = append(msgs, anthropic.BetaMessageParam{
				Role:    anthropic.BetaMessageParamRoleAssistant,
				Content: []anthropic.BetaContentBlockParamUnion{anthropic.NewBetaTextBlock(text)},
			})
		} else {
			msgs = append(msgs, anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock(text)))
		}
	}
	return &claudeConversation{b: b, messages: msgs}
}

type claudeConversation struct {
	b        *claudeBackend
	messages []anthropic.BetaMessageParam
}

func (b *claudeBackend) params(messages []anthropic.BetaMessageParam) anthropic.BetaMessageNewParams {
	p := anthropic.BetaMessageNewParams{
		Model:     anthropic.Model(b.cfg.Model),
		MaxTokens: b.cfg.MaxTokens,
		System:    []anthropic.BetaTextBlockParam{{Text: systemPrompt}},
		Messages:  messages,
		Tools:     b.tools,
		Thinking: anthropic.BetaThinkingConfigParamUnion{OfAdaptive: &anthropic.BetaThinkingConfigAdaptiveParam{
			Display: anthropic.BetaThinkingConfigAdaptiveDisplay(b.cfg.ThinkingDisplay),
		}},
		OutputConfig: anthropic.BetaOutputConfigParam{Effort: anthropic.BetaOutputConfigEffort(b.cfg.Effort)},
		// Automatic caching: each loop iteration re-sends the growing history.
		CacheControl: anthropic.NewBetaCacheControlEphemeralParam(),
	}
	if b.cfg.ThinkingDisplay == string(anthropic.BetaThinkingConfigAdaptiveDisplayUpdates) {
		p.Betas = append(p.Betas, anthropic.AnthropicBetaThinkingDisplayUpdates2026_08_18)
	}
	if b.cfg.Fallbacks {
		// On a safety-classifier decline the API re-runs the request on the
		// model Anthropic recommends for that refusal category.
		p.Fallbacks = anthropic.BetaFallbacksParamUnion{OfDefault: constant.ValueOf[constant.Default]()}
		p.Betas = append(p.Betas, anthropic.AnthropicBetaServerSideFallback2026_07_01)
	}
	return p
}

func (c *claudeConversation) next(ctx context.Context, emit func(Event)) (turn, error) {
	stream := c.b.client.Beta.Messages.NewStreaming(ctx, c.b.params(c.messages))
	defer stream.Close()

	var msg anthropic.BetaMessage
	for stream.Next() {
		event := stream.Current()
		if err := msg.Accumulate(event); err != nil {
			return turn{}, fmt.Errorf("agent: accumulate stream: %w", err)
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
		return turn{}, fmt.Errorf("agent: %w", err)
	}
	c.messages = append(c.messages, msg.ToParam())

	t := turn{
		model: string(msg.Model),
		usage: Usage{
			InputTokens:     msg.Usage.InputTokens,
			OutputTokens:    msg.Usage.OutputTokens,
			CacheReadTokens: msg.Usage.CacheReadInputTokens,
		},
	}
	var text strings.Builder
	for _, block := range msg.Content {
		switch b := block.AsAny().(type) {
		case anthropic.BetaTextBlock:
			text.WriteString(b.Text)
		case anthropic.BetaToolUseBlock:
			t.calls = append(t.calls, pendingCall{id: b.ID, name: b.Name, input: json.RawMessage(b.JSON.Input.Raw())})
		}
	}
	t.text = text.String()

	switch msg.StopReason {
	case anthropic.BetaStopReasonRefusal:
		t.stop = stopRefusal
		t.refusal = claudeRefusalText(msg)
	case anthropic.BetaStopReasonToolUse:
		t.stop = stopToolUse
	case anthropic.BetaStopReasonPauseTurn:
		t.stop = stopPause
	case anthropic.BetaStopReasonMaxTokens:
		t.stop = stopMaxTokens
	default:
		t.stop = stopEnd
	}
	return t, nil
}

// addToolResults sends every result in a single user message, which keeps
// Claude making parallel calls.
func (c *claudeConversation) addToolResults(results []toolResult) {
	blocks := make([]anthropic.BetaContentBlockParamUnion, len(results))
	for i, r := range results {
		blocks[i] = anthropic.NewBetaToolResultBlock(r.id, r.content, r.isError)
	}
	c.messages = append(c.messages, anthropic.NewBetaUserMessage(blocks...))
}

func claudeRefusalText(msg anthropic.BetaMessage) string {
	text := "The model declined to answer this request."
	if c := string(msg.StopDetails.Category); c != "" {
		text += " Category: " + c + "."
	}
	if msg.StopDetails.Explanation != "" {
		text += " " + msg.StopDetails.Explanation
	}
	return text
}
