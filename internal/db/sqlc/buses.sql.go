package db

import (
	"context"
	"database/sql"
	"time"
)

func scanBus(scanner rowScanner) (Bus, error) {
	var i Bus
	err := scanner.Scan(
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
	)
	return i, err
}

const checkBusRouteAssociation = `-- name: CheckBusRouteAssociation :one
SELECT
    b.id AS bus_id,
    r.id AS route_id
FROM buses b
JOIN routes r ON b.route_id = r.id
WHERE b.id = ?
  AND r.id = ?
LIMIT 1
`

type CheckBusRouteAssociationParams struct {
	ID   int32 `json:"id"`
	ID_2 int32 `json:"id_2"`
}

type CheckBusRouteAssociationRow struct {
	BusID   int32 `json:"bus_id"`
	RouteID int32 `json:"route_id"`
}

func (q *Queries) CheckBusRouteAssociation(ctx context.Context, arg CheckBusRouteAssociationParams) (CheckBusRouteAssociationRow, error) {
	row := q.db.QueryRowContext(ctx, checkBusRouteAssociation, arg.ID_2, arg.ID)
	var i CheckBusRouteAssociationRow
	err := row.Scan(&i.BusID, &i.RouteID)
	return i, err
}

const checkSeatAvailability = `-- name: CheckSeatAvailability :one
SELECT
    s.id AS seat_id,
    s.status
FROM bus_seats s
LEFT JOIN seat_reservations sr ON s.id = sr.bus_seat_id AND sr.status IN ('reserved', 'purchased')
WHERE s.id = ?
  AND s.bus_id = ?
  AND s.status = 'available'
  AND sr.id IS NULL
`

type CheckSeatAvailabilityParams struct {
	ID    int32 `json:"id"`
	BusID int32 `json:"bus_id"`
}

type CheckSeatAvailabilityRow struct {
	SeatID int32  `json:"seat_id"`
	Status string `json:"status"`
}

func (q *Queries) CheckSeatAvailability(ctx context.Context, arg CheckSeatAvailabilityParams) (CheckSeatAvailabilityRow, error) {
	row := q.db.QueryRowContext(ctx, checkSeatAvailability, arg.ID, arg.BusID)
	var i CheckSeatAvailabilityRow
	err := row.Scan(&i.SeatID, &i.Status)
	return i, err
}

const createBus = `-- name: CreateBus :one
INSERT INTO buses (route_id, departure_time, arrival_time, capacity, price, bus_type, corporation, super_corporation, service_number, is_vip)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
`

