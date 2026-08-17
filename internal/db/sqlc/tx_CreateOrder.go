package db

import (
	"context"
	"fmt"
	"time"
)

type CreateOrderTxParams struct {
	OrderNo   string
	UserID    int32
	BusID     int32
	SeatID    int32
	Amount    int32
	ExpiredAt time.Time
}

// CreateOrderTx 下单：把「座位 available→reserved」与「创建 pending 订单」放在同一事务，
// 让 MySQL 座位状态成为占座的唯一真相源（Redis 锁仅作快速分流，不承担正确性）。
//
// 并发安全：UpdateBusSeatStatusIf 用 WHERE status='available' 条件更新，靠行锁保证
// 同一座位只有一个下单能成功，其余 RowsAffected=0 回滚。
func (store *Store) CreateOrderTx(ctx context.Context, arg CreateOrderTxParams) (Order, error) {
	var order Order

	err := store.execTx(ctx, func(q *Queries) error {
		// ① 条件更新座位 available→reserved（占座真相源落库）
		claimed, err := q.UpdateBusSeatStatusIf(ctx, arg.SeatID, "available", "reserved")
		if err != nil {
			return err
		}
		if !claimed {
			return fmt.Errorf("seat is no longer available")
		}

		// ② 创建 pending 订单
		if _, err := q.CreateOrder(ctx, CreateOrderParams{
			OrderNo:   arg.OrderNo,
			UserID:    arg.UserID,
			BusID:     arg.BusID,
			SeatID:    arg.SeatID,
			Amount:    arg.Amount,
			ExpiredAt: arg.ExpiredAt,
		}); err != nil {
			return err
		}

		// ③ 读回订单
		order, err = q.GetOrderByNo(ctx, arg.OrderNo)
		if err != nil {
			return err
		}
		return nil
	})

	if err != nil {
		return order, fmt.Errorf("failed to create order: %w", err)
	}
	return order, nil
}
