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

// PayOrderTx 主动支付出票：把"订单 pending→paid"与"座位出票"放在同一个事务里。
// 走 claimOrderPayment（带 expired_at > NOW() 条件），订单过期则拒绝支付。
func (store *Store) PayOrderTx(ctx context.Context, arg PayOrderTxParams) (PayOrderTxResult, error) {
	return store.payOrderTx(ctx, arg, false)
}

// SettleOrderTx 补出票：用于"已确认付款但订单可能已过期"的场景（异步通知/查单兜底）。
// 走 settleOrderPayment（不带过期条件），钱已扣则必须出票，避免钱悬空。
func (store *Store) SettleOrderTx(ctx context.Context, arg PayOrderTxParams) (PayOrderTxResult, error) {
	return store.payOrderTx(ctx, arg, true)
}

func (store *Store) payOrderTx(ctx context.Context, arg PayOrderTxParams, settle bool) (PayOrderTxResult, error) {
	var result PayOrderTxResult

	err := store.execTx(ctx, func(q *Queries) error {
		// ① 条件更新订单 pending→paid（幂等核心）
		var claimed bool
		var err error
		if settle {
			claimed, err = q.SettleOrderPayment(ctx, arg.OrderNo, arg.UserID, arg.Channel)
		} else {
			claimed, err = q.ClaimOrderPayment(ctx, arg.OrderNo, arg.UserID, arg.Channel)
		}
		if err != nil {
			return err
		}
		if !claimed {
			// 没抢到状态变更：可能是重复支付（已 paid），也可能已取消/退款/过期
			order, err := q.GetOrderByNo(ctx, arg.OrderNo)
			if err != nil {
				return err
			}
			if order.Status == "paid" {
				result.Status = "already_paid"
				return nil
			}
			if order.Status == "pending" && time.Now().After(order.ExpiredAt) {
				return fmt.Errorf("order has expired")
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
