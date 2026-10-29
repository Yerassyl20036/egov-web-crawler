// Command eval scores search retrieval (always) and the full agent (only
// when ANTHROPIC_API_KEY is set and -agent is passed) on evals/cases.json.
//
//	DATABASE_URL=postgres://... go run ./cmd/eval
//	DATABASE_URL=postgres://... ANTHROPIC_API_KEY=... go run ./cmd/eval -agent
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/Yerassyl20036/egov-web-crawler/internal/agent"
	"github.com/Yerassyl20036/egov-web-crawler/internal/eval"
	"github.com/Yerassyl20036/egov-web-crawler/internal/store"
)

func main() {
	casesPath := flag.String("cases", "evals/cases.json", "eval cases")
	k := flag.Int("k", 10, "search depth")
	runAgent := flag.Bool("agent", false, "also run the end-to-end agent eval (needs ANTHROPIC_API_KEY; costs tokens)")
	only := flag.String("only", "", "comma-separated case ids to run")
	out := flag.String("out", "", "write full JSON results here")
	summary := flag.String("summary", "", "append the markdown report here (e.g. $GITHUB_STEP_SUMMARY)")
	minHit5 := flag.Float64("min-hit5", 0, "exit 1 if keyword-query Hit@5 falls below this")
	verbose := flag.Bool("v", false, "print per-case results")
	flag.Parse()

	ctx := context.Background()
	db, err := store.Open(ctx, mustEnv("DATABASE_URL"))
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	cases, err := eval.LoadCases(*casesPath)
	if err != nil {
		log.Fatal(err)
	}
	if *only != "" {
		keep := map[string]bool{}
		for _, id := range strings.Split(*only, ",") {
			keep[strings.TrimSpace(id)] = true
		}
		var filtered []eval.Case
		for _, c := range cases {
			if keep[c.ID] {
				filtered = append(filtered, c)
			}
		}
		cases = filtered
	}

	scores, err := eval.Retrieval(ctx, db, cases, *k)
	if err != nil {
		log.Fatal(err)
	}
	if *verbose {
		for _, s := range scores {
			fmt.Printf("%-14s %-8s rank=%-2d top3=%v\n", s.CaseID, s.Mode, s.Rank, head(s.Top, 3))
		}
	}
	retrieval := eval.SummarizeRetrieval(scores)
	report := fmt.Sprintf("### Retrieval (`search_datasets`, %d questions, k=%d)\n\n%s\n", len(cases), *k, eval.RetrievalTable(retrieval))

	results := map[string]any{"retrieval": scores, "retrieval_summary": retrieval}
	if *runAgent {
		if os.Getenv("ANTHROPIC_API_KEY") == "" && os.Getenv("ANTHROPIC_AUTH_TOKEN") == "" {
			log.Print("skipping agent eval: ANTHROPIC_API_KEY is not set")
		} else {
			cfg := agent.FromEnv()
			a := agent.New(cfg, &agent.Tools{Catalog: db})
			agentScores := eval.Agent(ctx, a, cases, func(s eval.AgentScore) {
				status := "MISS"
				if s.CitedExpected {
					status = "ok"
				}
				fmt.Fprintf(os.Stderr, "%-14s %-4s searches=%q cited=%v %s\n", s.CaseID, status, s.Searches, s.Citations, s.Error)
			})
			agentSummary := eval.SummarizeAgent(agentScores)
			report += fmt.Sprintf("### Agent (`%s`, effort `%s`, %d questions)\n\n%s\n", cfg.Model, cfg.Effort, len(cases), eval.AgentTable(agentSummary))
			results["agent"], results["agent_summary"] = agentScores, agentSummary
		}
	}

	fmt.Print(report)
	if *out != "" {
		data, _ := json.MarshalIndent(results, "", "  ")
		if err := os.WriteFile(*out, data, 0o644); err != nil {
			log.Fatal(err)
		}
	}
	if *summary != "" {
		f, err := os.OpenFile(*summary, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			log.Fatal(err)
		}
		f.WriteString(report)
		f.Close()
	}
	if len(retrieval) > 0 && retrieval[0].Hit5 < *minHit5 {
		log.Fatalf("keyword-query Hit@5 %.2f is below the threshold %.2f", retrieval[0].Hit5, *minHit5)
	}
}

func head(s []string, n int) []string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		log.Fatalf("%s is required", key)
	}
	return v
}
