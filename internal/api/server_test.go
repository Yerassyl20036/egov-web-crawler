package api

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Yerassyl20036/egov-web-crawler/internal/agent"
	"github.com/Yerassyl20036/egov-web-crawler/internal/catalog"
	"github.com/Yerassyl20036/egov-web-crawler/internal/store"
)

type fakeCatalog struct{}

func (fakeCatalog) Ping(context.Context) error         { return nil }
func (fakeCatalog) Count(context.Context) (int, error) { return 42, nil }
func (fakeCatalog) Get(context.Context, string) (catalog.Dataset, error) {
	return catalog.Dataset{}, store.ErrNotFound
}
func (fakeCatalog) GetMany(context.Context, []string) (map[string]catalog.Dataset, error) {
	return nil, nil
}
func (fakeCatalog) Categories(context.Context) ([]store.CategoryCount, error) { return nil, nil }
func (fakeCatalog) Search(_ context.Context, p store.SearchParams) ([]store.SearchHit, error) {
	if p.Query == "" {
		return nil, store.ErrEmptyQuery
	}
	return []store.SearchHit{{Dataset: catalog.Dataset{APIURI: "x", TitleRu: "Школы"}, Score: 1}}, nil
}

type fakeRunner struct{}

func (fakeRunner) Config() agent.Config { return agent.Config{Model: "claude-opus-5-5"} }
func (fakeRunner) Run(_ context.Context, h []agent.Message, emit func(agent.Event)) (*agent.Result, error) {
	emit(agent.Event{Type: "tool_call", ID: "t1", Name: "search_datasets", Input: json.RawMessage(`{"query":"школы"}`)})
	emit(agent.Event{Type: "tool_result", ID: "t1", Name: "search_datasets", Summary: "1 datasets"})
	emit(agent.Event{Type: "text", Text: "Answer"})
	return &agent.Result{Answer: "Answer", Turns: 2}, nil
}

func newTestServer(withAgent bool) *httptest.Server {
	s := &Server{Catalog: fakeCatalog{}, ChatPerMinute: 2}
	if withAgent {
		s.Agent = fakeRunner{}
	}
	return httptest.NewServer(s.Handler())
}

func TestHealth(t *testing.T) {
	srv := newTestServer(false)
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/api/health")
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	json.NewDecoder(resp.Body).Decode(&body)
	if resp.StatusCode != 200 || body["datasets"] != float64(42) {
		t.Fatalf("health = %d %v", resp.StatusCode, body)
	}
	if body["agent"].(map[string]any)["enabled"] != false {
		t.Fatalf("agent should be reported disabled: %v", body)
	}
}

func TestSearch(t *testing.T) {
	srv := newTestServer(false)
	defer srv.Close()
	resp, _ := http.Get(srv.URL + "/api/search?q=школы")
	var body struct {
		Count   int
		Results []map[string]any
	}
	json.NewDecoder(resp.Body).Decode(&body)
	if body.Count != 1 || body.Results[0]["url"] != "https://data.egov.kz/datasets/view?index=x" {
		t.Fatalf("search = %+v", body)
	}
	resp, _ = http.Get(srv.URL + "/api/search")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty search status = %d", resp.StatusCode)
	}
}

func postChat(t *testing.T, url, body string) *http.Response {
	t.Helper()
	resp, err := http.Post(url+"/api/chat", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestChatStreamsEvents(t *testing.T) {
	srv := newTestServer(true)
	defer srv.Close()
	resp := postChat(t, srv.URL, `{"messages":[{"role":"user","content":"schools in Astana"}]}`)
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type = %q", ct)
	}
	var events []string
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		if name, ok := strings.CutPrefix(sc.Text(), "event: "); ok {
			events = append(events, name)
		}
	}
	if got := strings.Join(events, ","); got != "tool_call,tool_result,text,done" {
		t.Fatalf("events = %s", got)
	}
}

func TestChatValidationAndLimits(t *testing.T) {
	srv := newTestServer(true)
	defer srv.Close()
	bad := []string{
		`not json`,
		`{"messages":[]}`,
		`{"messages":[{"role":"assistant","content":"hi"}]}`,
		`{"messages":[{"role":"user","content":"a"},{"role":"user","content":"b"}]}`,
		`{"messages":[{"role":"user","content":"` + strings.Repeat("x", maxMessageLen+1) + `"}]}`,
	}
	for _, b := range bad {
		if resp := postChat(t, srv.URL, b); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("body %.40q: status %d, want 400", b, resp.StatusCode)
		}
	}
	// ChatPerMinute = 2 gives a burst of 1; invalid requests don't count.
	if resp := postChat(t, srv.URL, `{"messages":[{"role":"user","content":"q"}]}`); resp.StatusCode != http.StatusOK {
		t.Errorf("first valid request: status %d", resp.StatusCode)
	}
	if resp := postChat(t, srv.URL, `{"messages":[{"role":"user","content":"q"}]}`); resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("rate limit: status %d, want 429", resp.StatusCode)
	}
}

func TestChatDisabledWithoutAgent(t *testing.T) {
	srv := newTestServer(false)
	defer srv.Close()
	if resp := postChat(t, srv.URL, `{"messages":[{"role":"user","content":"q"}]}`); resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503", resp.StatusCode)
	}
}

func TestChatDailyBudget(t *testing.T) {
	s := &Server{Catalog: fakeCatalog{}, Agent: fakeRunner{}, ChatPerDay: 1}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	body := `{"messages":[{"role":"user","content":"q"}]}`
	if resp := postChat(t, srv.URL, body); resp.StatusCode != http.StatusOK {
		t.Fatalf("first request: %d", resp.StatusCode)
	}
	if resp := postChat(t, srv.URL, body); resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("over budget: %d, want 429", resp.StatusCode)
	}
}
