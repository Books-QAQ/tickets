package db

import (
	"context"
	"fmt"
)

// CancelExpiredOrderTx 超时关单：把「订单 pending→canceled」与「座位 reserved→available」
// 放在同一事务，保证订单关闭和座位释放的原子性。
//
// 竞态安全：ClaimOrderCancel 与 ClaimOrderPayment 共用 status='pending' 条件，
// 关单与支付在数据库层互斥——谁先抢到状态变更谁生效，另一个 RowsAffected=0 跳过。
func (store *Store) CancelExpiredOrderTx(ctx context.Context, orderNo string) (Order, error) {
	var order Order

	err := store.execTx(ctx, func(q *Queries) error {
		// ① 读订单（拿到座位号）
		o, err := q.GetOrderByNo(ctx, orderNo)
		if err != nil {
			return err
		}
		order = o
		if order.Status != "pending" {
			return nil // 已支付或已关闭，跳过
		}

		// ② 条件更新订单 pending→canceled（幂等：已关闭则 RowsAffected=0）
		canceled, err := q.ClaimOrderCancel(ctx, orderNo)
		if err != nil {
			return err
		}
		if !canceled {
			return nil // 竞态：支付先到，跳过（不释放座位，避免误释放已售出的）
		}

		// ③ 条件更新座位 reserved→available（释放）
		if _, err := q.UpdateBusSeatStatusIf(ctx, order.SeatID, "reserved", "available"); err != nil {
			return err
		}

		order.Status = "canceled"
		return nil
	})

	if err != nil {
		return order, fmt.Errorf("failed to cancel expired order: %w", err)
	}
	return order, nil
}
