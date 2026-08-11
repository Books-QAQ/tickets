package test

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/mysql"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	db "github.com/Books-QAQ/tickets/internal/db/sqlc"
	"github.com/Books-QAQ/tickets/internal/util"
	"github.com/stretchr/testify/require"
)

type purchaseFixture struct {
	user  db.User
	route db.Route
	bus   db.Bus
	seat  db.BusSeat
}

func TestPurchaseTicketTxConsistencySuccess(t *testing.T) {
	totalStart := time.Now()
	setupStart := time.Now()
	store, pool := newTicketTestStore(t)
	ctx := context.Background()
	t.Logf("timing setup=%s", time.Since(setupStart))

	fixtureStart := time.Now()
	fixture := createPurchaseFixture(t, ctx, store)
	t.Logf("timing fixture_prepare=%s", time.Since(fixtureStart))

	purchaseStart := time.Now()
	result, err := store.PurchaseTicketTx(ctx, db.PurchaseTicketTxParams{
		UserID: fixture.user.ID,
		BusID:  fixture.bus.ID,
		SeatID: fixture.seat.ID,
	})
	t.Logf("timing purchase_tx=%s", time.Since(purchaseStart))

	require.NoError(t, err)
	require.NotZero(t, result.TicketID)

	verifyStart := time.Now()
	ticket, err := store.GetTicketByID(ctx, result.TicketID)
	require.NoError(t, err)
	require.Equal(t, fixture.user.ID, ticket.UserID)
	require.Equal(t, fixture.bus.ID, ticket.BusID)
	require.Equal(t, "purchased", ticket.Status)

	var reservationStatus string
	var reservationSeatID int32
	err = pool.QueryRowContext(ctx, `SELECT status, bus_seat_id FROM seat_reservations WHERE id = ?`, ticket.SeatReservationID).Scan(&reservationStatus, &reservationSeatID)
	require.NoError(t, err)
	require.Equal(t, "purchased", reservationStatus)
	require.Equal(t, fixture.seat.ID, reservationSeatID)

	var ticketCount int
	err = pool.QueryRowContext(ctx, `SELECT COUNT(*) FROM tickets WHERE id = ?`, result.TicketID).Scan(&ticketCount)
	require.NoError(t, err)
	require.Equal(t, 1, ticketCount)

	var seatStatus string
	err = pool.QueryRowContext(ctx, `SELECT status FROM bus_seats WHERE id = ?`, fixture.seat.ID).Scan(&seatStatus)
	require.NoError(t, err)
	require.Equal(t, "purchased", seatStatus)
	t.Logf(
		"purchase success user=%s ticket_id=%d bus_id=%d seat_id=%d ticket_status=%s reservation_status=%s seat_status=%s",
		fixture.user.Username,
		result.TicketID,
		fixture.bus.ID,
		fixture.seat.ID,
		ticket.Status,
		reservationStatus,
		seatStatus,
	)
	t.Logf("timing verification=%s", time.Since(verifyStart))
	t.Logf("timing total=%s", time.Since(totalStart))
}

func TestPurchaseTicketTxConsistencyRollbackOnFailure(t *testing.T) {
	totalStart := time.Now()
	setupStart := time.Now()
	store, pool := newTicketTestStore(t)
	ctx := context.Background()
	t.Logf("timing setup=%s", time.Since(setupStart))

	fixtureStart := time.Now()
	fixture := createPurchaseFixture(t, ctx, store)
	t.Logf("timing fixture_prepare=%s", time.Since(fixtureStart))

	preconditionStart := time.Now()
	seats, err := store.GetAvailableSeatsForBus(ctx, db.GetAvailableSeatsForBusParams{
		RouteID: fixture.route.ID,
		BusID:   fixture.bus.ID,
	})
	require.NoError(t, err)
	require.Len(t, seats, 1)

	_, err = pool.ExecContext(ctx, `UPDATE bus_seats SET status = 'reserved' WHERE id = ?`, fixture.seat.ID)
	require.NoError(t, err)
	t.Logf("timing precondition=%s", time.Since(preconditionStart))

	purchaseStart := time.Now()
	result, err := store.PurchaseTicketTx(ctx, db.PurchaseTicketTxParams{
		UserID: fixture.user.ID,
		BusID:  fixture.bus.ID,
		SeatID: fixture.seat.ID,
	})
	purchaseErr := err
	t.Logf("timing purchase_tx=%s", time.Since(purchaseStart))

	require.Error(t, purchaseErr)
	require.Zero(t, result.TicketID)

	verifyStart := time.Now()
	var ticketCount int
	err = pool.QueryRowContext(ctx, `SELECT COUNT(*) FROM tickets WHERE user_id = ? AND bus_id = ?`, fixture.user.ID, fixture.bus.ID).Scan(&ticketCount)
	require.NoError(t, err)
	require.Zero(t, ticketCount)

	var reservationCount int
	err = pool.QueryRowContext(ctx, `SELECT COUNT(*) FROM seat_reservations WHERE user_id = ? AND bus_id = ?`, fixture.user.ID, fixture.bus.ID).Scan(&reservationCount)
	require.NoError(t, err)
	require.Zero(t, reservationCount)
	t.Logf(
		"purchase rollback user=%s bus_id=%d seat_id=%d err=%v tickets_after_rollback=%d reservations_after_rollback=%d",
		fixture.user.Username,
		fixture.bus.ID,
		fixture.seat.ID,
		purchaseErr,
		ticketCount,
		reservationCount,
	)
	t.Logf("timing verification=%s", time.Since(verifyStart))
	t.Logf("timing total=%s", time.Since(totalStart))
}

