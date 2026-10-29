// Package eval measures the search tool and the agent on a fixed question
// set with known relevant datasets.
package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/Yerassyl20036/egov-web-crawler/internal/agent"
	"github.com/Yerassyl20036/egov-web-crawler/internal/store"
)

// Case is one evaluation question.
type Case struct {
	ID       string   `json:"id"`
	Lang     string   `json:"lang"`     // ru | en | kk
	Split    string   `json:"split"`    // dev (used while tuning search) | holdout (never tuned on)
	Question string   `json:"question"` // what a user would type
	Query    string   `json:"query"`    // the Russian keyword query the agent is told to produce
	Expected []string `json:"expected"` // api_uris that correctly answer the question
}

// LoadCases reads a JSON array of cases.
func LoadCases(path string) ([]Case, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cases []Case
	if err := json.Unmarshal(data, &cases); err != nil {
		return nil, fmt.Errorf("eval: parse %s: %w", path, err)
	}
	for _, c := range cases {
		if c.ID == "" || c.Question == "" || c.Query == "" || len(c.Expected) == 0 {
			return nil, fmt.Errorf("eval: case %q is incomplete", c.ID)
		}
	}
	return cases, nil
}

// Searcher is the search tool's backend.
type Searcher interface {
	Search(ctx context.Context, p store.SearchParams) ([]store.SearchHit, error)
}

// RetrievalScore holds ranking metrics for one case and one query mode.
type RetrievalScore struct {
	CaseID   string   `json:"case_id"`
	Lang     string   `json:"lang"`
	Split    string   `json:"split"`
	Mode     string   `json:"mode"` // "keywords" (Russian query) or "raw" (the user's question as-is)
	Rank     int      `json:"rank"` // 1-based rank of the first expected dataset, 0 = not in top k
	Recall5  float64  `json:"recall_at_5"`
	Recall10 float64  `json:"recall_at_10"`
	Top      []string `json:"top"`
}

// Retrieval runs every case through search twice: once with the curated
// Russian keyword query (what the agent should send) and once with the raw
// question (what a naive integration would send).
func Retrieval(ctx context.Context, s Searcher, cases []Case, k int) ([]RetrievalScore, error) {
	var out []RetrievalScore
	for _, c := range cases {
		for _, mode := range []string{"keywords", "raw"} {
			q := c.Query
			if mode == "raw" {
				q = c.Question
			}
			hits, err := s.Search(ctx, store.SearchParams{Query: q, Limit: k})
			if err != nil && err != store.ErrEmptyQuery {
				return nil, fmt.Errorf("eval: case %s: %w", c.ID, err)
			}
			top := make([]string, len(hits))
			for i, h := range hits {
				top[i] = h.APIURI
			}
			out = append(out, RetrievalScore{
				CaseID: c.ID, Lang: c.Lang, Split: c.Split, Mode: mode, Top: top,
				Rank:     firstRank(top, c.Expected),
				Recall5:  recallAt(top, c.Expected, 5),
				Recall10: recallAt(top, c.Expected, 10),
			})
		}
	}
	return out, nil
}

// RetrievalSummary aggregates RetrievalScores for one group.
type RetrievalSummary struct {
	Group    string  `json:"group"`
	N        int     `json:"n"`
	Hit1     float64 `json:"hit_at_1"`
	Hit5     float64 `json:"hit_at_5"`
	Recall5  float64 `json:"recall_at_5"`
	Recall10 float64 `json:"recall_at_10"`
	MRR      float64 `json:"mrr_at_10"`
}

