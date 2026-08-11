package db

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

type PurchaseTicketTxParams struct {
	UserID int32 `json:"user_id"`
	BusID  int32 `json:"bus_id"`
	SeatID int32 `json:"seat_id"`
}

type PurchaseTicketTxResult struct {
	TicketID   int32     `json:"ticket_id"`
	BusID      int32     `json:"bus_id"`
	SeatID     int32     `json:"seat_id"`
	ReservedAt time.Time `json:"reserved_at"`
}

// PurchaseTicketTx handles purchasing a ticket in a transaction
func (store *Store) PurchaseTicketTx(ctx context.Context, arg PurchaseTicketTxParams) (PurchaseTicketTxResult, error) {
	var result PurchaseTicketTxResult

	err := store.execTx(ctx, func(q *Queries) error {
		claimed, err := q.ClaimAvailableBusSeat(ctx, arg.SeatID, "purchased")
		if err != nil {
			return err
		}
		if !claimed {
			return fmt.Errorf("seat is no longer available")
		}

		reservation, err := q.CreateSeatReservation(ctx, CreateSeatReservationParams{
			BusID:       arg.BusID,
			BusSeatID:   arg.SeatID,
			UserID:      arg.UserID,
			Status:      "purchased",
			PurchasedAt: sql.NullTime{Time: time.Now(), Valid: true},
		})
		if err != nil {
			return err
		}

		ticket, err := q.PurchaseTicket(ctx, PurchaseTicketParams{
			UserID:            arg.UserID,
			BusID:             arg.BusID,
			SeatReservationID: reservation.ID,
		})
		if err != nil {
			return err
		}

		result = PurchaseTicketTxResult{
			TicketID:   ticket.ID,
			BusID:      arg.BusID,
			SeatID:     arg.SeatID,
			ReservedAt: ticket.PurchasedAt.Time,
		}

		return nil
	})

	if err != nil {
		return result, fmt.Errorf("failed to purchase ticket: %v", err)
	}

	return result, nil
}