func newTicketTestStore(t *testing.T) (*db.Store, *sql.DB) {
	t.Helper()

	root := projectRoot(t)
	config, err := util.LoadConfig(root)
	require.NoError(t, err)

	dsn := fmt.Sprintf("%s:%s@tcp(%s:%s)/%s?parseTime=true&multiStatements=true",
		config.DBUSERNAME,
		config.DBPASSWORD,
		config.DBHOST,
		config.DBPORT,
		config.DBDATABASE,
	)

	pool, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Skipf("skip purchase transaction test: cannot create db pool: %v", err)
	}

	if err := pool.PingContext(context.Background()); err != nil {
		pool.Close()
		t.Skipf("skip purchase transaction test: mysql unavailable: %v", err)
	}

	runMigrations(t, config, root)
	truncateTicketTables(t, pool)

	t.Cleanup(func() {
		truncateTicketTables(t, pool)
		pool.Close()
	})

	return db.NewStore(pool), pool
}

func createPurchaseFixture(t *testing.T, ctx context.Context, store *db.Store) purchaseFixture {
	t.Helper()

	hashedPassword, err := util.HashPassword("secret123")
	require.NoError(t, err)

	user, err := store.CreateUser(ctx, db.CreateUserParams{
		Username:       "buyer_" + util.RandomString(6),
		HashedPassword: hashedPassword,
		FullName:       "Purchase Tester",
	})
	require.NoError(t, err)

	originCity, err := store.CreateCity(ctx, "Origin-"+util.RandomString(6))
	require.NoError(t, err)

	destinationCity, err := store.CreateCity(ctx, "Destination-"+util.RandomString(6))
	require.NoError(t, err)

	originTerminal, err := store.CreateTerminal(ctx, db.CreateTerminalParams{
		CityID: originCity.ID,
		Name:   "Origin-Terminal-" + util.RandomString(4),
	})
	require.NoError(t, err)

	destinationTerminal, err := store.CreateTerminal(ctx, db.CreateTerminalParams{
		CityID: destinationCity.ID,
		Name:   "Destination-Terminal-" + util.RandomString(4),
	})
	require.NoError(t, err)

	route, err := store.CreateRoute(ctx, db.CreateRouteParams{
		OriginTerminalID:      originTerminal.ID,
		DestinationTerminalID: destinationTerminal.ID,
		Duration:              120,
		Distance:              120,
	})
	require.NoError(t, err)

	bus, err := store.CreateBus(ctx, db.CreateBusParams{
		RouteID:       route.ID,
		DepartureTime: time.Now().Add(24 * time.Hour),
		ArrivalTime:   time.Now().Add(26 * time.Hour),
		Capacity:      40,
		Price:         88,
		BusType:       "standard",
		Corporation: sql.NullString{
			String: "Codex Lines",
			Valid:  true,
		},
		ServiceNumber: sql.NullString{
			String: "TM1001",
			Valid:  true,
		},
	})
	require.NoError(t, err)

	seat, err := store.CreateBusSeat(ctx, db.CreateBusSeatParams{
		BusID:      bus.ID,
		SeatNumber: 1,
	})
	require.NoError(t, err)

	return purchaseFixture{
		user:  user,
		route: route,
		bus:   bus,
		seat:  seat,
	}
}

func runMigrations(t *testing.T, config util.Config, root string) {
	t.Helper()

	migrationURL := "file://" + filepath.ToSlash(filepath.Join(root, "internal", "db", "migration"))
	dsn := fmt.Sprintf(
		"mysql://%s:%s@tcp(%s:%s)/%s?multiStatements=true",
		url.QueryEscape(config.DBUSERNAME),
		url.QueryEscape(config.DBPASSWORD),
		config.DBHOST,
		config.DBPORT,
		config.DBDATABASE,
	)
	m, err := migrate.New(migrationURL, dsn)
	require.NoError(t, err)
	defer func() {
		sourceErr, dbErr := m.Close()
		require.NoError(t, sourceErr)
		require.NoError(t, dbErr)
	}()

	err = m.Up()
	require.True(t, err == nil || err == migrate.ErrNoChange, "unexpected migration error: %v", err)
}

func truncateTicketTables(t *testing.T, pool *sql.DB) {
	t.Helper()

	_, err := pool.ExecContext(context.Background(), `
		SET FOREIGN_KEY_CHECKS = 0;
		TRUNCATE TABLE tickets;
		TRUNCATE TABLE seat_reservations;
		TRUNCATE TABLE bus_seats;
		TRUNCATE TABLE buses;
		TRUNCATE TABLE routes;
		TRUNCATE TABLE terminals;
		TRUNCATE TABLE cities;
		TRUNCATE TABLE sessions;
		TRUNCATE TABLE penalties;
		TRUNCATE TABLE users;
		SET FOREIGN_KEY_CHECKS = 1;
	`)
	require.NoError(t, err)
}

func projectRoot(t *testing.T) string {
	t.Helper()

	_, filename, _, ok := runtime.Caller(0)
	require.True(t, ok)

	return filepath.Dir(filepath.Dir(filename))
}
