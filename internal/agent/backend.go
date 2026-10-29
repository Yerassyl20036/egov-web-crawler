package agent

import (
	"context"
	"encoding/json"
)

// backend is one LLM provider. It turns the chat history into a
// provider-specific conversation that the shared loop in Agent.Run drives.
type backend interface {
	start(history []Message) conversation
}

// conversation holds the provider-specific message history of one question.
type conversation interface {
	// next streams one model turn, emitting text, thinking and fallback
	// events, and appends the model's message to the history.
	next(ctx context.Context, emit func(Event)) (turn, error)
	// addToolResults appends the results of the last turn's tool calls.
	addToolResults(results []toolResult)
}

type stopReason string

// Stop reasons, named like the Anthropic API's for continuity in the UI.
const (
	stopEnd       stopReason = "end_turn"
	stopToolUse   stopReason = "tool_use"
	stopRefusal   stopReason = "refusal"
	stopMaxTokens stopReason = "max_tokens"
	stopPause     stopReason = "pause_turn"
)

// turn is what one model response produced.
type turn struct {
	stop    stopReason
	text    string // answer text of this response
	calls   []pendingCall
	model   string
	usage   Usage
	refusal string // user-facing explanation when stop == stopRefusal
}

type pendingCall struct {
	id, name string
	input    json.RawMessage
}

type toolResult struct {
	id      string
	content string
	isError bool
}
