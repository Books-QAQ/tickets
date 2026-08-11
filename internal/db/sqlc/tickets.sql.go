package db

import (
	"context"
	"database/sql"
	"time"
)

func scanPenalty(scanner rowScanner) (Penalty, error) {
	var i Penalty
	err := scanner.Scan(
		&i.ID,
		&i.BusID,
		&i.ActualHoursBefore,
		&i.HoursBefore,
		&i.Percent,
		&i.CustomText,
	)
	return i, err
}

const createPenalty = `-- name: CreatePenalty :one
INSERT INTO penalties (bus_id, actual_hours_before, hours_before, percent, custom_text)
VALUES (?, ?, ?, ?, ?)
`

type CreatePenaltyParams struct {
	BusID             int32           `json:"bus_id"`
	ActualHoursBefore sql.NullFloat64 `json:"actual_hours_before"`
	HoursBefore       sql.NullFloat64 `json:"hours_before"`
	Percent           int32           `json:"percent"`
	CustomText        sql.NullString  `json:"custom_text"`
}

func (q *Queries) CreatePenalty(ctx context.Context, arg CreatePenaltyParams) (Penalty, error) {
	result, err := q.db.ExecContext(ctx, createPenalty,
		arg.BusID,
		arg.ActualHoursBefore,
		arg.HoursBefore,
		arg.Percent,
		arg.CustomText,
	)
	if err != nil {
		return Penalty{}, err
	}

	id, err := result.LastInsertId()
	if err != nil {
		return Penalty{}, err
	}

	row := q.db.QueryRowContext(ctx, "SELECT id, bus_id, actual_hours_before, hours_before, percent, custom_text FROM penalties WHERE id = ?", id)
	return scanPenalty(row)
}

const createSeatReservation = `-- name: CreateSeatReservation :one
INSERT INTO seat_reservations (bus_id, bus_seat_id, user_id, status, reserved_at, purchased_at)
VALUES (?, ?, ?, ?, NOW(), ?)
`

type CreateSeatReservationParams struct {
	BusID       int32        `json:"bus_id"`
	BusSeatID   int32        `json:"bus_seat_id"`
	UserID      int32        `json:"user_id"`
	Status      string       `json:"status"`
	PurchasedAt sql.NullTime `json:"purchased_at"`
}

func (q *Queries) CreateSeatReservation(ctx context.Context, arg CreateSeatReservationParams) (SeatReservation, error) {
	result, err := q.db.ExecContext(ctx, createSeatReservation, arg.BusID, arg.BusSeatID, arg.UserID, arg.Status, arg.PurchasedAt)
	if err != nil {
		return SeatReservation{}, err
	}

	id, err := result.LastInsertId()
	if err != nil {
		return SeatReservation{}, err
	}

	return q.GetSeatReservationByID(ctx, int32(id))
}

const deleteTicket = `-- name: DeleteTicket :exec
DELETE FROM tickets
WHERE id = ?
`

func (q *Queries) DeleteTicket(ctx context.Context, id int32) error {
	_, err := q.db.ExecContext(ctx, deleteTicket, id)
	return err
}

const getBusPenalties = `-- name: GetBusPenalties :many
SELECT id, bus_id, actual_hours_before, hours_before, percent, custom_text
FROM penalties
WHERE bus_id = ?
`

