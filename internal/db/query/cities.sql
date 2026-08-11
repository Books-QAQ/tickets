-- cities.sql

-- name: GetCityByID :one
SELECT id, name
FROM cities
WHERE id = ?;

-- name: CreateCity :one
INSERT INTO cities (name)
VALUES (?);

-- name: GetAllCities :many
SELECT id, name
FROM cities;
