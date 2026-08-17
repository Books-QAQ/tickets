package db

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

type PayOrderTxParams struct {
	OrderNo string
	UserID  int32
	BusID   int32
	SeatID  int32
	Channel string
}

type PayOrderTxResult struct {
	TicketID int32  `json:"ticket_id"`
	Status   string `json:"status"` // paid / already_paid
}

// PayOrderTx 支付出票：把"订单 pending→paid"与"座位出票"放在同一个事务里，
// 保证"订单已支付"和"座位已售出"的原子性与幂等。
//
// 幂等保证：ClaimOrderPayment 用 WHERE status='pending' 条件更新，重复支付回调
// 时 RowsAffected=0，走 already_paid 分支，不会重复出票。
func (store *Store) PayOrderTx(ctx context.Context, arg PayOrderTxParams) (PayOrderTxResult, error) {
	var result PayOrderTxResult

	err := store.execTx(ctx, func(q *Queries) error {
		// ① 条件更新订单 pending→paid（幂等核心）
		claimed, err := q.ClaimOrderPayment(ctx, arg.OrderNo, arg.UserID, arg.Channel)
		if err != nil {
			return err
		}
		if !claimed {
			// 没抢到状态变更：可能是重复支付（已 paid），也可能是已取消/退款
			order, err := q.GetOrderByNo(ctx, arg.OrderNo)
			if err != nil {
				return err
			}
			if order.Status == "paid" {
				result.Status = "already_paid"
				return nil
			}
			return fmt.Errorf("order is not payable, current status: %s", order.Status)
		}

		// ② 出票：条件更新座位 reserved→purchased（下单已 reserved，这里推进到售出）
		seatClaimed, err := q.UpdateBusSeatStatusIf(ctx, arg.SeatID, "reserved", "purchased")
		if err != nil {
			return err
		}
		if !seatClaimed {
			return fmt.Errorf("seat is no longer available")
		}

		// ③ 写预订记录 + 车票
		reservation, err := q.CreateSeatReservation(ctx, CreateSeatReservationParams{
			BusID:       arg.BusID,
			BusSeatID:   arg.SeatID,
			UserID:      arg.UserID,
			Status:      "purchased",
			PurchasedAt: sql.NullTime{Time: time.Now(), Valid: true},
		})
		if err != nil {
			return err
		}

		ticket, err := q.PurchaseTicket(ctx, PurchaseTicketParams{
			UserID:            arg.UserID,
			BusID:             arg.BusID,
			SeatReservationID: reservation.ID,
		})
		if err != nil {
			return err
		}

		result.TicketID = ticket.ID
		result.Status = "paid"
		return nil
	})

	if err != nil {
		return result, fmt.Errorf("failed to pay order: %w", err)
	}
	return result, nil
}
