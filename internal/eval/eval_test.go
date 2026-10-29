package eval

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/Yerassyl20036/egov-web-crawler/internal/agent"
	"github.com/Yerassyl20036/egov-web-crawler/internal/catalog"
	"github.com/Yerassyl20036/egov-web-crawler/internal/store"
)

type scriptedSearch map[string][]string

func (s scriptedSearch) Search(_ context.Context, p store.SearchParams) ([]store.SearchHit, error) {
	var hits []store.SearchHit
	for _, uri := range s[p.Query] {
		hits = append(hits, store.SearchHit{Dataset: catalog.Dataset{APIURI: uri}})
	}
	return hits, nil
}

func TestRetrievalMetrics(t *testing.T) {
	cases := []Case{
		{ID: "a", Lang: "en", Question: "hospitals", Query: "больницы", Expected: []string{"h1", "h2"}},
		{ID: "b", Lang: "ru", Question: "школы", Query: "школы", Expected: []string{"s1"}},
	}
	search := scriptedSearch{
		"больницы":  {"x", "h2", "y", "h1"}, // first hit at rank 2, both found
		"hospitals": {},                     // raw English query finds nothing
		"школы":     {"s1"},
	}
	scores, err := Retrieval(context.Background(), search, cases, 10)
	if err != nil {
		t.Fatal(err)
	}
	sum := SummarizeRetrieval(scores)
	byGroup := map[string]RetrievalSummary{}
	for _, s := range sum {
		byGroup[s.Group] = s
	}
	kw := byGroup["keywords / all"]
	if kw.N != 2 || kw.Hit1 != 0.5 || kw.Hit5 != 1 || math.Abs(kw.MRR-0.75) > 1e-9 || kw.Recall5 != 1 {
		t.Fatalf("keywords summary = %+v", kw)
	}
	raw := byGroup["raw / lang: en"]
	if raw.Hit5 != 0 || raw.Recall10 != 0 {
		t.Fatalf("raw/en summary = %+v", raw)
	}
	if sum[0].Group != "keywords / all" {
		t.Fatalf("summary order = %v", sum[0].Group)
	}
}

type scriptedRunner struct{ results map[string]error }

func (r scriptedRunner) Run(_ context.Context, h []agent.Message, _ func(agent.Event)) (*agent.Result, error) {
	if err := r.results[h[0].Content]; err != nil {
		return nil, err
	}
	return &agent.Result{Citations: []agent.Citation{{DatasetRef: agent.DatasetRef{APIURI: "h1"}, Verified: true}}}, nil
}

func TestAgentSkipsRateLimitedQuestions(t *testing.T) {
	cases := []Case{
		{ID: "a", Lang: "en", Question: "qa", Expected: []string{"h1"}},
		{ID: "b", Lang: "en", Question: "qb", Expected: []string{"h1"}},
		{ID: "c", Lang: "en", Question: "qc", Expected: []string{"h1"}},
	}
	runner := scriptedRunner{results: map[string]error{
		"qb": errors.New("agent: openrouter returned 429: rate limit exceeded: free-models-per-day"),
		"qc": errors.New("agent: something broke"),
	}}
	scores := Agent(context.Background(), runner, cases, func(AgentScore) bool { return true })
	sum := SummarizeAgent(scores)[0]
	if sum.N != 2 || sum.Skipped != 1 || sum.Errors != 1 || sum.CitedExpected != 0.5 {
		t.Fatalf("summary = %+v", sum)
	}
	stopped := Agent(context.Background(), runner, cases, func(AgentScore) bool { return false })
	if len(stopped) != 1 {
		t.Fatalf("progress returning false should stop the run, got %d scores", len(stopped))
	}
}
