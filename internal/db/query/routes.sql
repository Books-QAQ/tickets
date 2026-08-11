-- routes.sql

-- name: CreateRoute :one
INSERT INTO routes (origin_terminal_id, destination_terminal_id, duration, distance)
VALUES (?, ?, ?, ?);

-- name: GetAllRoutes :many
SELECT id, origin_terminal_id, destination_terminal_id, duration, distance
FROM routes;

-- name: GetRouteByID :one
SELECT id, origin_terminal_id, destination_terminal_id, duration, distance
FROM routes
WHERE id = ?;

-- name: ListRoutes :many
SELECT 
    r.id AS route_id,
    r.origin_terminal_id,
    r.destination_terminal_id,
    t1.name AS origin_terminal_name,
    t2.name AS destination_terminal_name,
    b.id AS bus_id,
    b.departure_time,
    b.arrival_time,
    b.capacity,
    b.price,
    b.bus_type,
    b.corporation,
    b.super_corporation,
    b.service_number,
    b.is_vip,
    -- Calculate available seats by counting seats in 'available' status
    SUM(CASE WHEN bs.status = 'available' THEN 1 ELSE 0 END) AS available_seats
FROM 
    routes r
    JOIN terminals t1 ON r.origin_terminal_id = t1.id
    JOIN terminals t2 ON r.destination_terminal_id = t2.id
    JOIN buses b ON b.route_id = r.id
    LEFT JOIN bus_seats bs ON bs.bus_id = b.id
WHERE 
    r.origin_terminal_id = ?
    AND r.destination_terminal_id = ?
    AND DATE(b.departure_time) = DATE(?)
GROUP BY 
    r.id,
    r.origin_terminal_id,
    r.destination_terminal_id,
    t1.name,
    t2.name,
    b.id,
    b.departure_time,
    b.arrival_time,
    b.capacity,
    b.price,
    b.bus_type,
    b.corporation,
    b.super_corporation,
    b.service_number,
    b.is_vip;
