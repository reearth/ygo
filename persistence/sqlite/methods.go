package sqlite

import (
	"context"

	"github.com/reearth/ygo/persistence"
)

// Load is a temporary stub replaced in Task 2.
func (s *Store) Load(ctx context.Context, room string) (persistence.LoadResult, error) {
	return persistence.LoadResult{}, nil
}
