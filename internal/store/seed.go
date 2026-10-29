package store

import (
	"context"
	"fmt"

	"github.com/Yerassyl20036/egov-web-crawler/internal/catalog"
)

// SeedFile upserts every dataset of a snapshot or legacy CSV file and
// returns how many rows it read.
func (s *Store) SeedFile(ctx context.Context, path string) (int, error) {
	datasets, err := catalog.ReadFile(path)
	if err != nil {
		return 0, fmt.Errorf("store: read %s: %w", path, err)
	}
	for i := 0; i < len(datasets); i += 500 {
		end := min(i+500, len(datasets))
		if err := s.Upsert(ctx, datasets[i:end]); err != nil {
			return i, fmt.Errorf("store: upsert rows %d-%d: %w", i, end, err)
		}
	}
	return len(datasets), nil
}
