package db

import (
	"context"
	"time"
)

const getBusSaleOpenAt = `
SELECT sale_open_at
FROM buses
WHERE id = ?
`

func (q *Queries) GetBusSaleOpenAt(ctx context.Context, busID int32) (time.Time, error) {
	row := q.db.QueryRowContext(ctx, getBusSaleOpenAt, busID)
	var saleOpenAt time.Time
	err := row.Scan(&saleOpenAt)
	return saleOpenAt, err
}
