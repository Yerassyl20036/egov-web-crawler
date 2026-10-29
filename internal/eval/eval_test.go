package eval

import (
	"context"
	"math"
	"testing"

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
