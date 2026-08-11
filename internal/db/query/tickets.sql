-- name: GetTicketByID :one
SELECT id, user_id, bus_id, reserved_at, status, seat_reservation_id
FROM tickets
WHERE id = ?;

-- name: DeleteTicket :exec
DELETE FROM tickets
WHERE id = ?;

-- name: GetReservedTicketsCount :one
SELECT COUNT(*)
FROM tickets
WHERE bus_id = ?;

-- name: GetUserTickets :many
SELECT t.id, b.route_id, b.departure_time, b.arrival_time, b.capacity, b.price, b.bus_type, b.corporation, b.super_corporation, b.service_number, b.is_vip
FROM tickets t
JOIN buses b ON t.bus_id = b.id
WHERE t.user_id = ?;

-- name: CreatePenalty :one
INSERT INTO penalties (bus_id, actual_hours_before, hours_before, percent, custom_text)
VALUES (?, ?, ?, ?, ?);

-- name: CreateSeatReservation :one
INSERT INTO seat_reservations (bus_id, bus_seat_id, user_id, status, reserved_at, purchased_at)
VALUES (?, ?, ?, ?, NOW(), ?);

-- name: GetSeatReservationByID :one
SELECT id, bus_id, bus_seat_id, user_id, status, reserved_at, purchased_at
FROM seat_reservations
WHERE id = ?;

-- name: GetBusPenalties :many
SELECT id, bus_id, actual_hours_before, hours_before, percent, custom_text
FROM penalties
WHERE bus_id = ?;

-- name: ReserveTicket :one
INSERT INTO tickets (user_id, bus_id, seat_reservation_id, status, reserved_at)
VALUES (?, ?, ?, 'reserved', NOW());

-- name: PurchaseTicket :one
INSERT INTO tickets (user_id, bus_id, seat_reservation_id, status, purchased_at)
VALUES (?, ?, ?, 'purchased', NOW());

-- name: ListUserTickets :many
SELECT 
    t.id AS ticket_id,
    t.bus_id,
    sr.bus_seat_id AS seat_id,
    t.reserved_at,
    b.departure_time,
    b.arrival_time,
    b.price,
    s.seat_number,
    sr.status AS reservation_status
FROM 
    tickets t
JOIN 
    buses b ON t.bus_id = b.id
JOIN 
    seat_reservations sr ON t.seat_reservation_id = sr.id
JOIN 
    bus_seats s ON sr.bus_seat_id = s.id
WHERE 
    t.user_id = ?
ORDER BY 
    t.reserved_at DESC;

-- name: UpdateTicketStatus :exec
UPDATE tickets
SET status = ?
WHERE id = ?;

-- name: UpdateSeatReservationStatusByID :exec
UPDATE seat_reservations
SET status = ?
WHERE id = ?;
