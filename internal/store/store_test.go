package store

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/Yerassyl20036/egov-web-crawler/internal/catalog"
)

func TestBuildTSQuery(t *testing.T) {
	cases := map[string]string{
		"Список больниц в Алматы":      "список:* | больниц:* | алматы:*",
		"hospitals in Almaty, 2024!":   "hospitals:* | almaty:* | 2024:*",
		"  ':*&|!() ":                  "",
		"Алматы алматы":                "алматы:*",
		"Шымкент қаласы бойынша тізім": "шымкент:* | қаласы:* | тізім:*",
	}
	for in, want := range cases {
		if got := BuildTSQuery(in); got != want {
			t.Errorf("BuildTSQuery(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestSearchIntegration runs against a real Postgres when TEST_DATABASE_URL
// is set (CI provides one). It uses a throwaway schema.
func TestSearchIntegration(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	s, err := Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.pool.Exec(ctx, "DROP TABLE IF EXISTS datasets, schema_migrations"); err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("migrations must be idempotent: %v", err)
	}

	now := time.Now()
	err = s.Upsert(ctx, []catalog.Dataset{
		{APIURI: "hospitals_almaty", TitleRu: "Перечень больниц города Алматы", CategoryRu: "Здравоохранение",
			CategoryEn: "Healthcare", AgencyRu: "Акимат города Алматы", Keywords: []string{"больницы"}, Views: 10, CrawledAt: now},
		{APIURI: "schools_astana", TitleRu: "Список школ города Астаны", TitleKk: "Астана қаласының мектептері",
			CategoryRu: "Образование и наука", AgencyRu: "Акимат города Астаны", CrawledAt: now},
	})
	if err != nil {
		t.Fatal(err)
	}

	hits, err := s.Search(ctx, SearchParams{Query: "больница Алматы", Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 || hits[0].APIURI != "hospitals_almaty" {
		t.Fatalf("stemmed search failed: %+v", hits)
	}
	hits, _ = s.Search(ctx, SearchParams{Query: "мектептері", Limit: 5})
	if len(hits) == 0 || hits[0].APIURI != "schools_astana" {
		t.Fatalf("kazakh search failed: %+v", hits)
	}
	hits, _ = s.Search(ctx, SearchParams{Query: "больнци", Limit: 5}) // typo
	if len(hits) == 0 || hits[0].APIURI != "hospitals_almaty" {
		t.Fatalf("trigram fallback failed: %+v", hits)
	}
	hits, _ = s.Search(ctx, SearchParams{Category: "Healthcare"})
	if len(hits) != 1 {
		t.Fatalf("category filter: got %d hits", len(hits))
	}
	if _, err := s.Get(ctx, "nope"); err != ErrNotFound {
		t.Fatalf("Get missing = %v", err)
	}
}
