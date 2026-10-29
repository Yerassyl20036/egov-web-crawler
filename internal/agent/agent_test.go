package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/Yerassyl20036/egov-web-crawler/internal/catalog"
	"github.com/Yerassyl20036/egov-web-crawler/internal/store"
)

type fakeCatalog struct {
	datasets []catalog.Dataset
	queries  []store.SearchParams
}

func (f *fakeCatalog) Search(_ context.Context, p store.SearchParams) ([]store.SearchHit, error) {
	f.queries = append(f.queries, p)
	var hits []store.SearchHit
	for _, d := range f.datasets {
		if strings.Contains(strings.ToLower(d.TitleRu), strings.ToLower(strings.Fields(p.Query)[0])) {
			hits = append(hits, store.SearchHit{Dataset: d, Score: 0.5})
		}
	}
	return hits, nil
}

func (f *fakeCatalog) Get(_ context.Context, uri string) (catalog.Dataset, error) {
	for _, d := range f.datasets {
		if d.APIURI == uri {
			return d, nil
		}
	}
	return catalog.Dataset{}, store.ErrNotFound
}

func (f *fakeCatalog) GetMany(ctx context.Context, uris []string) (map[string]catalog.Dataset, error) {
	out := map[string]catalog.Dataset{}
	for _, u := range uris {
		if d, err := f.Get(ctx, u); err == nil {
			out[u] = d
		}
	}
	return out, nil
}

func (f *fakeCatalog) Categories(context.Context) ([]store.CategoryCount, error) {
	return []store.CategoryCount{{CategoryRu: "Здравоохранение", CategoryEn: "Healthcare", Count: 1}}, nil
}

func newFakeCatalog() *fakeCatalog {
	return &fakeCatalog{datasets: []catalog.Dataset{{
		APIURI: "hospitals_almaty", TitleRu: "Больницы города Алматы", CategoryRu: "Здравоохранение",
		AgencyRu: "Акимат города Алматы", Actualization: "YEARLY",
	}}}
}

func TestExecuteValidatesInput(t *testing.T) {
	tools := &Tools{Catalog: newFakeCatalog()}
	ctx := context.Background()

	cases := []struct {
		name, tool, input string
		wantError         bool
	}{
		{"valid search", "search_datasets", `{"query":"больницы"}`, false},
		{"empty query", "search_datasets", `{"query":"  "}`, true},
		{"truncated json", "search_datasets", `{"query":"боль`, true},
		{"unknown field", "search_datasets", `{"query":"x","sql":"DROP TABLE"}`, true},
		{"limit out of range", "search_datasets", `{"query":"x","limit":500}`, true},
		{"bad api_uri", "get_dataset", `{"api_uri":"../../etc/passwd"}`, true},
		{"missing dataset", "get_dataset", `{"api_uri":"nope"}`, true},
		{"valid get", "get_dataset", `{"api_uri":"hospitals_almaty"}`, false},
		{"categories", "list_categories", ``, false},
		{"preview disabled", "preview_rows", `{"api_uri":"hospitals_almaty"}`, true},
		{"unknown tool", "drop_tables", `{}`, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := tools.Execute(ctx, c.tool, []byte(c.input))
			if out.IsError != c.wantError {
				t.Fatalf("IsError = %v, want %v (content %s)", out.IsError, c.wantError, out.Content)
			}
			if !json.Valid([]byte(out.Content)) {
				t.Fatalf("tool content is not JSON: %s", out.Content)
			}
		})
	}

	out := tools.Execute(ctx, "search_datasets", []byte(`{"query":"больницы"}`))
	if len(out.Datasets) != 1 || !strings.Contains(out.Content, "Ежегодно") {
		t.Fatalf("search should render cadence label and dataset refs: %s", out.Content)
	}
}

func TestDefinitionsHidePreviewWithoutKey(t *testing.T) {
	without := (&Tools{Catalog: newFakeCatalog()}).Specs()
	with := (&Tools{Catalog: newFakeCatalog(), Rows: nopRows{}}).Specs()
	if len(with) != len(without)+1 {
		t.Fatalf("preview_rows should only be offered with a row previewer")
	}
}

type nopRows struct{}

func (nopRows) PreviewRows(context.Context, string, string, int) ([]map[string]any, error) {
	return nil, nil
}

func TestCitedAPIURIs(t *testing.T) {
	answer := "See [Больницы](https://data.egov.kz/datasets/view?index=hospitals_almaty) and " +
		"<https://data.egov.kz/datasets/view?index=schools-2024>, again https://data.egov.kz/datasets/view?index=hospitals_almaty."
	got := CitedAPIURIs(answer)
	if strings.Join(got, ",") != "hospitals_almaty,schools-2024" {
		t.Fatalf("got %v", got)
	}
}

// fakeMessagesAPI serves scripted SSE responses for /v1/messages and records
// every request body.
type fakeMessagesAPI struct {
	mu       sync.Mutex
	turns    [][]string // SSE "data" payloads per request
	requests []map[string]any
	headers  []http.Header
}

func (f *fakeMessagesAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req map[string]any
	json.Unmarshal(body, &req)
	f.mu.Lock()
	i := len(f.requests)
	f.requests = append(f.requests, req)
	f.headers = append(f.headers, r.Header.Clone())
	f.mu.Unlock()
	if i >= len(f.turns) {
		http.Error(w, `{"type":"error","error":{"type":"invalid_request_error","message":"unexpected call"}}`, 400)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	for _, data := range f.turns[i] {
		var ev struct{ Type string }
		json.Unmarshal([]byte(data), &ev)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Type, data)
	}
}

