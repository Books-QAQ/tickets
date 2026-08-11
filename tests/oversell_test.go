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

type oversellPurchaseAttempt struct {
	User         db.User
	Result       db.PurchaseTicketTxResult
	SeatID       int32
	Success      bool
	LastErr      error
	AttemptCount int
}

func TestPurchaseTicketTxPreventsOversell(t *testing.T) {
	testCases := []struct {
		name      string
		userLimit int
		seatCount int
	}{
		{name: "SingleTicket", userLimit: 20, seatCount: 1},
		{name: "MultiTicket", userLimit: 20, seatCount: 5},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			runOversellScenario(t, tc.userLimit, tc.seatCount)
		})
	}
}

func runOversellScenario(t *testing.T, userLimit, seatCount int) {
	t.Helper()

	totalStart := time.Now()
	store, pool := testsupport.NewTicketUserListStore(t)
	ctx := context.Background()

	usersStart := time.Now()
	users := testsupport.EnsureCSVUsers(t, ctx, store, userLimit)
	require.GreaterOrEqual(t, len(users), seatCount+1, "need more users than seats to test oversell")
	t.Logf("timing users_prepare=%s user_count=%d", time.Since(usersStart), len(users))

	fixtureStart := time.Now()
	fixture := testsupport.CreateBusPurchaseFixture(t, ctx, store, pool, seatCount)
	t.Logf("timing fixture_prepare=%s bus_id=%d seat_count=%d", time.Since(fixtureStart), fixture.Bus.ID, len(fixture.Seats))

	start := make(chan struct{})
	results := make(chan oversellPurchaseAttempt, len(users))
	var wg sync.WaitGroup

	purchaseStart := time.Now()
	for _, user := range users {
		user := user
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start

			attempt := oversellPurchaseAttempt{User: user}
			for _, seat := range fixture.Seats {
				attempt.AttemptCount++
				result, err := store.PurchaseTicketTx(ctx, db.PurchaseTicketTxParams{
					UserID: user.ID,
					BusID:  fixture.Bus.ID,
					SeatID: seat.ID,
				})
				if err == nil {
					attempt.Success = true
					attempt.Result = result
					attempt.SeatID = seat.ID
					results <- attempt
					return
				}

				attempt.LastErr = err
			}

			results <- attempt
		}()
	}

	close(start)
	wg.Wait()
	close(results)
	t.Logf("timing concurrent_purchase=%s attempts=%d", time.Since(purchaseStart), len(users))

	successes := 0
	failures := 0
	winnersBySeat := make(map[int32]string, seatCount)

	verifyStart := time.Now()
	for attempt := range results {
		if attempt.Success {
			successes++
			require.NotZero(t, attempt.Result.TicketID)
			require.Equal(t, attempt.SeatID, attempt.Result.SeatID)
			snapshot, snapshotErr := testsupport.FetchTicketSnapshot(ctx, pool, attempt.Result.TicketID)
			require.NoError(t, snapshotErr)
			winnersBySeat[snapshot.SeatID] = attempt.User.Username
			t.Logf(
				"user=%s success attempts=%d ticket_id=%d bus_id=%d seat_id=%d ticket_status=%s reservation_status=%s seat_status=%s",
				attempt.User.Username,
				attempt.AttemptCount,
				snapshot.TicketID,
				snapshot.BusID,
				snapshot.SeatID,
				snapshot.TicketStatus,
				snapshot.ReservationStatus,
				snapshot.SeatStatus,
			)
			continue
		}

		failures++
		require.Error(t, attempt.LastErr)
		require.Contains(t, attempt.LastErr.Error(), "seat is no longer available")
		t.Logf(
			"user=%s failed attempts=%d err=%v",
			attempt.User.Username,
			attempt.AttemptCount,
			attempt.LastErr,
		)
	}

	require.Equal(t, seatCount, successes, "successful purchases should match available seats")
	require.Equal(t, len(users)-seatCount, failures)
	require.Len(t, winnersBySeat, seatCount, "each seat should have exactly one winner")

	var ticketCount int
	err := pool.QueryRowContext(ctx, `SELECT COUNT(*) FROM tickets WHERE bus_id = ?`, fixture.Bus.ID).Scan(&ticketCount)
	require.NoError(t, err)
	require.Equal(t, seatCount, ticketCount)

	var reservationCount int
	err = pool.QueryRowContext(ctx, `SELECT COUNT(*) FROM seat_reservations WHERE bus_id = ?`, fixture.Bus.ID).Scan(&reservationCount)
	require.NoError(t, err)
	require.Equal(t, seatCount, reservationCount)

	var purchasedSeatCount int
	err = pool.QueryRowContext(ctx, `SELECT COUNT(*) FROM bus_seats WHERE bus_id = ? AND status = 'purchased'`, fixture.Bus.ID).Scan(&purchasedSeatCount)
	require.NoError(t, err)
	require.Equal(t, seatCount, purchasedSeatCount)

	for _, seat := range fixture.Seats {
		var seatStatus string
		err = pool.QueryRowContext(ctx, `SELECT status FROM bus_seats WHERE id = ?`, seat.ID).Scan(&seatStatus)
		require.NoError(t, err)
		require.Equal(t, "purchased", seatStatus)
	}

	t.Logf("timing verification=%s", time.Since(verifyStart))
	t.Logf("oversell summary bus_id=%d seat_count=%d total_success=%d total_failure=%d winners=%v", fixture.Bus.ID, seatCount, successes, failures, winnersBySeat)
	t.Logf("timing total=%s", time.Since(totalStart))
}
