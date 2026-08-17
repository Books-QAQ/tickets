package db

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func newOrderRows(orderNo string, status string, seatID int32) *sqlmock.Rows {
	now := time.Now()
	return sqlmock.NewRows([]string{
		"id", "order_no", "user_id", "bus_id", "seat_id", "amount",
		"status", "pay_channel", "paid_at", "expired_at", "created_at",
	}).AddRow(1, orderNo, 1, 5, seatID, 8800, status, "mock", now, now, now)
}

// 验证支付幂等的核心：条件更新 pending→paid，RowsAffected=1 才算出票成功。
func TestClaimOrderPaymentIdempotent(t *testing.T) {
	sqlDB, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer sqlDB.Close()

	q := New(sqlDB)
	ctx := context.Background()

	// 第一次支付：RowsAffected=1 → 抢到状态变更，claimed=true
	mock.ExpectExec("UPDATE orders").
		WillReturnResult(sqlmock.NewResult(0, 1))
	claimed, err := q.ClaimOrderPayment(ctx, "order-1", 1, "mock")
	require.NoError(t, err)
	require.True(t, claimed, "首次支付应抢到状态变更")

	// 第二次支付（重复回调）：RowsAffected=0 → 没抢到，claimed=false
	mock.ExpectExec("UPDATE orders").
		WillReturnResult(sqlmock.NewResult(0, 0))
	claimed, err = q.ClaimOrderPayment(ctx, "order-1", 1, "mock")
	require.NoError(t, err)
	require.False(t, claimed, "重复支付不应抢到状态变更")

	require.NoError(t, mock.ExpectationsWereMet())
}

// 验证重复支付整条链路：订单已 paid 时，PayOrderTx 返回 already_paid 且不出票。
func TestPayOrderTxRepeatPaymentIsIdempotent(t *testing.T) {
	sqlDB, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer sqlDB.Close()

	store := NewStore(sqlDB)
	ctx := context.Background()

	mock.ExpectBegin()

	// ① 条件更新订单：RowsAffected=0，说明已是 paid（重复支付）
	mock.ExpectExec("UPDATE orders").
		WillReturnResult(sqlmock.NewResult(0, 0))

	// ② 查订单当前状态 → paid
	mock.ExpectQuery("SELECT id, order_no").
		WillReturnRows(newOrderRows("order-1", "paid", 10))

	// ③ already_paid 分支直接返回 nil，提交（无出票写操作）
	mock.ExpectCommit()

	result, err := store.PayOrderTx(ctx, PayOrderTxParams{
		OrderNo: "order-1",
		UserID:  1,
		BusID:   5,
		SeatID:  10,
		Channel: "mock",
	})
	require.NoError(t, err)
	require.Equal(t, "already_paid", result.Status, "重复支付应返回 already_paid")
	require.Zero(t, result.TicketID, "重复支付不应生成新票")

	require.NoError(t, mock.ExpectationsWereMet())
}

// 验证首次支付正常出票：订单 pending→paid，座位 reserved→purchased，生成票。
func TestPayOrderTxFirstPayment(t *testing.T) {
	sqlDB, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer sqlDB.Close()

	store := NewStore(sqlDB)
	ctx := context.Background()
	now := time.Now()

	mock.ExpectBegin()

	// ① 订单 pending→paid，RowsAffected=1
	mock.ExpectExec("UPDATE orders").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// ② 座位 reserved→purchased，RowsAffected=1
	mock.ExpectExec("UPDATE bus_seats").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// ③ 创建 seat_reservation：INSERT → LastInsertId=101 → 回读
	mock.ExpectExec("INSERT INTO seat_reservations").
		WillReturnResult(sqlmock.NewResult(101, 1))
	reservationRows := sqlmock.NewRows([]string{
		"id", "bus_id", "bus_seat_id", "user_id", "status", "reserved_at", "purchased_at",
	}).AddRow(101, 5, 10, 1, "purchased", now, now)
	mock.ExpectQuery("SELECT id, bus_id, bus_seat_id, user_id, status, reserved_at, purchased_at").
		WillReturnRows(reservationRows)

	// ④ 创建 ticket：INSERT → LastInsertId=202 → 回读
	mock.ExpectExec("INSERT INTO tickets").
		WillReturnResult(sqlmock.NewResult(202, 1))
	ticketRows := sqlmock.NewRows([]string{
		"id", "user_id", "bus_id", "seat_reservation_id", "status", "purchased_at",
	}).AddRow(202, 1, 5, 101, "purchased", now)
	mock.ExpectQuery("SELECT id, user_id, bus_id, seat_reservation_id, status, purchased_at").
		WillReturnRows(ticketRows)

	mock.ExpectCommit()

	result, err := store.PayOrderTx(ctx, PayOrderTxParams{
		OrderNo: "order-1",
		UserID:  1,
		BusID:   5,
		SeatID:  10,
		Channel: "mock",
	})
	require.NoError(t, err)
	require.Equal(t, "paid", result.Status)
	require.NotZero(t, result.TicketID)

	require.NoError(t, mock.ExpectationsWereMet())
}

