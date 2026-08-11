-- buses.sql

-- name: CreateBus :one
INSERT INTO buses (route_id, departure_time, arrival_time, capacity, price, bus_type, corporation, super_corporation, service_number, is_vip)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: SearchBuses :many
SELECT b.id, b.route_id, b.departure_time, b.arrival_time, b.capacity, b.price, b.bus_type, b.corporation, b.super_corporation, b.service_number, b.is_vip
FROM buses b
JOIN routes r ON b.route_id = r.id
WHERE r.origin_terminal_id = ? AND r.destination_terminal_id = ? AND b.departure_time >= ?;

-- name: SearchBusesByCities :many
SELECT b.id, b.route_id, b.departure_time, b.arrival_time, b.capacity, b.price, b.bus_type, b.corporation, b.super_corporation, b.service_number, b.is_vip
FROM buses b
JOIN routes r ON b.route_id = r.id
JOIN terminals t_origin ON r.origin_terminal_id = t_origin.id
JOIN terminals t_destination ON r.destination_terminal_id = t_destination.id
WHERE t_origin.city_id = ? AND t_destination.city_id = ? AND b.departure_time >= ?;


-- name: GetBusByID :one
SELECT id, route_id, departure_time, arrival_time, capacity, price, bus_type, corporation, super_corporation, service_number, is_vip
FROM buses
WHERE id = ?;


-- name: CreateBusSeat :one
INSERT INTO bus_seats (bus_id, seat_number, status)
VALUES (?, ?, 'available');

-- name: GetBusSeats :many
SELECT id, bus_id, seat_number, status
FROM bus_seats
WHERE bus_id = ?
ORDER BY seat_number;

-- name: GetSeatByID :one
SELECT 
    bs.id AS seat_id,                
    bs.bus_id,                  
    bs.seat_number, 
    bs.status AS seat_status,              
    sr.status AS reservation_status,
    sr.user_id
FROM 
    bus_seats bs
LEFT JOIN 
    seat_reservations sr ON bs.id = sr.bus_seat_id
WHERE 
    bs.id = ?
    AND bs.bus_id = ? 
LIMIT 1;

-- name: GetAvailableSeatsForBus :many
SELECT
    bs.id AS seat_id,
    bs.seat_number,
    bs.status
FROM
    bus_seats bs
JOIN 
    buses b ON bs.bus_id = b.id
WHERE
    b.route_id = ?
    AND bs.bus_id = ?
    AND bs.status = 'available' -- Only select seats that are available
ORDER BY
    bs.seat_number;



-- name: UpdateSeatReservationStatus :exec
UPDATE seat_reservations
SET 
    status = ?
WHERE 
    bus_seat_id = ?
    AND user_id = ?;

-- name: UpdateBusSeatStatus :exec
UPDATE bus_seats
SET status = ?
WHERE id = ?;


-- name: CheckBusRouteAssociation :one
SELECT 
    b.id AS bus_id,
    r.id AS route_id
FROM 
    buses b
JOIN 
    routes r ON b.route_id = r.id
WHERE 
    b.id = ?  -- BusID
    AND r.id = ?  -- RouteID
LIMIT 1;

-- name: CheckSeatAvailability :one
SELECT 
    s.id AS seat_id, 
    s.status 
FROM 
    bus_seats s
LEFT JOIN 
    seat_reservations sr ON s.id = sr.bus_seat_id AND sr.status IN ('reserved', 'purchased')
WHERE 
    s.id = ? 
    AND s.bus_id = ?
    AND s.status = 'available' -- Ensures seat is available
    AND sr.id IS NULL;  -- Ensures no conflicting reservation or purchase exists


-- name: UpdateSeatStatusAfterTrip :exec
UPDATE bus_seats
SET status = 'available'
WHERE bus_id = ?
  AND status = 'purchased';
