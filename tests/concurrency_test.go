package test

import (
	"context"
	"sync"
	"testing"
	"time"

	db "github.com/Books-QAQ/tickets/internal/db/sqlc"
	"github.com/Books-QAQ/tickets/tests/testsupport"
	"github.com/stretchr/testify/require"
)

func TestPurchaseTicketTxConcurrentDistinctSeatPurchases(t *testing.T) {
	totalStart := time.Now()
	store, pool := testsupport.NewTicketUserListStore(t)
	ctx := context.Background()

	usersStart := time.Now()
	users := testsupport.EnsureCSVUsers(t, ctx, store, 1000)
	require.GreaterOrEqual(t, len(users), 2, "need at least 2 users in scripts/users.example.csv")
	t.Logf("timing users_prepare=%s user_count=%d", time.Since(usersStart), len(users))

	fixtureStart := time.Now()
	fixture := testsupport.CreateBusPurchaseFixture(t, ctx, store, pool, len(users))
	t.Logf("timing fixture_prepare=%s bus_id=%d seat_count=%d", time.Since(fixtureStart), fixture.Bus.ID, len(fixture.Seats))

	start := make(chan struct{})
	results := make(chan testsupport.PurchaseAttempt, len(users))
	var wg sync.WaitGroup

	purchaseStart := time.Now()
	for i, user := range users {
		user := user
		seat := fixture.Seats[i]

		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			result, err := store.PurchaseTicketTx(ctx, db.PurchaseTicketTxParams{
				UserID: user.ID,
				BusID:  fixture.Bus.ID,
				SeatID: seat.ID,
			})
			results <- testsupport.PurchaseAttempt{User: user, Result: result, Err: err}
		}()
	}

	close(start)
	wg.Wait()
	close(results)
	t.Logf("timing concurrent_purchase=%s attempts=%d", time.Since(purchaseStart), len(users))

	seenTicketIDs := make(map[int32]struct{}, len(users))
	seenSeatIDs := make(map[int32]struct{}, len(users))

	verifyStart := time.Now()
	for attempt := range results {
		require.NoError(t, attempt.Err)
		require.NotZero(t, attempt.Result.TicketID)
		require.Equal(t, fixture.Bus.ID, attempt.Result.BusID)
		snapshot, snapshotErr := testsupport.FetchTicketSnapshot(ctx, pool, attempt.Result.TicketID)
		require.NoError(t, snapshotErr)
		t.Logf(
			"user=%s success ticket_id=%d bus_id=%d seat_id=%d ticket_status=%s reservation_status=%s seat_status=%s",
			attempt.User.Username,
			snapshot.TicketID,
			snapshot.BusID,
			snapshot.SeatID,
			snapshot.TicketStatus,
			snapshot.ReservationStatus,
			snapshot.SeatStatus,
		)

		_, duplicateTicket := seenTicketIDs[attempt.Result.TicketID]
		require.False(t, duplicateTicket, "duplicate ticket id returned")
		seenTicketIDs[attempt.Result.TicketID] = struct{}{}

		_, duplicateSeat := seenSeatIDs[attempt.Result.SeatID]
		require.False(t, duplicateSeat, "duplicate seat id returned")
		seenSeatIDs[attempt.Result.SeatID] = struct{}{}
	}

	require.Len(t, seenTicketIDs, len(users))
	require.Len(t, seenSeatIDs, len(users))

	var ticketCount int
	err := pool.QueryRowContext(ctx, `SELECT COUNT(*) FROM tickets WHERE bus_id = ?`, fixture.Bus.ID).Scan(&ticketCount)
	require.NoError(t, err)
	require.Equal(t, len(users), ticketCount)

	var reservationCount int
	err = pool.QueryRowContext(ctx, `SELECT COUNT(*) FROM seat_reservations WHERE bus_id = ?`, fixture.Bus.ID).Scan(&reservationCount)
	require.NoError(t, err)
	require.Equal(t, len(users), reservationCount)

	var purchasedSeatCount int
	err = pool.QueryRowContext(ctx, `SELECT COUNT(*) FROM bus_seats WHERE bus_id = ? AND status = 'purchased'`, fixture.Bus.ID).Scan(&purchasedSeatCount)
	require.NoError(t, err)
	require.Equal(t, len(users), purchasedSeatCount)
	t.Logf("timing verification=%s", time.Since(verifyStart))
	t.Logf("concurrency summary bus_id=%d users=%d tickets=%d reservations=%d purchased_seats=%d", fixture.Bus.ID, len(users), ticketCount, reservationCount, purchasedSeatCount)
	t.Logf("timing total=%s", time.Since(totalStart))
}