func (q *Queries) GetBusPenalties(ctx context.Context, busID int32) ([]Penalty, error) {
	rows, err := q.db.QueryContext(ctx, getBusPenalties, busID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	items := []Penalty{}
	for rows.Next() {
		item, err := scanPenalty(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return items, nil
}

const getSeatReservationByID = `-- name: GetSeatReservationByID :one
SELECT id, bus_id, bus_seat_id, user_id, status, reserved_at, purchased_at
FROM seat_reservations
WHERE id = ?
`

func (q *Queries) GetSeatReservationByID(ctx context.Context, id int32) (SeatReservation, error) {
	row := q.db.QueryRowContext(ctx, getSeatReservationByID, id)
	var i SeatReservation
	err := row.Scan(
		&i.ID,
		&i.BusID,
		&i.BusSeatID,
		&i.UserID,
		&i.Status,
		&i.ReservedAt,
		&i.PurchasedAt,
	)
	return i, err
}

const getReservedTicketsCount = `-- name: GetReservedTicketsCount :one
SELECT COUNT(*)
FROM tickets
WHERE bus_id = ?
`

func (q *Queries) GetReservedTicketsCount(ctx context.Context, busID int32) (int64, error) {
	row := q.db.QueryRowContext(ctx, getReservedTicketsCount, busID)
	var count int64
	err := row.Scan(&count)
	return count, err
}

const getTicketByID = `-- name: GetTicketByID :one
SELECT id, user_id, bus_id, reserved_at, status, seat_reservation_id
FROM tickets
WHERE id = ?
`

type GetTicketByIDRow struct {
	ID                int32        `json:"id"`
	UserID            int32        `json:"user_id"`
	BusID             int32        `json:"bus_id"`
	ReservedAt        sql.NullTime `json:"reserved_at"`
	Status            string       `json:"status"`
	SeatReservationID int32        `json:"seat_reservation_id"`
}

func (q *Queries) GetTicketByID(ctx context.Context, id int32) (GetTicketByIDRow, error) {
	row := q.db.QueryRowContext(ctx, getTicketByID, id)
	var i GetTicketByIDRow
	err := row.Scan(
		&i.ID,
		&i.UserID,
		&i.BusID,
		&i.ReservedAt,
		&i.Status,
		&i.SeatReservationID,
	)
	return i, err
}

const getUserTickets = `-- name: GetUserTickets :many
SELECT t.id, b.route_id, b.departure_time, b.arrival_time, b.capacity, b.price, b.bus_type, b.corporation, b.super_corporation, b.service_number, b.is_vip
FROM tickets t
JOIN buses b ON t.bus_id = b.id
WHERE t.user_id = ?
`

type GetUserTicketsRow struct {
	ID               int32          `json:"id"`
	RouteID          int32          `json:"route_id"`
	DepartureTime    time.Time      `json:"departure_time"`
	ArrivalTime      time.Time      `json:"arrival_time"`
	Capacity         int32          `json:"capacity"`
	Price            int32          `json:"price"`
	BusType          string         `json:"bus_type"`
	Corporation      sql.NullString `json:"corporation"`
	SuperCorporation sql.NullString `json:"super_corporation"`
	ServiceNumber    sql.NullString `json:"service_number"`
	IsVip            bool           `json:"is_vip"`
}

func (q *Queries) GetUserTickets(ctx context.Context, userID int32) ([]GetUserTicketsRow, error) {
	rows, err := q.db.QueryContext(ctx, getUserTickets, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	items := []GetUserTicketsRow{}
	for rows.Next() {
		var i GetUserTicketsRow
		if err := rows.Scan(
			&i.ID,
			&i.RouteID,
			&i.DepartureTime,
			&i.ArrivalTime,
			&i.Capacity,
			&i.Price,
			&i.BusType,
			&i.Corporation,
			&i.SuperCorporation,
			&i.ServiceNumber,
			&i.IsVip,
		); err != nil {
			return nil, err
		}
		items = append(items, i)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return items, nil
}

const listUserTickets = `-- name: ListUserTickets :many
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
FROM tickets t
JOIN buses b ON t.bus_id = b.id
JOIN seat_reservations sr ON t.seat_reservation_id = sr.id
JOIN bus_seats s ON sr.bus_seat_id = s.id
WHERE t.user_id = ?
ORDER BY t.reserved_at DESC
`

type ListUserTicketsRow struct {
	TicketID          int32        `json:"ticket_id"`
	BusID             int32        `json:"bus_id"`
	SeatID            int32        `json:"seat_id"`
	ReservedAt        sql.NullTime `json:"reserved_at"`
	DepartureTime     time.Time    `json:"departure_time"`
	ArrivalTime       time.Time    `json:"arrival_time"`
	Price             int32        `json:"price"`
	SeatNumber        int32        `json:"seat_number"`
	ReservationStatus string       `json:"reservation_status"`
}

func (q *Queries) ListUserTickets(ctx context.Context, userID int32) ([]ListUserTicketsRow, error) {
	rows, err := q.db.QueryContext(ctx, listUserTickets, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	items := []ListUserTicketsRow{}
	for rows.Next() {
		var i ListUserTicketsRow
		if err := rows.Scan(
			&i.TicketID,
			&i.BusID,
			&i.SeatID,
			&i.ReservedAt,
			&i.DepartureTime,
			&i.ArrivalTime,
			&i.Price,
			&i.SeatNumber,
			&i.ReservationStatus,
		); err != nil {
			return nil, err
		}
		items = append(items, i)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return items, nil
}

const purchaseTicket = `-- name: PurchaseTicket :one
INSERT INTO tickets (user_id, bus_id, seat_reservation_id, status, purchased_at)
VALUES (?, ?, ?, 'purchased', NOW())
`

type PurchaseTicketParams struct {
	UserID            int32 `json:"user_id"`
	BusID             int32 `json:"bus_id"`
	SeatReservationID int32 `json:"seat_reservation_id"`
}

type PurchaseTicketRow struct {
	ID                int32        `json:"id"`
	UserID            int32        `json:"user_id"`
	BusID             int32        `json:"bus_id"`
	SeatReservationID int32        `json:"seat_reservation_id"`
	Status            string       `json:"status"`
	PurchasedAt       sql.NullTime `json:"purchased_at"`
}

func (q *Queries) PurchaseTicket(ctx context.Context, arg PurchaseTicketParams) (PurchaseTicketRow, error) {
	result, err := q.db.ExecContext(ctx, purchaseTicket, arg.UserID, arg.BusID, arg.SeatReservationID)
	if err != nil {
		return PurchaseTicketRow{}, err
	}

	id, err := result.LastInsertId()
	if err != nil {
		return PurchaseTicketRow{}, err
	}

	row := q.db.QueryRowContext(ctx, "SELECT id, user_id, bus_id, seat_reservation_id, status, purchased_at FROM tickets WHERE id = ?", id)
	var i PurchaseTicketRow
	err = row.Scan(
		&i.ID,
		&i.UserID,
		&i.BusID,
		&i.SeatReservationID,
		&i.Status,
		&i.PurchasedAt,
	)
	return i, err
}

const reserveTicket = `-- name: ReserveTicket :one
INSERT INTO tickets (user_id, bus_id, seat_reservation_id, status, reserved_at)
VALUES (?, ?, ?, 'reserved', NOW())
`

type ReserveTicketParams struct {
	UserID            int32 `json:"user_id"`
	BusID             int32 `json:"bus_id"`
	SeatReservationID int32 `json:"seat_reservation_id"`
}

type ReserveTicketRow struct {
	ID                int32        `json:"id"`
	UserID            int32        `json:"user_id"`
	BusID             int32        `json:"bus_id"`
	SeatReservationID int32        `json:"seat_reservation_id"`
	Status            string       `json:"status"`
	ReservedAt        sql.NullTime `json:"reserved_at"`
}

func (q *Queries) ReserveTicket(ctx context.Context, arg ReserveTicketParams) (ReserveTicketRow, error) {
	result, err := q.db.ExecContext(ctx, reserveTicket, arg.UserID, arg.BusID, arg.SeatReservationID)
	if err != nil {
		return ReserveTicketRow{}, err
	}

	id, err := result.LastInsertId()
	if err != nil {
		return ReserveTicketRow{}, err
	}

	row := q.db.QueryRowContext(ctx, "SELECT id, user_id, bus_id, seat_reservation_id, status, reserved_at FROM tickets WHERE id = ?", id)
	var i ReserveTicketRow
	err = row.Scan(
		&i.ID,
		&i.UserID,
		&i.BusID,
		&i.SeatReservationID,
		&i.Status,
		&i.ReservedAt,
	)
	return i, err
}

const updateTicketStatus = `-- name: UpdateTicketStatus :exec
UPDATE tickets
SET status = ?
WHERE id = ?
`

type UpdateTicketStatusParams struct {
	ID     int32  `json:"id"`
	Status string `json:"status"`
}

func (q *Queries) UpdateTicketStatus(ctx context.Context, arg UpdateTicketStatusParams) error {
	_, err := q.db.ExecContext(ctx, updateTicketStatus, arg.Status, arg.ID)
	return err
}

const updateSeatReservationStatusByID = `-- name: UpdateSeatReservationStatusByID :exec
UPDATE seat_reservations
SET status = ?
WHERE id = ?
`

type UpdateSeatReservationStatusByIDParams struct {
	Status string `json:"status"`
	ID     int32  `json:"id"`
}

func (q *Queries) UpdateSeatReservationStatusByID(ctx context.Context, arg UpdateSeatReservationStatusByIDParams) error {
	_, err := q.db.ExecContext(ctx, updateSeatReservationStatusByID, arg.Status, arg.ID)
	return err
}
