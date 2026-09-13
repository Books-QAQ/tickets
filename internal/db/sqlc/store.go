package db

import "database/sql"

// Store bundles query helpers and the shared database handle.
type Store struct {
	*Queries
	db *sql.DB
}

// RawDB 暴露底层连接（客服侧有一批聚合查询不适合走 sqlc 生成层：会话列表/消息/反馈）
func (s *Store) RawDB() *sql.DB { return s.db }

// NewStore create a new store
func NewStore(db *sql.DB) *Store {
	return &Store{
		db:      db,
		Queries: New(db),
	}
}
