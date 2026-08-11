package testsupport

import (
	"context"
	"database/sql"
	"encoding/csv"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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

type CSVUserSeed struct {
	Username string
	Password string
	FullName string
}

type BusPurchaseFixture struct {
	Route               db.Route
	Bus                 db.Bus
	Seats               []db.BusSeat
	OriginCityName      string
	DestinationCityName string
}

type PurchaseAttempt struct {
	User   db.User
	Result db.PurchaseTicketTxResult
	Err    error
}

type TicketSnapshot struct {
	TicketID           int32
	Username           string
	BusID              int32
	SeatID             int32
	TicketStatus       string
	ReservationStatus  string
	SeatStatus         string
	SeatReservationID  int32
}

func NewTicketUserListStore(t *testing.T) (*db.Store, *sql.DB) {
	t.Helper()

	root := ProjectRoot(t)
	config, err := util.LoadConfig(root)
	require.NoError(t, err)

	dsn := BuildTestDSN(config)
	pool, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Skipf("skip user list purchase test: cannot create db pool: %v", err)
	}

	// Keep the test stable under higher concurrency instead of opening an
	// unbounded number of client connections against the local MySQL proxy.
	pool.SetMaxOpenConns(128)
	pool.SetMaxIdleConns(32)
	pool.SetConnMaxLifetime(5 * time.Minute)
	pool.SetConnMaxIdleTime(2 * time.Minute)

	if err := pool.PingContext(context.Background()); err != nil {
		pool.Close()
		t.Skipf("skip user list purchase test: mysql unavailable: %v", err)
	}

	RunMigrations(t, config, root)

	t.Cleanup(func() {
		pool.Close()
	})

	return db.NewStore(pool), pool
}

func LoadCSVUserSeeds(t *testing.T) []CSVUserSeed {
	t.Helper()

	csvPath := filepath.Join(ProjectRoot(t), "scripts", "users.example.csv")
	file, err := os.Open(csvPath)
	require.NoError(t, err)
	defer file.Close()

	reader := csv.NewReader(file)
	records, err := reader.ReadAll()
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(records), 2, "CSV must include a header and at least one user")

	seeds := make([]CSVUserSeed, 0, len(records)-1)
	for _, record := range records[1:] {
		if len(record) < 3 {
			continue
		}

		username := strings.TrimSpace(record[0])
		password := strings.TrimSpace(record[1])
		fullName := strings.TrimSpace(record[2])
		if username == "" || password == "" || fullName == "" {
			continue
		}

		seeds = append(seeds, CSVUserSeed{
			Username: username,
			Password: password,
			FullName: fullName,
		})
	}

	require.NotEmpty(t, seeds, "no valid users found in scripts/users.example.csv")
	return seeds
}

func EnsureCSVUsers(t *testing.T, ctx context.Context, store *db.Store, limit int) []db.User {
	t.Helper()

	seeds := LoadCSVUserSeeds(t)
	if limit > 0 && len(seeds) > limit {
		seeds = seeds[:limit]
	}

	users := make([]db.User, 0, len(seeds))
	for _, seed := range seeds {
		user, err := store.GetUser(ctx, seed.Username)
		if err == nil {
			users = append(users, user)
			continue
		}

		require.True(t, errors.Is(err, sql.ErrNoRows), "unexpected user lookup error: %v", err)

		hashedPassword, hashErr := util.HashPassword(seed.Password)
		require.NoError(t, hashErr)

		user, err = store.CreateUser(ctx, db.CreateUserParams{
			Username:       seed.Username,
			HashedPassword: hashedPassword,
			FullName:       seed.FullName,
		})
		require.NoError(t, err)
		users = append(users, user)
	}

	return users
}