func sseTurn(stopReason string, blocks ...string) []string {
	out := []string{`{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-5-5","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":100,"output_tokens":1}}}`}
	out = append(out, blocks...)
	out = append(out,
		fmt.Sprintf(`{"type":"message_delta","delta":{"stop_reason":%q,"stop_sequence":null},"usage":{"output_tokens":42}}`, stopReason),
		`{"type":"message_stop"}`)
	return out
}

func TestRunToolLoopAgainstFakeAPI(t *testing.T) {
	api := &fakeMessagesAPI{turns: [][]string{
		sseTurn("tool_use",
			`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"Searching for hospitals in Almaty."}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig"}}`,
			`{"type":"content_block_stop","index":0}`,
			`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"search_datasets","input":{}}}`,
			`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"query\": \"больницы Алматы\"}"}}`,
			`{"type":"content_block_stop","index":1}`,
		),
		sseTurn("end_turn",
			`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Try [Больницы](https://data.egov.kz/datasets/view?index=hospitals_almaty)"}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" and [this](https://data.egov.kz/datasets/view?index=made_up)."}}`,
			`{"type":"content_block_stop","index":0}`,
		),
	}}
	srv := httptest.NewServer(api)
	defer srv.Close()

	cat := newFakeCatalog()
	a := New(Config{Model: "claude-opus-5-5", Effort: "medium", ThinkingDisplay: "updates", MaxTokens: 1000, MaxTurns: 4, Fallbacks: true},
		&Tools{Catalog: cat}, option.WithBaseURL(srv.URL), option.WithAPIKey("test-key"), option.WithMaxRetries(0))

	var events []Event
	res, err := a.Run(context.Background(), []Message{{Role: "user", Content: "hospitals in Almaty?"}}, func(e Event) { events = append(events, e) })
	if err != nil {
		t.Fatal(err)
	}

	if len(cat.queries) != 1 || cat.queries[0].Query != "больницы Алматы" {
		t.Fatalf("tool not executed with streamed input: %+v", cat.queries)
	}
	var types []string
	for _, e := range events {
		types = append(types, e.Type)
	}
	if got := strings.Join(types, ","); got != "thinking,tool_call,tool_result,text,text" {
		t.Fatalf("events = %s", got)
	}
	if res.Turns != 2 || len(res.ToolCalls) != 1 || res.Usage.OutputTokens != 84 {
		t.Fatalf("result = %+v", res)
	}
	if len(res.Citations) != 2 || !res.Citations[0].Verified || res.Citations[1].Verified {
		t.Fatalf("citations should verify against the catalog: %+v", res.Citations)
	}

	// Request shape: adaptive thinking, effort, server-side fallbacks,
	// eager input streaming, beta headers.
	first := api.requests[0]
	if first["fallbacks"] != "default" {
		t.Errorf("fallbacks = %v", first["fallbacks"])
	}
	if th := first["thinking"].(map[string]any); th["type"] != "adaptive" || th["display"] != "updates" {
		t.Errorf("thinking = %v", th)
	}
	if oc := first["output_config"].(map[string]any); oc["effort"] != "medium" {
		t.Errorf("output_config = %v", oc)
	}
	tool := first["tools"].([]any)[0].(map[string]any)
	if tool["eager_input_streaming"] != true {
		t.Errorf("tool = %v", tool)
	}
	beta := api.headers[0].Get("anthropic-beta")
	for _, want := range []string{"server-side-fallback-2026-07-01", "thinking-display-updates-2026-08-18"} {
		if !strings.Contains(beta, want) {
			t.Errorf("anthropic-beta %q lacks %s", beta, want)
		}
	}

	// Second request carries the assistant turn and the tool result.
	msgs := api.requests[1]["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("second request has %d messages", len(msgs))
	}
	result := msgs[2].(map[string]any)["content"].([]any)[0].(map[string]any)
	if result["type"] != "tool_result" || result["tool_use_id"] != "toolu_1" {
		t.Fatalf("tool result block = %v", result)
	}
}

func TestRunDoesNotExecuteToolsOnRefusal(t *testing.T) {
	api := &fakeMessagesAPI{turns: [][]string{
		sseTurn("refusal",
			`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"search_datasets","input":{}}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"query\": \"x"}}`,
			`{"type":"content_block_stop","index":0}`,
		),
	}}
	srv := httptest.NewServer(api)
	defer srv.Close()
	cat := newFakeCatalog()
	a := New(Config{Model: "claude-opus-5-5", Effort: "low", ThinkingDisplay: "omitted", MaxTokens: 1000, MaxTurns: 3},
		&Tools{Catalog: cat}, option.WithBaseURL(srv.URL), option.WithAPIKey("k"), option.WithMaxRetries(0))

	var sawRefusal bool
	_, err := a.Run(context.Background(), []Message{{Role: "user", Content: "q"}}, func(e Event) {
		sawRefusal = sawRefusal || e.Type == "refusal"
	})
	if err != ErrRefused || !sawRefusal {
		t.Fatalf("err = %v, sawRefusal = %v", err, sawRefusal)
	}
	if len(cat.queries) != 0 {
		t.Fatal("tools must not run on a refused turn")
	}
	if _, ok := api.requests[0]["fallbacks"]; ok {
		t.Fatal("fallbacks must be omitted when disabled")
	}
}

func TestHistoryValidation(t *testing.T) {
	for _, h := range [][]Message{
		nil,
		{{Role: "assistant", Content: "hi"}},
		{{Role: "user", Content: "  "}},
		{{Role: "system", Content: "x"}, {Role: "user", Content: "q"}},
	} {
		if err := validateHistory(h); err == nil {
			t.Errorf("history %+v should be rejected", h)
		}
	}
}
