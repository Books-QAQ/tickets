-- users.sql

-- name: CreateUser :one
INSERT INTO users (
  username,
  hashed_password,
  full_name
) VALUES (
  ?, ?, ?
);

-- name: GetUserByID :one
SELECT id, username, hashed_password
FROM users
WHERE id = ?;

-- name: GetUserByUsername :one
SELECT id, username, hashed_password
FROM users
WHERE username = ?;

-- name: GetUser :one
SELECT * FROM users
WHERE username = ? LIMIT 1;

-- name: UpdateUser :one
UPDATE users
SET
  full_name = ?
WHERE
  username = ?;

-- name: UpdateUserPassword :one
UPDATE users
SET hashed_password = ?,
    password_changed_at = now()
WHERE username = ?;
