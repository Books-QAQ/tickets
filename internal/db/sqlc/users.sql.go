package db

import "context"

const createUser = `-- name: CreateUser :one
INSERT INTO users (
  username,
  hashed_password,
  full_name
) VALUES (
  ?, ?, ?
)
`

type CreateUserParams struct {
	Username       string `json:"username"`
	HashedPassword string `json:"hashed_password"`
	FullName       string `json:"full_name"`
}

func (q *Queries) CreateUser(ctx context.Context, arg CreateUserParams) (User, error) {
	_, err := q.db.ExecContext(ctx, createUser, arg.Username, arg.HashedPassword, arg.FullName)
	if err != nil {
		return User{}, err
	}

	return q.GetUser(ctx, arg.Username)
}

const getUser = `-- name: GetUser :one
SELECT id, username, hashed_password, full_name, password_changed_at, created_at FROM users
WHERE username = ? LIMIT 1
`

func (q *Queries) GetUser(ctx context.Context, username string) (User, error) {
	row := q.db.QueryRowContext(ctx, getUser, username)
	var i User
	err := row.Scan(
		&i.ID,
		&i.Username,
		&i.HashedPassword,
		&i.FullName,
		&i.PasswordChangedAt,
		&i.CreatedAt,
	)
	return i, err
}

const getUserByID = `-- name: GetUserByID :one
SELECT id, username, hashed_password
FROM users
WHERE id = ?
`

type GetUserByIDRow struct {
	ID             int32  `json:"id"`
	Username       string `json:"username"`
	HashedPassword string `json:"hashed_password"`
}

func (q *Queries) GetUserByID(ctx context.Context, id int32) (GetUserByIDRow, error) {
	row := q.db.QueryRowContext(ctx, getUserByID, id)
	var i GetUserByIDRow
	err := row.Scan(&i.ID, &i.Username, &i.HashedPassword)
	return i, err
}

const getUserByUsername = `-- name: GetUserByUsername :one
SELECT id, username, hashed_password
FROM users
WHERE username = ?
`

type GetUserByUsernameRow struct {
	ID             int32  `json:"id"`
	Username       string `json:"username"`
	HashedPassword string `json:"hashed_password"`
}

func (q *Queries) GetUserByUsername(ctx context.Context, username string) (GetUserByUsernameRow, error) {
	row := q.db.QueryRowContext(ctx, getUserByUsername, username)
	var i GetUserByUsernameRow
	err := row.Scan(&i.ID, &i.Username, &i.HashedPassword)
	return i, err
}

const updateUser = `-- name: UpdateUser :one
UPDATE users
SET full_name = ?
WHERE username = ?
`

type UpdateUserParams struct {
	FullName string `json:"full_name"`
	Username string `json:"username"`
}

func (q *Queries) UpdateUser(ctx context.Context, arg UpdateUserParams) (User, error) {
	_, err := q.db.ExecContext(ctx, updateUser, arg.FullName, arg.Username)
	if err != nil {
		return User{}, err
	}

	return q.GetUser(ctx, arg.Username)
}

const updateUserPassword = `-- name: UpdateUserPassword :one
UPDATE users
SET hashed_password = ?,
    password_changed_at = NOW()
WHERE username = ?
`

type UpdateUserPasswordParams struct {
	HashedPassword string `json:"hashed_password"`
	Username       string `json:"username"`
}

func (q *Queries) UpdateUserPassword(ctx context.Context, arg UpdateUserPasswordParams) (User, error) {
	_, err := q.db.ExecContext(ctx, updateUserPassword, arg.HashedPassword, arg.Username)
	if err != nil {
		return User{}, err
	}

	return q.GetUser(ctx, arg.Username)
}
