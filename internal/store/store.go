// Package store persists the dataset catalog in Postgres and serves the
// full-text search behind the agent's search_datasets tool.
package store

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Yerassyl20036/egov-web-crawler/internal/catalog"
)

//go:embed migrations/*.sql
var migrations embed.FS

// ErrNotFound is returned when a dataset does not exist.
var ErrNotFound = errors.New("store: dataset not found")

// Store is a Postgres-backed dataset catalog.
type Store struct {
	pool *pgxpool.Pool
}

// Open connects to Postgres. Call Migrate before first use.
func Open(ctx context.Context, databaseURL string) (*Store, error) {
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, fmt.Errorf("store: connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("store: ping: %w", err)
	}
	return &Store{pool: pool}, nil
}

// Close releases the connection pool.
func (s *Store) Close() { s.pool.Close() }

// Ping checks the database connection (used by /api/health).
func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

// Migrate applies embedded SQL migrations once each, in file-name order.
// An advisory lock keeps the api and seed containers from racing.
func (s *Store) Migrate(ctx context.Context) error {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()

	const lockID = 7_316_002 // arbitrary, unique to this app
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", lockID); err != nil {
		return fmt.Errorf("store: migration lock: %w", err)
	}
	defer conn.Exec(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", lockID)

	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version text PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return fmt.Errorf("store: create schema_migrations: %w", err)
	}

	files, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(files)
	for _, file := range files {
		version := strings.TrimSuffix(strings.TrimPrefix(file, "migrations/"), ".sql")
		var applied bool
		if err := conn.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)", version).Scan(&applied); err != nil {
			return err
		}
		if applied {
			continue
		}
		sql, err := migrations.ReadFile(file)
		if err != nil {
			return err
		}
		tx, err := conn.Begin(ctx)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, string(sql)); err != nil {
			tx.Rollback(ctx)
			return fmt.Errorf("store: migration %s: %w", version, err)
		}
		if _, err := tx.Exec(ctx, "INSERT INTO schema_migrations (version) VALUES ($1)", version); err != nil {
			tx.Rollback(ctx)
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
	}
	return nil
}

// Count returns the number of datasets in the catalog.
func (s *Store) Count(ctx context.Context) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, "SELECT count(*) FROM datasets").Scan(&n)
	return n, err
}

const upsertSQL = `
INSERT INTO datasets (
	api_uri, title_ru, title_kk, title_en, description_ru, description_kk, description_en,
	keywords, keywords_text, category_ru, category_kk, category_en, agency_ru, agency_kk, agency_en,
	owner, actualization, is_actual, status, views, downloads, created_at, modified_at, crawled_at
) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24)
ON CONFLICT (api_uri) DO UPDATE SET
	title_ru = EXCLUDED.title_ru, title_kk = EXCLUDED.title_kk, title_en = EXCLUDED.title_en,
	description_ru = EXCLUDED.description_ru, description_kk = EXCLUDED.description_kk,
	description_en = EXCLUDED.description_en,
	keywords = EXCLUDED.keywords, keywords_text = EXCLUDED.keywords_text,
	category_ru = EXCLUDED.category_ru, category_kk = EXCLUDED.category_kk, category_en = EXCLUDED.category_en,
	agency_ru = EXCLUDED.agency_ru, agency_kk = EXCLUDED.agency_kk, agency_en = EXCLUDED.agency_en,
	-- the listing API has no owner; keep one found earlier on the passport page
	owner = COALESCE(NULLIF(EXCLUDED.owner, ''), datasets.owner),
	actualization = EXCLUDED.actualization, is_actual = EXCLUDED.is_actual, status = EXCLUDED.status,
	views = EXCLUDED.views, downloads = EXCLUDED.downloads,
	created_at = EXCLUDED.created_at, modified_at = EXCLUDED.modified_at, crawled_at = EXCLUDED.crawled_at`

