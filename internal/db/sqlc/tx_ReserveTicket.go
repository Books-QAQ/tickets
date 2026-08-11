package db

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

type reserveSeatResponse struct {
	TicketID   int32     `json:"ticket_id"`
	BusID      int32     `json:"bus_id"`
	SeatID     int32     `json:"seat_id"`
	ReservedAt time.Time `json:"reserved_at"`
}

type ReserveTicketTxParams struct {
	UserID int32 `json:"user_id"`
	BusID  int32 `json:"bus_id"`
	SeatID int32 `json:"seat_id"`
}

func (store *Store) ReserveTicketTx(ctx context.Context, arg ReserveTicketTxParams) (reserveSeatResponse, error) {
	var result reserveSeatResponse

	err := store.execTx(ctx, func(q *Queries) error {
		claimed, err := q.ClaimAvailableBusSeat(ctx, arg.SeatID, "reserved")
		if err != nil {
			return fmt.Errorf("failed to claim seat: %v", err)
		}
		if !claimed {
			return fmt.Errorf("seat is no longer available")
		}

		reservation, err := q.CreateSeatReservation(ctx, CreateSeatReservationParams{
			BusID:       arg.BusID,
			BusSeatID:   arg.SeatID,
			UserID:      arg.UserID,
			Status:      "reserved",
			PurchasedAt: sql.NullTime{},
		})
		if err != nil {
			return fmt.Errorf("failed to create seat reservation: %v", err)
		}

		ticket, err := q.ReserveTicket(ctx, ReserveTicketParams{
			UserID:            arg.UserID,
			BusID:             arg.BusID,
			SeatReservationID: reservation.ID,
		})
		if err != nil {
			return fmt.Errorf("failed to reserve ticket: %v", err)
		}

		result = reserveSeatResponse{
			TicketID:   ticket.ID,
			BusID:      ticket.BusID,
			SeatID:     arg.SeatID,
			ReservedAt: ticket.ReservedAt.Time,
		}

		return nil
	})

	return result, err
}
