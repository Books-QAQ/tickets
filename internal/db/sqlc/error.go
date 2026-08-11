package db

import (
	"database/sql"
	"errors"
	"strconv"

	"github.com/go-sql-driver/mysql"
)

const (
	ForeignKeyViolation = "1452"
	UniqueViolation     = "1062"
)

var ErrRecordNotFound = sql.ErrNoRows

func ErrorCode(err error) string {
	var mysqlErr *mysql.MySQLError
	if errors.As(err, &mysqlErr) {
		return strconv.FormatUint(uint64(mysqlErr.Number), 10)
	}
	return ""
}
