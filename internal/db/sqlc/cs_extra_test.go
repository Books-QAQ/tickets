package db

import (
	"context"
	"database/sql"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

// 越权矩阵的 SQL 层铁证（§8.5）：**每条查询都必须带 user_id 条件**。
// 这里不测业务逻辑，只钉死"跨用户查询在 SQL 层就不可能命中"。

func TestGetOrderByNoForUserAlwaysScopedByUser(t *testing.T) {
	sqlDB, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer sqlDB.Close()

	// 关键断言：WITH user_id 条件，且两个参数（订单号 + 用户）都传下去
	mock.ExpectQuery(regexp.QuoteMeta("WHERE order_no = ? AND user_id = ?")).
		WithArgs("3f2a1b4c-5d6e-7f80-9a0b-1c2d3e4f5a6b", int32(9)).
		WillReturnError(sql.ErrNoRows)

	q := New(sqlDB)
	_, err = q.GetOrderByNoForUser(context.Background(), "3f2a1b4c-5d6e-7f80-9a0b-1c2d3e4f5a6b", 9)
	require.ErrorIs(t, err, sql.ErrNoRows, "跨用户订单必须查不到（统一返回未查询到）")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestListOrdersByUserScoped(t *testing.T) {
	sqlDB, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer sqlDB.Close()

	now := time.Now()
	rows := sqlmock.NewRows([]string{"id", "order_no", "user_id", "bus_id", "seat_id", "amount",
		"status", "pay_channel", "paid_at", "expired_at", "created_at"}).
		AddRow(1, "o-1", 9, 3, 4, 120, "pending", nil, nil, now, now)

	mock.ExpectQuery(regexp.QuoteMeta("WHERE user_id = ? AND (? = '' OR status = ?)")).
		WithArgs(int32(9), "pending", "pending", 5).
		WillReturnRows(rows)

	q := New(sqlDB)
	got, err := q.ListOrdersByUser(context.Background(), 9, "pending", 5)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, int32(9), got[0].UserID)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestCreateSupportTicketGeneratesTrackableNo(t *testing.T) {
	sqlDB, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer sqlDB.Close()

	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO support_tickets")).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), "refund", "threshold", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(42, 1))
	// 工单号由**自增主键**派生（并发安全），不是 MAX(id)+1 那种竞态写法
	mock.ExpectExec(regexp.QuoteMeta("UPDATE support_tickets")).
		WithArgs(int64(42)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	mock.ExpectQuery(regexp.QuoteMeta("SELECT id, ticket_no, conv_id, user_id, order_no, category, path, status, summary, created_at")).
		WithArgs(int64(42)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "ticket_no", "conv_id", "user_id", "order_no",
			"category", "path", "status", "summary", "created_at"}).
			AddRow(42, "CS20260912-0042", nil, int32(9), nil, "refund", "threshold", "pending", "分类：refund；", time.Now()))

	store := NewStore(sqlDB)
	tk, err := store.CreateSupportTicket(context.Background(), CreateSupportTicketParams{
		UserID:   sql.NullInt32{Int32: 9, Valid: true},
		Category: "refund",
		Path:     "threshold",
		Summary:  "分类：refund；",
	})
	require.NoError(t, err)
	require.Equal(t, "CS20260912-0042", tk.TicketNo)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestUpdateSupportTicketStatusIsConditional(t *testing.T) {
	sqlDB, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer sqlDB.Close()

	// 条件更新：WHERE id = ? AND status = ?（照抄支付/关单纪律）
	mock.ExpectExec(regexp.QuoteMeta("WHERE id = ? AND status = ?")).
		WithArgs("closed", sql.NullTime{}, int64(7), "pending").
		WillReturnResult(sqlmock.NewResult(0, 0)) // 影响 0 行 = 状态已变，拒绝

	q := New(sqlDB)
	ok, err := q.UpdateSupportTicketStatus(context.Background(), 7, "pending", "closed", sql.NullTime{})
	require.NoError(t, err)
	require.False(t, ok, "影响行数为 0 时必须判定失败（不允许无条件覆盖状态）")
	require.NoError(t, mock.ExpectationsWereMet())
}