// Upsert inserts or updates datasets in one batch round trip.
func (s *Store) Upsert(ctx context.Context, datasets []catalog.Dataset) error {
	batch := &pgx.Batch{}
	for _, d := range datasets {
		if d.APIURI == "" {
			continue
		}
		keywords := d.Keywords
		if keywords == nil {
			keywords = []string{}
		}
		batch.Queue(upsertSQL,
			d.APIURI, d.TitleRu, d.TitleKk, d.TitleEn, d.DescriptionRu, d.DescriptionKk, d.DescriptionEn,
			keywords, strings.Join(keywords, " "), d.CategoryRu, d.CategoryKk, d.CategoryEn,
			d.AgencyRu, d.AgencyKk, d.AgencyEn, d.Owner, d.Actualization, d.IsActual, d.Status,
			d.Views, d.Downloads, d.CreatedAt, d.ModifiedAt, d.CrawledAt)
	}
	return s.pool.SendBatch(ctx, batch).Close()
}

// SetOwner records the owner parsed from a dataset's passport page.
func (s *Store) SetOwner(ctx context.Context, apiURI, owner string) error {
	_, err := s.pool.Exec(ctx, "UPDATE datasets SET owner = $2 WHERE api_uri = $1", apiURI, owner)
	return err
}

const selectColumns = `api_uri, title_ru, title_kk, title_en, description_ru, description_kk, description_en,
	keywords, category_ru, category_kk, category_en, agency_ru, agency_kk, agency_en, owner,
	actualization, is_actual, status, views, downloads, created_at, modified_at, crawled_at`

func scanDataset(row pgx.Row, extra ...any) (catalog.Dataset, error) {
	var d catalog.Dataset
	dest := []any{&d.APIURI, &d.TitleRu, &d.TitleKk, &d.TitleEn, &d.DescriptionRu, &d.DescriptionKk,
		&d.DescriptionEn, &d.Keywords, &d.CategoryRu, &d.CategoryKk, &d.CategoryEn, &d.AgencyRu,
		&d.AgencyKk, &d.AgencyEn, &d.Owner, &d.Actualization, &d.IsActual, &d.Status, &d.Views,
		&d.Downloads, &d.CreatedAt, &d.ModifiedAt, &d.CrawledAt}
	err := row.Scan(append(dest, extra...)...)
	return d, err
}

// Get returns one dataset by api_uri.
func (s *Store) Get(ctx context.Context, apiURI string) (catalog.Dataset, error) {
	row := s.pool.QueryRow(ctx, "SELECT "+selectColumns+" FROM datasets WHERE api_uri = $1", apiURI)
	d, err := scanDataset(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return d, ErrNotFound
	}
	return d, err
}

// GetMany returns the datasets that exist among apiURIs, keyed by api_uri.
func (s *Store) GetMany(ctx context.Context, apiURIs []string) (map[string]catalog.Dataset, error) {
	out := make(map[string]catalog.Dataset, len(apiURIs))
	if len(apiURIs) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, "SELECT "+selectColumns+" FROM datasets WHERE api_uri = ANY($1)", apiURIs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		d, err := scanDataset(rows)
		if err != nil {
			return nil, err
		}
		out[d.APIURI] = d
	}
	return out, rows.Err()
}

// CategoryCount is one row of the category facet.
type CategoryCount struct {
	CategoryRu string `json:"category_ru"`
	CategoryEn string `json:"category_en,omitempty"`
	Count      int    `json:"count"`
}

// Categories lists the portal's categories with dataset counts.
func (s *Store) Categories(ctx context.Context) ([]CategoryCount, error) {
	rows, err := s.pool.Query(ctx, `SELECT category_ru, max(category_en), count(*)
		FROM datasets WHERE category_ru <> '' GROUP BY category_ru ORDER BY count(*) DESC`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (CategoryCount, error) {
		var c CategoryCount
		err := r.Scan(&c.CategoryRu, &c.CategoryEn, &c.Count)
		return c, err
	})
}