type CreateBusParams struct {
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

func (q *Queries) CreateBus(ctx context.Context, arg CreateBusParams) (Bus, error) {
	result, err := q.db.ExecContext(ctx, createBus,
		arg.RouteID,
		arg.DepartureTime,
		arg.ArrivalTime,
		arg.Capacity,
		arg.Price,
		arg.BusType,
		arg.Corporation,
		arg.SuperCorporation,
		arg.ServiceNumber,
		arg.IsVip,
	)
	if err != nil {
		return Bus{}, err
	}

	id, err := result.LastInsertId()
	if err != nil {
		return Bus{}, err
	}

	return q.GetBusByID(ctx, int32(id))
}

const createBusSeat = `-- name: CreateBusSeat :one
INSERT INTO bus_seats (bus_id, seat_number, status)
VALUES (?, ?, 'available')
`

type CreateBusSeatParams struct {
	BusID      int32 `json:"bus_id"`
	SeatNumber int32 `json:"seat_number"`
}

func (q *Queries) CreateBusSeat(ctx context.Context, arg CreateBusSeatParams) (BusSeat, error) {
	result, err := q.db.ExecContext(ctx, createBusSeat, arg.BusID, arg.SeatNumber)
	if err != nil {
		return BusSeat{}, err
	}

	id, err := result.LastInsertId()
	if err != nil {
		return BusSeat{}, err
	}

	row := q.db.QueryRowContext(ctx, "SELECT id, bus_id, seat_number, status FROM bus_seats WHERE id = ?", id)
	var i BusSeat
	err = row.Scan(&i.ID, &i.BusID, &i.SeatNumber, &i.Status)
	return i, err
}

const getAvailableSeatsForBus = `-- name: GetAvailableSeatsForBus :many
SELECT
    bs.id AS seat_id,
    bs.seat_number,
    bs.status
FROM bus_seats bs
JOIN buses b ON bs.bus_id = b.id
WHERE b.route_id = ?
  AND bs.bus_id = ?
  AND bs.status = 'available'
ORDER BY bs.seat_number
`

type GetAvailableSeatsForBusParams struct {
	RouteID int32 `json:"route_id"`
	BusID   int32 `json:"bus_id"`
}

type GetAvailableSeatsForBusRow struct {
	SeatID     int32  `json:"seat_id"`
	SeatNumber int32  `json:"seat_number"`
	Status     string `json:"status"`
}

func (q *Queries) GetAvailableSeatsForBus(ctx context.Context, arg GetAvailableSeatsForBusParams) ([]GetAvailableSeatsForBusRow, error) {
	rows, err := q.db.QueryContext(ctx, getAvailableSeatsForBus, arg.RouteID, arg.BusID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	items := []GetAvailableSeatsForBusRow{}
	for rows.Next() {
		var i GetAvailableSeatsForBusRow
		if err := rows.Scan(&i.SeatID, &i.SeatNumber, &i.Status); err != nil {
			return nil, err
		}
		items = append(items, i)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return items, nil
}

const getBusByID = `-- name: GetBusByID :one
SELECT id, route_id, departure_time, arrival_time, capacity, price, bus_type, corporation, super_corporation, service_number, is_vip
FROM buses
WHERE id = ?
`

func (q *Queries) GetBusByID(ctx context.Context, id int32) (Bus, error) {
	row := q.db.QueryRowContext(ctx, getBusByID, id)
	return scanBus(row)
}

const getBusSeats = `-- name: GetBusSeats :many
SELECT id, bus_id, seat_number, status
FROM bus_seats
WHERE bus_id = ?
ORDER BY seat_number
`

func (q *Queries) GetBusSeats(ctx context.Context, busID int32) ([]BusSeat, error) {
	rows, err := q.db.QueryContext(ctx, getBusSeats, busID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	items := []BusSeat{}
	for rows.Next() {
		var i BusSeat
		if err := rows.Scan(&i.ID, &i.BusID, &i.SeatNumber, &i.Status); err != nil {
			return nil, err
		}
		items = append(items, i)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return items, nil
}

const getSeatByID = `-- name: GetSeatByID :one
SELECT
    bs.id AS seat_id,
    bs.bus_id,
    bs.seat_number,
    bs.status AS seat_status,
    sr.status AS reservation_status,
    sr.user_id
FROM bus_seats bs
LEFT JOIN seat_reservations sr ON bs.id = sr.bus_seat_id
WHERE bs.id = ?
  AND bs.bus_id = ?
LIMIT 1
`

type GetSeatByIDParams struct {
	ID    int32 `json:"id"`
	BusID int32 `json:"bus_id"`
}

type GetSeatByIDRow struct {
	SeatID            int32          `json:"seat_id"`
	BusID             int32          `json:"bus_id"`
	SeatNumber        int32          `json:"seat_number"`
	SeatStatus        string         `json:"seat_status"`
	ReservationStatus sql.NullString `json:"reservation_status"`
	UserID            sql.NullInt64  `json:"user_id"`
}

func (q *Queries) GetSeatByID(ctx context.Context, arg GetSeatByIDParams) (GetSeatByIDRow, error) {
	row := q.db.QueryRowContext(ctx, getSeatByID, arg.ID, arg.BusID)
	var i GetSeatByIDRow
	err := row.Scan(
		&i.SeatID,
		&i.BusID,
		&i.SeatNumber,
		&i.SeatStatus,
		&i.ReservationStatus,
		&i.UserID,
	)
	return i, err
}

const searchBuses = `-- name: SearchBuses :many
SELECT b.id, b.route_id, b.departure_time, b.arrival_time, b.capacity, b.price, b.bus_type, b.corporation, b.super_corporation, b.service_number, b.is_vip
FROM buses b
JOIN routes r ON b.route_id = r.id
WHERE r.origin_terminal_id = ? AND r.destination_terminal_id = ? AND b.departure_time >= ?
`

type SearchBusesParams struct {
	OriginTerminalID      int32     `json:"origin_terminal_id"`
	DestinationTerminalID int32     `json:"destination_terminal_id"`
	DepartureTime         time.Time `json:"departure_time"`
}

func (q *Queries) SearchBuses(ctx context.Context, arg SearchBusesParams) ([]Bus, error) {
	rows, err := q.db.QueryContext(ctx, searchBuses, arg.OriginTerminalID, arg.DestinationTerminalID, arg.DepartureTime)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	items := []Bus{}
	for rows.Next() {
		item, err := scanBus(rows)
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

const searchBusesByCities = `-- name: SearchBusesByCities :many
SELECT b.id, b.route_id, b.departure_time, b.arrival_time, b.capacity, b.price, b.bus_type, b.corporation, b.super_corporation, b.service_number, b.is_vip
FROM buses b
JOIN routes r ON b.route_id = r.id
JOIN terminals t_origin ON r.origin_terminal_id = t_origin.id
JOIN terminals t_destination ON r.destination_terminal_id = t_destination.id
WHERE t_origin.city_id = ? AND t_destination.city_id = ? AND b.departure_time >= ?
`

type SearchBusesByCitiesParams struct {
	CityID        int32     `json:"city_id"`
	CityID_2      int32     `json:"city_id_2"`
	DepartureTime time.Time `json:"departure_time"`
}

func (q *Queries) SearchBusesByCities(ctx context.Context, arg SearchBusesByCitiesParams) ([]Bus, error) {
	rows, err := q.db.QueryContext(ctx, searchBusesByCities, arg.CityID, arg.CityID_2, arg.DepartureTime)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	items := []Bus{}
	for rows.Next() {
		item, err := scanBus(rows)
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

const updateSeatReservationStatus = `-- name: UpdateSeatReservationStatus :exec
UPDATE seat_reservations
SET status = ?
WHERE bus_seat_id = ?
  AND user_id = ?
`

type UpdateSeatReservationStatusParams struct {
	Status    string `json:"status"`
	BusSeatID int32  `json:"bus_seat_id"`
	UserID    int32  `json:"user_id"`
}

func (q *Queries) UpdateSeatReservationStatus(ctx context.Context, arg UpdateSeatReservationStatusParams) error {
	_, err := q.db.ExecContext(ctx, updateSeatReservationStatus, arg.Status, arg.BusSeatID, arg.UserID)
	return err
}

const updateBusSeatStatus = `-- name: UpdateBusSeatStatus :exec
UPDATE bus_seats
SET status = ?
WHERE id = ?
`

type UpdateBusSeatStatusParams struct {
	Status string `json:"status"`
	ID     int32  `json:"id"`
}

func (q *Queries) UpdateBusSeatStatus(ctx context.Context, arg UpdateBusSeatStatusParams) error {
	_, err := q.db.ExecContext(ctx, updateBusSeatStatus, arg.Status, arg.ID)
	return err
}

const claimAvailableBusSeat = `
UPDATE bus_seats
SET status = ?
WHERE id = ?
  AND status = 'available'
`

func (q *Queries) ClaimAvailableBusSeat(ctx context.Context, id int32, status string) (bool, error) {
	result, err := q.db.ExecContext(ctx, claimAvailableBusSeat, status, id)
	if err != nil {
		return false, err
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}

	return affected == 1, nil
}

const updateSeatStatusAfterTrip = `-- name: UpdateSeatStatusAfterTrip :exec
UPDATE bus_seats
SET status = 'available'
WHERE bus_id = ?
  AND status = 'purchased'
`

func (q *Queries) UpdateSeatStatusAfterTrip(ctx context.Context, busID int32) error {
	_, err := q.db.ExecContext(ctx, updateSeatStatusAfterTrip, busID)
	return err
}
