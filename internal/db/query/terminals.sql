-- terminals.sql

-- name: CreateTerminal :one
INSERT INTO terminals (city_id, name)
VALUES (?, ?);

-- name: GetTerminalByID :one
SELECT id, city_id, name
FROM terminals
WHERE id = ?;

-- name: GetTerminalsByCity :many
SELECT id, city_id, name
FROM terminals
WHERE city_id = ?;

-- name: ListTerminals :many
SELECT id, name, city_id
FROM terminals
ORDER BY name;