// SummarizeRetrieval groups scores by mode, then by mode+split and
// mode+language.
func SummarizeRetrieval(scores []RetrievalScore) []RetrievalSummary {
	groups := map[string][]RetrievalScore{}
	var order []string
	add := func(key string, s RetrievalScore) {
		if _, ok := groups[key]; !ok {
			order = append(order, key)
		}
		groups[key] = append(groups[key], s)
	}
	for _, s := range scores {
		add(s.Mode+" / all", s)
		if s.Split != "" {
			add(s.Mode+" / split: "+s.Split, s)
		}
		add(s.Mode+" / lang: "+s.Lang, s)
	}
	rank := func(key string) int {
		r := 0
		if !strings.HasPrefix(key, "keywords") {
			r += 10
		}
		switch {
		case strings.HasSuffix(key, "/ all"):
		case strings.Contains(key, "split:"):
			r++
		default:
			r += 2
		}
		return r
	}
	sort.SliceStable(order, func(i, j int) bool {
		if ri, rj := rank(order[i]), rank(order[j]); ri != rj {
			return ri < rj
		}
		return order[i] < order[j]
	})
	out := make([]RetrievalSummary, 0, len(order))
	for _, key := range order {
		g := groups[key]
		sum := RetrievalSummary{Group: key, N: len(g)}
		for _, s := range g {
			if s.Rank == 1 {
				sum.Hit1++
			}
			if s.Rank >= 1 && s.Rank <= 5 {
				sum.Hit5++
			}
			if s.Rank >= 1 {
				sum.MRR += 1 / float64(s.Rank)
			}
			sum.Recall5 += s.Recall5
			sum.Recall10 += s.Recall10
		}
		n := float64(len(g))
		sum.Hit1, sum.Hit5, sum.MRR = sum.Hit1/n, sum.Hit5/n, sum.MRR/n
		sum.Recall5, sum.Recall10 = sum.Recall5/n, sum.Recall10/n
		out = append(out, sum)
	}
	return out
}

// AgentScore is the end-to-end outcome of one question.
type AgentScore struct {
	CaseID          string   `json:"case_id"`
	Lang            string   `json:"lang"`
	CitedExpected   bool     `json:"cited_expected"`     // the answer links at least one expected dataset
	RetrievedExpect bool     `json:"retrieved_expected"` // some tool call surfaced an expected dataset
	Citations       []string `json:"citations"`
	Unverified      int      `json:"unverified_citations"` // links to api_uris not in the catalog
	ToolCalls       int      `json:"tool_calls"`
	Searches        []string `json:"searches"`
	InputTokens     int64    `json:"input_tokens"`
	OutputTokens    int64    `json:"output_tokens"`
	DurationMS      int64    `json:"duration_ms"`
	Error           string   `json:"error,omitempty"`
	Skipped         bool     `json:"skipped,omitempty"` // provider quota/rate limit: not scored
}

// Runner is the agent under test.
type Runner interface {
	Run(ctx context.Context, history []agent.Message, emit func(agent.Event)) (*agent.Result, error)
}

// Agent asks the agent every question and checks its citations. Questions
// that fail on a provider rate limit or quota are marked Skipped and left
// out of the scores. progress is called after each question; returning
// false stops the run.
func Agent(ctx context.Context, r Runner, cases []Case, progress func(AgentScore) bool) []AgentScore {
	var out []AgentScore
	for _, c := range cases {
		qctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
		res, err := r.Run(qctx, []agent.Message{{Role: "user", Content: c.Question}}, func(agent.Event) {})
		cancel()
		s := AgentScore{CaseID: c.ID, Lang: c.Lang}
		if err != nil {
			s.Error = err.Error()
			s.Skipped = isRateLimit(s.Error)
		}
		if res != nil && !s.Skipped {
			for _, cit := range res.Citations {
				s.Citations = append(s.Citations, cit.APIURI)
				if !cit.Verified {
					s.Unverified++
				}
				if slices.Contains(c.Expected, cit.APIURI) {
					s.CitedExpected = true
				}
			}
			for _, tc := range res.ToolCalls {
				if tc.Name == "search_datasets" {
					var in struct{ Query string }
					json.Unmarshal(tc.Input, &in)
					s.Searches = append(s.Searches, in.Query)
				}
				for _, d := range tc.Datasets {
					if slices.Contains(c.Expected, d.APIURI) {
						s.RetrievedExpect = true
					}
				}
			}
			s.ToolCalls = len(res.ToolCalls)
			s.InputTokens, s.OutputTokens = res.Usage.InputTokens, res.Usage.OutputTokens
			s.DurationMS = res.DurationMS
		}
		out = append(out, s)
		if progress != nil && !progress(s) {
			break
		}
	}
	return out
}

func isRateLimit(msg string) bool {
	msg = strings.ToLower(msg)
	return strings.Contains(msg, "returned 429") || strings.Contains(msg, "rate limit") ||
		strings.Contains(msg, "rate-limit") || strings.Contains(msg, "insufficient balance") ||
		strings.Contains(msg, "credit balance is too low")
}

