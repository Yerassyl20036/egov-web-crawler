// Command seed loads a dataset snapshot (or the legacy CSV export) into
// Postgres so the app works without crawling the portal first.
//
//	DATABASE_URL=postgres://... go run ./cmd/seed -file data/datasets.jsonl.gz
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"time"

	"github.com/Yerassyl20036/egov-web-crawler/internal/store"
)

func main() {
	file := flag.String("file", "data/datasets.jsonl.gz", "snapshot (.jsonl[.gz]) or legacy export (.csv)")
	ifEmpty := flag.Bool("if-empty", false, "skip seeding when the catalog already has data")
	flag.Parse()

	url := os.Getenv("DATABASE_URL")
	if url == "" {
		log.Fatal("DATABASE_URL is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	db, err := store.Open(ctx, url)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	if err := db.Migrate(ctx); err != nil {
		log.Fatal(err)
	}

	if *ifEmpty {
		if n, err := db.Count(ctx); err != nil {
			log.Fatal(err)
		} else if n > 0 {
			log.Printf("catalog already has %d datasets; skipping seed", n)
			return
		}
	}

	start := time.Now()
	read, err := db.SeedFile(ctx, *file)
	if err != nil {
		log.Fatal(err)
	}
	n, _ := db.Count(ctx)
	log.Printf("seeded %d datasets from %s in %s (catalog now has %d)", read, *file, time.Since(start).Round(time.Millisecond), n)
}