func CreateBusPurchaseFixture(t *testing.T, ctx context.Context, store *db.Store, pool *sql.DB, seatCount int) BusPurchaseFixture {
	t.Helper()

	suffix := util.RandomString(8)
	originCityName := "TestOrigin" + suffix
	destinationCityName := "TestDestination" + suffix

	originCity, err := store.CreateCity(ctx, originCityName)
	require.NoError(t, err)

	destinationCity, err := store.CreateCity(ctx, destinationCityName)
	require.NoError(t, err)

	t.Cleanup(func() {
		_, cleanupErr := pool.ExecContext(context.Background(), `DELETE FROM cities WHERE name IN (?, ?)`, originCityName, destinationCityName)
		require.NoError(t, cleanupErr)
	})

	originTerminal, err := store.CreateTerminal(ctx, db.CreateTerminalParams{
		CityID: originCity.ID,
		Name:   "OriginTerminal" + suffix,
	})
	require.NoError(t, err)

	destinationTerminal, err := store.CreateTerminal(ctx, db.CreateTerminalParams{
		CityID: destinationCity.ID,
		Name:   "DestinationTerminal" + suffix,
	})
	require.NoError(t, err)

	route, err := store.CreateRoute(ctx, db.CreateRouteParams{
		OriginTerminalID:      originTerminal.ID,
		DestinationTerminalID: destinationTerminal.ID,
		Duration:              90,
		Distance:              100,
	})
	require.NoError(t, err)

	bus, err := store.CreateBus(ctx, db.CreateBusParams{
		RouteID:       route.ID,
		DepartureTime: time.Now().Add(6 * time.Hour),
		ArrivalTime:   time.Now().Add(8 * time.Hour),
		Capacity:      int32(seatCount),
		Price:         88,
		BusType:       "standard",
		Corporation: sql.NullString{
			String: "Codex Express",
			Valid:  true,
		},
		ServiceNumber: sql.NullString{
			String: "TEST-" + suffix,
			Valid:  true,
		},
	})
	require.NoError(t, err)

	seats := make([]db.BusSeat, 0, seatCount)
	for i := 1; i <= seatCount; i++ {
		seat, seatErr := store.CreateBusSeat(ctx, db.CreateBusSeatParams{
			BusID:      bus.ID,
			SeatNumber: int32(i),
		})
		require.NoError(t, seatErr)
		seats = append(seats, seat)
	}

	return BusPurchaseFixture{
		Route:               route,
		Bus:                 bus,
		Seats:               seats,
		OriginCityName:      originCityName,
		DestinationCityName: destinationCityName,
	}
}

func FetchTicketSnapshot(ctx context.Context, pool *sql.DB, ticketID int32) (TicketSnapshot, error) {
	var snapshot TicketSnapshot

	err := pool.QueryRowContext(ctx, `
		SELECT
			t.id,
			u.username,
			t.bus_id,
			sr.bus_seat_id,
			t.status,
			sr.status,
			bs.status,
			t.seat_reservation_id
		FROM tickets t
		JOIN users u ON u.id = t.user_id
		JOIN seat_reservations sr ON sr.id = t.seat_reservation_id
		JOIN bus_seats bs ON bs.id = sr.bus_seat_id
		WHERE t.id = ?
	`, ticketID).Scan(
		&snapshot.TicketID,
		&snapshot.Username,
		&snapshot.BusID,
		&snapshot.SeatID,
		&snapshot.TicketStatus,
		&snapshot.ReservationStatus,
		&snapshot.SeatStatus,
		&snapshot.SeatReservationID,
	)

	return snapshot, err
}

func FetchSeatStatus(ctx context.Context, pool *sql.DB, seatID int32) (string, error) {
	var status string
	err := pool.QueryRowContext(ctx, `SELECT status FROM bus_seats WHERE id = ?`, seatID).Scan(&status)
	return status, err
}

func BuildTestDSN(config util.Config) string {
	host := config.DBHOST
	if strings.EqualFold(host, "localhost") {
		host = "127.0.0.1"
	}

	return fmt.Sprintf("%s:%s@tcp(%s:%s)/%s?parseTime=true&multiStatements=true",
		config.DBUSERNAME,
		config.DBPASSWORD,
		host,
		config.DBPORT,
		config.DBDATABASE,
	)
}

func RunMigrations(t *testing.T, config util.Config, root string) {
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

func ProjectRoot(t *testing.T) string {
	t.Helper()

	_, filename, _, ok := runtime.Caller(0)
	require.True(t, ok)

	return filepath.Dir(filepath.Dir(filepath.Dir(filename)))
}