// AgentSummary aggregates AgentScores for one group.
type AgentSummary struct {
	Group         string  `json:"group"`
	N             int     `json:"n"`
	CitedExpected float64 `json:"cited_expected_rate"`
	Retrieved     float64 `json:"retrieved_expected_rate"`
	Unverified    int     `json:"unverified_citations"`
	Errors        int     `json:"errors"`
	Skipped       int     `json:"skipped_rate_limited"`
	AvgToolCalls  float64 `json:"avg_tool_calls"`
	AvgSeconds    float64 `json:"avg_seconds"`
	AvgInputTok   float64 `json:"avg_input_tokens"`
	AvgOutputTok  float64 `json:"avg_output_tokens"`
}

// SummarizeAgent groups agent scores overall and by language.
func SummarizeAgent(scores []AgentScore) []AgentSummary {
	langs := []string{"all"}
	for _, s := range scores {
		if !slices.Contains(langs, s.Lang) {
			langs = append(langs, s.Lang)
		}
	}
	sort.Strings(langs[1:])
	var out []AgentSummary
	for _, lang := range langs {
		sum := AgentSummary{Group: lang}
		for _, s := range scores {
			if lang != "all" && s.Lang != lang {
				continue
			}
			if s.Skipped {
				sum.Skipped++
				continue
			}
			sum.N++
			if s.CitedExpected {
				sum.CitedExpected++
			}
			if s.RetrievedExpect {
				sum.Retrieved++
			}
			if s.Error != "" {
				sum.Errors++
			}
			sum.Unverified += s.Unverified
			sum.AvgToolCalls += float64(s.ToolCalls)
			sum.AvgSeconds += float64(s.DurationMS) / 1000
			sum.AvgInputTok += float64(s.InputTokens)
			sum.AvgOutputTok += float64(s.OutputTokens)
		}
		if sum.N == 0 {
			if sum.Skipped > 0 {
				out = append(out, sum)
			}
			continue
		}
		n := float64(sum.N)
		sum.CitedExpected /= n
		sum.Retrieved /= n
		sum.AvgToolCalls /= n
		sum.AvgSeconds /= n
		sum.AvgInputTok /= n
		sum.AvgOutputTok /= n
		out = append(out, sum)
	}
	return out
}

// RetrievalTable renders a markdown table.
func RetrievalTable(rows []RetrievalSummary) string {
	var b strings.Builder
	b.WriteString("| Query mode / language | N | Hit@1 | Hit@5 | Recall@5 | Recall@10 | MRR@10 |\n")
	b.WriteString("|---|---:|---:|---:|---:|---:|---:|\n")
	for _, r := range rows {
		fmt.Fprintf(&b, "| %s | %d | %.2f | %.2f | %.2f | %.2f | %.2f |\n", r.Group, r.N, r.Hit1, r.Hit5, r.Recall5, r.Recall10, r.MRR)
	}
	return b.String()
}

// AgentTable renders a markdown table.
func AgentTable(rows []AgentSummary) string {
	var b strings.Builder
	b.WriteString("| Language | N | Cited an expected dataset | Expected dataset retrieved | Unverified links | Errors | Avg tool calls | Avg latency (s) | Avg input tok | Avg output tok |\n")
	b.WriteString("|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|\n")
	skipped := 0
	for _, r := range rows {
		if r.Group == "all" {
			skipped = r.Skipped
		}
		fmt.Fprintf(&b, "| %s | %d | %.2f | %.2f | %d | %d | %.1f | %.1f | %.0f | %.0f |\n", r.Group, r.N, r.CitedExpected,
			r.Retrieved, r.Unverified, r.Errors, r.AvgToolCalls, r.AvgSeconds, r.AvgInputTok, r.AvgOutputTok)
	}
	if skipped > 0 {
		fmt.Fprintf(&b, "\n_%d question(s) not scored: the provider's rate limit or quota stopped them._\n", skipped)
	}
	return b.String()
}

func firstRank(top, expected []string) int {
	for i, uri := range top {
		if slices.Contains(expected, uri) {
			return i + 1
		}
	}
	return 0
}

func recallAt(top, expected []string, k int) float64 {
	if len(top) > k {
		top = top[:k]
	}
	found := 0
	for _, e := range expected {
		if slices.Contains(top, e) {
			found++
		}
	}
	return float64(found) / float64(len(expected))
}
