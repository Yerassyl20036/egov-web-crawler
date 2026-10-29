// Command crawler walks the data.egov.kz catalog and writes every dataset
// passport to Postgres (when DATABASE_URL is set) and/or a JSONL snapshot.
//
//	go run ./cmd/crawler -export data/datasets.jsonl.gz
//	DATABASE_URL=postgres://... go run ./cmd/crawler -passports
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"sync"
	"time"

	"github.com/Yerassyl20036/egov-web-crawler/internal/catalog"
	"github.com/Yerassyl20036/egov-web-crawler/internal/egov"
	"github.com/Yerassyl20036/egov-web-crawler/internal/store"
)

func main() {
	maxPages := flag.Int("pages", 2000, "maximum number of listing pages to fetch")
	pageSize := flag.Int("page-size", 10, "datasets per listing page (the portal caps it at 10)")
	delay := flag.Duration("delay", 300*time.Millisecond, "pause between listing pages (be polite)")
	passports := flag.Bool("passports", false, "also fetch each dataset's HTML passport page (adds the owner field; slow)")
	workers := flag.Int("workers", 4, "concurrent passport fetchers")
	export := flag.String("export", "", "write a JSONL snapshot to this path (.gz to compress)")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	var db *store.Store
	if url := os.Getenv("DATABASE_URL"); url != "" {
		var err error
		if db, err = store.Open(ctx, url); err != nil {
			log.Fatal(err)
		}
		defer db.Close()
		if err := db.Migrate(ctx); err != nil {
			log.Fatal(err)
		}
	}
	if db == nil && *export == "" {
		log.Fatal("nothing to do: set DATABASE_URL and/or -export")
	}

	client := egov.NewClient(os.Getenv("EGOV_API_KEY"))
	seen := map[string]bool{}
	var all []catalog.Dataset

	start := time.Now()
crawl:
	for page := 0; page < *maxPages; page++ {
		resp, err := listWithRetry(ctx, client, page, *pageSize)
		if err != nil {
			log.Printf("page %d: %v (stopping)", page, err)
			break
		}
		if len(resp.Datasets) == 0 {
			log.Printf("page %d is empty: reached the end of the catalog", page)
			break
		}
		crawledAt := time.Now().UTC()
		batch := make([]catalog.Dataset, 0, len(resp.Datasets))
		for _, l := range resp.Datasets {
			d := l.ToDataset(crawledAt)
			if d.APIURI == "" || seen[d.APIURI] {
				continue
			}
			seen[d.APIURI] = true
			batch = append(batch, d)
		}
		if db != nil {
			if err := db.Upsert(ctx, batch); err != nil {
				log.Fatalf("page %d: upsert: %v", page, err)
			}
		}
		all = append(all, batch...)
		if page%10 == 0 {
			log.Printf("page %d: %d datasets so far", page, len(all))
		}
		select {
		case <-ctx.Done():
			log.Print("interrupted")
			break crawl
		case <-time.After(*delay):
		}
	}

	if *passports {
		fetchPassports(ctx, client, db, all, *workers)
	}

	if *export != "" {
		if err := catalog.WriteSnapshot(*export, all); err != nil {
			log.Fatal(err)
		}
		log.Printf("wrote %d datasets to %s", len(all), *export)
	}
	log.Printf("done: %d datasets in %s", len(all), time.Since(start).Round(time.Second))
}

// listWithRetry retries a listing page with exponential backoff; the portal
// occasionally stalls for tens of seconds.
func listWithRetry(ctx context.Context, client *egov.Client, page, size int) (*egov.ListResponse, error) {
	var err error
	for attempt := 0; attempt < 4; attempt++ {
		var resp *egov.ListResponse
		if resp, err = client.ListDatasets(ctx, page, size); err == nil {
			return resp, nil
		}
		wait := time.Duration(1<<attempt) * 2 * time.Second
		log.Printf("page %d: %v (retrying in %s)", page, err, wait)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
	}
	return nil, err
}

// fetchPassports enriches datasets with the owner from their passport page,
// using a small worker pool so the portal is not hammered.
func fetchPassports(ctx context.Context, client *egov.Client, db *store.Store, all []catalog.Dataset, workers int) {
	jobs := make(chan int)
	var wg sync.WaitGroup
	var mu sync.Mutex
	done := 0
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				p, err := client.FetchPassport(ctx, all[i].APIURI)
				if err != nil {
					log.Printf("passport %s: %v", all[i].APIURI, err)
					continue
				}
				if p.Owner != "" {
					all[i].Owner = p.Owner
					if db != nil {
						if err := db.SetOwner(ctx, all[i].APIURI, p.Owner); err != nil {
							log.Printf("passport %s: save owner: %v", all[i].APIURI, err)
						}
					}
				}
				mu.Lock()
				if done++; done%200 == 0 {
					log.Printf("passports: %d/%d", done, len(all))
				}
				mu.Unlock()
				time.Sleep(200 * time.Millisecond)
			}
		}()
	}
	for i := range all {
		if ctx.Err() != nil {
			break
		}
		jobs <- i
	}
	close(jobs)
	wg.Wait()
}
