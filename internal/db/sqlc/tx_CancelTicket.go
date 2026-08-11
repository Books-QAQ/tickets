package db

import (
	"context"
	"fmt"
)

// CancelTicketParams holds the input parameters for CancelTicketTx
type CancelTicketParams struct {
	UserID   int32 `json:"user_id"`
	TicketID int32 `json:"ticket_id"`
	SeatID   int32 `json:"seat_id"`
}

// CancelTicketTx cancels a ticket and updates the seat status within a transaction
func (store *Store) CancelTicketTx(ctx context.Context, arg CancelTicketParams) error {
	err := store.execTx(ctx, func(q *Queries) error {
		reservation, err := q.GetSeatReservationByID(ctx, arg.SeatID)
		if err != nil {
			return fmt.Errorf("failed to fetch seat reservation: %w", err)
		}

		err = q.UpdateTicketStatus(ctx, UpdateTicketStatusParams{
			ID:     arg.TicketID,
			Status: "canceled",
		})
		if err != nil {
			return fmt.Errorf("failed to update ticket status: %w", err)
		}

		err = q.UpdateSeatReservationStatusByID(ctx, UpdateSeatReservationStatusByIDParams{
			Status: "canceled",
			ID:     reservation.ID,
		})
		if err != nil {
			return fmt.Errorf("failed to update reservation status: %w", err)
		}

		err = q.UpdateBusSeatStatus(ctx, UpdateBusSeatStatusParams{
			Status: "available",
			ID:     reservation.BusSeatID,
		})
		if err != nil {
			return fmt.Errorf("failed to update bus seat status: %w", err)
		}

		return nil
	})

	return err
}
