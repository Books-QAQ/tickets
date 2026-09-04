package db

import (
	"context"
	"database/sql"
	"time"
)

// Order 对应 orders 表，是支付功能的核心实体。
type Order struct {
	ID         int64          `json:"id"`
	OrderNo    string         `json:"order_no"`
	UserID     int32          `json:"user_id"`
	BusID      int32          `json:"bus_id"`
	SeatID     int32          `json:"seat_id"`
	Amount     int32          `json:"amount"`
	Status     string         `json:"status"`
	PayChannel sql.NullString `json:"pay_channel"`
	PaidAt     sql.NullTime   `json:"paid_at"`
	ExpiredAt  time.Time      `json:"expired_at"`
	CreatedAt  time.Time      `json:"created_at"`
}

type CreateOrderParams struct {
	OrderNo   string
	UserID    int32
	BusID     int32
	SeatID    int32
	Amount    int32
	ExpiredAt time.Time
}

const createOrder = `
INSERT INTO orders (order_no, user_id, bus_id, seat_id, amount, expired_at)
VALUES (?, ?, ?, ?, ?, ?)
`

func (q *Queries) CreateOrder(ctx context.Context, arg CreateOrderParams) (int64, error) {
	res, err := q.db.ExecContext(ctx, createOrder, arg.OrderNo, arg.UserID, arg.BusID, arg.SeatID, arg.Amount, arg.ExpiredAt)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

const getOrderByNo = `
SELECT id, order_no, user_id, bus_id, seat_id, amount, status, pay_channel, paid_at, expired_at, created_at
FROM orders
WHERE order_no = ?
`

func (q *Queries) GetOrderByNo(ctx context.Context, orderNo string) (Order, error) {
	row := q.db.QueryRowContext(ctx, getOrderByNo, orderNo)
	var o Order
	err := row.Scan(
		&o.ID,
		&o.OrderNo,
		&o.UserID,
		&o.BusID,
		&o.SeatID,
		&o.Amount,
		&o.Status,
		&o.PayChannel,
		&o.PaidAt,
		&o.ExpiredAt,
		&o.CreatedAt,
	)
	return o, err
}

// claimOrderPayment 条件更新订单状态 pending→paid，返回是否抢到状态变更。
// 这是支付幂等的核心：重复回调时 RowsAffected=0，不会重复出票。
// expired_at > NOW() 是防"订单过期仍可支付"的最终防线：一旦过期，无论
// mock 直付还是支付宝回调/查单，条件都不成立，支付必然失败。
const claimOrderPayment = `
UPDATE orders
SET status = 'paid', paid_at = NOW(), pay_channel = ?
WHERE order_no = ? AND user_id = ? AND status = 'pending' AND expired_at > NOW()
`

func (q *Queries) ClaimOrderPayment(ctx context.Context, orderNo string, userID int32, channel string) (bool, error) {
	res, err := q.db.ExecContext(ctx, claimOrderPayment, channel, orderNo, userID)
	if err != nil {
		return false, err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected == 1, nil
}

// settleOrderPayment 条件更新订单 pending→paid，用于"已确认付款的补出票"。
// 与 claimOrderPayment 的区别：不带 expired_at > NOW() 条件。
// 场景：买家在订单过期前已付款，但异步通知/查单在过期后才到达——
// 此时钱已真实扣除，必须出票（否则钱悬空），所以不能用过期条件拦截。
// 与 ClaimOrderCancel 共用 status='pending' 条件，关单与补出票仍在 DB 层互斥。
const settleOrderPayment = `
UPDATE orders
SET status = 'paid', paid_at = NOW(), pay_channel = ?
WHERE order_no = ? AND user_id = ? AND status = 'pending'
`

func (q *Queries) SettleOrderPayment(ctx context.Context, orderNo string, userID int32, channel string) (bool, error) {
	res, err := q.db.ExecContext(ctx, settleOrderPayment, channel, orderNo, userID)
	if err != nil {
		return false, err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected == 1, nil
}

// setOrderChannel 在发起支付时将渠道写回订单（仅 pending 状态可写），
// 后续主动查单（GetOrderStatus）据此选择正确的渠道，避免 NULL 渠道默认回 mock。
const setOrderChannel = `
UPDATE orders
SET pay_channel = ?
WHERE order_no = ? AND status = 'pending'
`

func (q *Queries) SetOrderChannel(ctx context.Context, orderNo string, channel string) error {
	_, err := q.db.ExecContext(ctx, setOrderChannel, channel, orderNo)
	return err
}

// claimOrderCancel 条件更新订单 pending→canceled，返回是否抢到状态变更。
// 与 ClaimOrderPayment 共用 status='pending' 条件，保证关单与支付在数据库层互斥。
const claimOrderCancel = `
UPDATE orders
SET status = 'canceled'
WHERE order_no = ? AND status = 'pending'
`

func (q *Queries) ClaimOrderCancel(ctx context.Context, orderNo string) (bool, error) {
	res, err := q.db.ExecContext(ctx, claimOrderCancel, orderNo)
	if err != nil {
		return false, err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected == 1, nil
}

// listExpiredOrders 扫描超时未支付的订单，供关单定时任务批量处理。
const listExpiredOrders = `
SELECT id, order_no, user_id, bus_id, seat_id, amount, status, pay_channel, paid_at, expired_at, created_at
FROM orders
WHERE status = 'pending' AND expired_at < ?
LIMIT ?
`

func (q *Queries) ListExpiredOrders(ctx context.Context, now time.Time, limit int) ([]Order, error) {
	rows, err := q.db.QueryContext(ctx, listExpiredOrders, now, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	items := []Order{}
	for rows.Next() {
		var o Order
		if err := rows.Scan(
			&o.ID,
			&o.OrderNo,
			&o.UserID,
			&o.BusID,
			&o.SeatID,
			&o.Amount,
			&o.Status,
			&o.PayChannel,
			&o.PaidAt,
			&o.ExpiredAt,
			&o.CreatedAt,
		); err != nil {
			return nil, err
		}
		items = append(items, o)
	}
	return items, rows.Err()
}
