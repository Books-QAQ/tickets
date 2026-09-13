package db

import (
	"context"
	crand "crypto/rand"
	"database/sql"
	"encoding/hex"
	"time"
)

// 本文件是**手写**的 sqlc 之外的查询（与 orders_extra.go 同样的取舍）：
// 客服工具层需要的聚合查询用 sqlc 表达会牵扯到多表 join 与动态条件，
// 手写 SQL + 显式 Scan 更可控，也不必为了改一句 SQL 重跑 sqlc 生成。

// ---------- 订单：按用户 + 状态列表（待支付 / 已退款 / 最近订单消歧） ----------

// ListOrdersByUser 按用户列订单；status 为空串表示不限状态。
// 一律以 user_id 为第一条件（§8.5.2：每条 SQL 强制 WHERE user_id = ?）。
const listOrdersByUser = `
SELECT id, order_no, user_id, bus_id, seat_id, amount, status, pay_channel, paid_at, expired_at, created_at
FROM orders
WHERE user_id = ? AND (? = '' OR status = ?)
ORDER BY created_at DESC
LIMIT ?`

func (q *Queries) ListOrdersByUser(ctx context.Context, userID int32, status string, limit int) ([]Order, error) {
	rows, err := q.db.QueryContext(ctx, listOrdersByUser, userID, status, status, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	items := []Order{}
	for rows.Next() {
		var o Order
		if err := rows.Scan(
			&o.ID, &o.OrderNo, &o.UserID, &o.BusID, &o.SeatID, &o.Amount,
			&o.Status, &o.PayChannel, &o.PaidAt, &o.ExpiredAt, &o.CreatedAt,
		); err != nil {
			return nil, err
		}
		items = append(items, o)
	}
	return items, rows.Err()
}

// GetOrderByNoForUser 带归属的订单查询：跨用户查询返回 sql.ErrNoRows（工具层统一转"未查询到"）。
// 注意**不是**先查再判断 403 —— 不泄露"该订单号是否存在"（§8.5.2）。
const getOrderByNoForUser = `
SELECT id, order_no, user_id, bus_id, seat_id, amount, status, pay_channel, paid_at, expired_at, created_at
FROM orders
WHERE order_no = ? AND user_id = ?`

func (q *Queries) GetOrderByNoForUser(ctx context.Context, orderNo string, userID int32) (Order, error) {
	row := q.db.QueryRowContext(ctx, getOrderByNoForUser, orderNo, userID)
	var o Order
	err := row.Scan(
		&o.ID, &o.OrderNo, &o.UserID, &o.BusID, &o.SeatID, &o.Amount,
		&o.Status, &o.PayChannel, &o.PaidAt, &o.ExpiredAt, &o.CreatedAt,
	)
	return o, err
}

// ListPendingOrdersForUser 待支付且**未过期**的订单。
// 过滤条件写在 SQL 里（`expired_at > NOW()`）而不是拿回 Go 再比 ——
// 两侧都用数据库时钟，避免"Go 本地时钟 vs DB 会话时区"不一致导致的误判（M2 实测踩过：
// 容器会话默认 UTC，夹具按 UTC 写入，Go 按 local 比较，15 分钟后到期的订单被判成已过期）。
// 剩余支付时间也由 SQL 算（TIMESTAMPDIFF），口径与关单任务完全一致。
const listPendingOrdersForUser = `
SELECT id, order_no, user_id, bus_id, seat_id, amount, status, pay_channel, paid_at, expired_at, created_at,
       TIMESTAMPDIFF(MINUTE, NOW(), expired_at) AS left_minutes
FROM orders
WHERE user_id = ? AND status = 'pending' AND expired_at > NOW()
ORDER BY expired_at ASC
LIMIT ?`

// PendingOrder 待支付订单（带 SQL 算出的剩余分钟）
type PendingOrder struct {
	Order
	LeftMinutes int64 `json:"left_minutes"`
}

func (q *Queries) ListPendingOrdersForUser(ctx context.Context, userID int32, limit int) ([]PendingOrder, error) {
	rows, err := q.db.QueryContext(ctx, listPendingOrdersForUser, userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	items := []PendingOrder{}
	for rows.Next() {
		var p PendingOrder
		if err := rows.Scan(
			&p.ID, &p.OrderNo, &p.UserID, &p.BusID, &p.SeatID, &p.Amount,
			&p.Status, &p.PayChannel, &p.PaidAt, &p.ExpiredAt, &p.CreatedAt, &p.LeftMinutes,
		); err != nil {
			return nil, err
		}
		items = append(items, p)
	}
	return items, rows.Err()
}

// ---------- 车票：带线路/车站/座位的明细（我的车票） ----------

// UserTicketDetail 车票明细（工具层输出用；已含出发/到达站与城市）
type UserTicketDetail struct {
	UserTicketID      int32        `json:"user_ticket_id"`
	BusID             int32        `json:"bus_id"`
	RouteID           int32        `json:"route_id"`
	Status            string       `json:"status"` // tickets.status: reserved|purchased|canceled
	DepartureTime     time.Time    `json:"departure_time"`
	ArrivalTime       time.Time    `json:"arrival_time"`
	Price             int32        `json:"price"`
	SeatNumber        int32        `json:"seat_number"`
	OriginTerminal    string       `json:"origin_terminal"`
	OriginCity        string       `json:"origin_city"`
	DestinationTerm   string       `json:"destination_terminal"`
	DestinationCity   string       `json:"destination_city"`
	ServiceNumber     sql.NullString `json:"service_number"`
	ReservationStatus string       `json:"reservation_status"`
	ReservedAt        sql.NullTime `json:"reserved_at"`
}

const listUserTicketsDetailed = `
SELECT
    t.id AS ticket_id, t.bus_id, b.route_id, t.status,
    b.departure_time, b.arrival_time, b.price, s.seat_number,
    t1.name AS origin_terminal, c1.name AS origin_city,
    t2.name AS destination_terminal, c2.name AS destination_city,
    b.service_number, sr.status AS reservation_status, t.reserved_at
FROM tickets t
JOIN buses b        ON t.bus_id = b.id
JOIN routes r       ON b.route_id = r.id
JOIN terminals t1   ON r.origin_terminal_id = t1.id
JOIN terminals t2   ON r.destination_terminal_id = t2.id
JOIN cities c1      ON t1.city_id = c1.id
JOIN cities c2      ON t2.city_id = c2.id
JOIN seat_reservations sr ON t.seat_reservation_id = sr.id
JOIN bus_seats s    ON sr.bus_seat_id = s.id
WHERE t.user_id = ?
ORDER BY t.reserved_at DESC`

func (q *Queries) ListUserTicketsDetailed(ctx context.Context, userID int32) ([]UserTicketDetail, error) {
	rows, err := q.db.QueryContext(ctx, listUserTicketsDetailed, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	items := []UserTicketDetail{}
	for rows.Next() {
		var it UserTicketDetail
		if err := rows.Scan(
			&it.UserTicketID, &it.BusID, &it.RouteID, &it.Status,
			&it.DepartureTime, &it.ArrivalTime, &it.Price, &it.SeatNumber,
			&it.OriginTerminal, &it.OriginCity,
			&it.DestinationTerm, &it.DestinationCity,
			&it.ServiceNumber, &it.ReservationStatus, &it.ReservedAt,
		); err != nil {
			return nil, err
		}
		items = append(items, it)
	}
	return items, rows.Err()
}

// CountAvailableSeats 单班次余票（v1 只读，工具用；与前端同源：bus_seats.status='available'）
const countAvailableSeats = `SELECT COUNT(*) FROM bus_seats WHERE bus_id = ? AND status = 'available'`

func (q *Queries) CountAvailableSeats(ctx context.Context, busID int32) (int64, error) {
	var n int64
	err := q.db.QueryRowContext(ctx, countAvailableSeats, busID).Scan(&n)
	return n, err
}

// ---------- 客服工单（support_tickets） ----------

// SupportTicket 客服工单（人工转接的落库形态）
type SupportTicket struct {
	ID        int64          `json:"id"`
	TicketNo  string         `json:"ticket_no"`
	ConvID    sql.NullString `json:"conv_id"`
	UserID    sql.NullInt32  `json:"user_id"`
	OrderNo   sql.NullString `json:"order_no"`
	Category  string         `json:"category"`
	Path      string         `json:"path"`
	Status    string         `json:"status"`
	Summary   string         `json:"summary"`
	CreatedAt time.Time      `json:"created_at"`
}

const insertSupportTicket = `
INSERT INTO support_tickets (ticket_no, conv_id, user_id, order_no, category, path, status, summary)
VALUES (?, ?, ?, ?, ?, ?, 'pending', ?)`

// 工单号 = CS + yyyymmdd + '-' + 4 位自增，取的是**本行自增主键**（表内唯一，无需额外序列表）。
// 两段式（先插占位 UUID 再改号）是为了并发安全：直接拼 MAX(id)+1 会有竞态，
// 而 ticket_no 有 UNIQUE 约束，占位值必须天然不重复。
const finalizeSupportTicketNo = `
UPDATE support_tickets
SET ticket_no = CONCAT('CS', DATE_FORMAT(created_at, '%Y%m%d'), '-', LPAD(id, 4, '0'))
WHERE id = ?`

func (store *Store) CreateSupportTicket(ctx context.Context, arg CreateSupportTicketParams) (SupportTicket, error) {
	var id int64
	err := store.execTx(ctx, func(q *Queries) error {
		// 占位值必须天然不重复（ticket_no 有 UNIQUE 约束），用随机 hex 而不是时间戳
		buf := make([]byte, 12)
		if _, err := crand.Read(buf); err != nil {
			return err
		}
		placeholder := "tmp-" + hex.EncodeToString(buf)

		res, err := q.db.ExecContext(ctx, insertSupportTicket,
			placeholder, arg.ConvID, arg.UserID, arg.OrderNo, arg.Category, arg.Path, arg.Summary)
		if err != nil {
			return err
		}
		if id, err = res.LastInsertId(); err != nil {
			return err
		}
		_, err = q.db.ExecContext(ctx, finalizeSupportTicketNo, id)
		return err
	})
	if err != nil {
		return SupportTicket{}, err
	}
	return store.GetSupportTicketByID(ctx, id)
}

type CreateSupportTicketParams struct {
	ConvID   sql.NullString
	UserID   sql.NullInt32
	OrderNo  sql.NullString
	Category string
	Path     string
	Summary  string
}

const getSupportTicketByID = `
SELECT id, ticket_no, conv_id, user_id, order_no, category, path, status, summary, created_at
FROM support_tickets WHERE id = ?`

func (q *Queries) GetSupportTicketByID(ctx context.Context, id int64) (SupportTicket, error) {
	var t SupportTicket
	err := q.db.QueryRowContext(ctx, getSupportTicketByID, id).Scan(
		&t.ID, &t.TicketNo, &t.ConvID, &t.UserID, &t.OrderNo,
		&t.Category, &t.Path, &t.Status, &t.Summary, &t.CreatedAt,
	)
	return t, err
}

const listSupportTicketsByUser = `
SELECT id, ticket_no, conv_id, user_id, order_no, category, path, status, summary, created_at
FROM support_tickets
WHERE user_id = ?
ORDER BY created_at DESC
LIMIT ?`

// ListSupportTicketsByUser 用户查自己的工单（§10.4 V1 范围）；
// user_id 一律来自服务端解析，接口签名上不接受客户端传入。
func (q *Queries) ListSupportTicketsByUser(ctx context.Context, userID int32, limit int) ([]SupportTicket, error) {
	rows, err := q.db.QueryContext(ctx, listSupportTicketsByUser, userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	items := []SupportTicket{}
	for rows.Next() {
		var t SupportTicket
		if err := rows.Scan(
			&t.ID, &t.TicketNo, &t.ConvID, &t.UserID, &t.OrderNo,
			&t.Category, &t.Path, &t.Status, &t.Summary, &t.CreatedAt,
		); err != nil {
			return nil, err
		}
		items = append(items, t)
	}
	return items, rows.Err()
}

// UpdateSupportTicketStatus 条件更新（照抄支付/关单链路纪律）：
// WHERE status = ? 保证"已关闭的工单拒绝再回复/再分配"，以影响行数为判定依据。
const updateSupportTicketStatus = `
UPDATE support_tickets SET status = ?, resolved_at = ?
WHERE id = ? AND status = ?`

func (q *Queries) UpdateSupportTicketStatus(ctx context.Context, id int64, from, to string, resolvedAt sql.NullTime) (bool, error) {
	res, err := q.db.ExecContext(ctx, updateSupportTicketStatus, to, resolvedAt, id, from)
	if err != nil {
		return false, err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected == 1, nil
}