// 验证下单事务：座位 available→reserved + 创建 pending 订单。
func TestCreateOrderTxReservesSeat(t *testing.T) {
	sqlDB, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer sqlDB.Close()

	store := NewStore(sqlDB)
	ctx := context.Background()

	mock.ExpectBegin()

	// ① 座位 available→reserved，RowsAffected=1
	mock.ExpectExec("UPDATE bus_seats").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// ② 创建订单
	mock.ExpectExec("INSERT INTO orders").
		WillReturnResult(sqlmock.NewResult(1, 1))

	// ③ 读回订单
	mock.ExpectQuery("SELECT id, order_no").
		WillReturnRows(newOrderRows("order-1", "pending", 10))

	mock.ExpectCommit()

	order, err := store.CreateOrderTx(ctx, CreateOrderTxParams{
		OrderNo:   "order-1",
		UserID:    1,
		BusID:     5,
		SeatID:    10,
		Amount:    8800,
		ExpiredAt: time.Now().Add(15 * time.Minute),
	})
	require.NoError(t, err)
	require.Equal(t, "pending", order.Status)
	require.Equal(t, int32(10), order.SeatID)

	require.NoError(t, mock.ExpectationsWereMet())
}

// 验证下单并发：座位已被占（reserved），第二个下单事务回滚失败。
func TestCreateOrderTxSeatAlreadyTaken(t *testing.T) {
	sqlDB, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer sqlDB.Close()

	store := NewStore(sqlDB)
	ctx := context.Background()

	mock.ExpectBegin()

	// 座位 available→reserved 失败，RowsAffected=0（已被占）
	mock.ExpectExec("UPDATE bus_seats").
		WillReturnResult(sqlmock.NewResult(0, 0))

	mock.ExpectRollback()

	_, err = store.CreateOrderTx(ctx, CreateOrderTxParams{
		OrderNo:   "order-2",
		UserID:    2,
		BusID:     5,
		SeatID:    10,
		Amount:    8800,
		ExpiredAt: time.Now().Add(15 * time.Minute),
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "seat is no longer available")

	require.NoError(t, mock.ExpectationsWereMet())
}

// 验证超时关单：订单 pending→canceled + 座位 reserved→available。
func TestCancelExpiredOrderTx(t *testing.T) {
	sqlDB, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer sqlDB.Close()

	store := NewStore(sqlDB)
	ctx := context.Background()

	mock.ExpectBegin()

	// ① 读订单 → pending，拿到 seatID=10
	mock.ExpectQuery("SELECT id, order_no").
		WillReturnRows(newOrderRows("order-1", "pending", 10))

	// ② 订单 pending→canceled，RowsAffected=1
	mock.ExpectExec("UPDATE orders").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// ③ 座位 reserved→available，RowsAffected=1
	mock.ExpectExec("UPDATE bus_seats").
		WillReturnResult(sqlmock.NewResult(0, 1))

	mock.ExpectCommit()

	order, err := store.CancelExpiredOrderTx(ctx, "order-1")
	require.NoError(t, err)
	require.Equal(t, "canceled", order.Status)

	require.NoError(t, mock.ExpectationsWereMet())
}

// 验证关单 vs 支付竞态：订单已 paid 时，关单事务跳过，不释放座位（不误释放已售出的）。
func TestCancelExpiredOrderTxRaceWithPayment(t *testing.T) {
	sqlDB, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer sqlDB.Close()

	store := NewStore(sqlDB)
	ctx := context.Background()

	mock.ExpectBegin()

	// ① 读订单 → paid（支付先到）
	mock.ExpectQuery("SELECT id, order_no").
		WillReturnRows(newOrderRows("order-1", "paid", 10))

	// ② status != pending，直接返回，提交（没有 UPDATE orders / UPDATE bus_seats）
	mock.ExpectCommit()

	order, err := store.CancelExpiredOrderTx(ctx, "order-1")
	require.NoError(t, err)
	require.Equal(t, "paid", order.Status, "已支付订单不应被关单")

	// 关键断言：没有调用 ClaimOrderCancel 和 UpdateBusSeatStatusIf
	require.NoError(t, mock.ExpectationsWereMet())
}
