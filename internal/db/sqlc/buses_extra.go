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

// updateBusSeatStatusIf 通用座位状态流转：fromStatus→toStatus，条件更新保证并发安全。
// 这是座位状态机的核心原语：下单 available→reserved、支付 reserved→purchased、
// 关单 reserved→available 都走它，靠行锁 + RowsAffected 判胜负。
const updateBusSeatStatusIf = `
UPDATE bus_seats
SET status = ?
WHERE id = ? AND status = ?
`

func (q *Queries) UpdateBusSeatStatusIf(ctx context.Context, id int32, fromStatus, toStatus string) (bool, error) {
	res, err := q.db.ExecContext(ctx, updateBusSeatStatusIf, toStatus, id, fromStatus)
	if err != nil {
		return false, err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected == 1, nil
}
