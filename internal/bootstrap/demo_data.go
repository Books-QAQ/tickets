package bootstrap

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/rs/zerolog/log"
)

var beijingLocation = time.FixedZone("CST", 8*3600)

type terminalSeed struct {
	City string
	Name string
}

type routeSeed struct {
	OriginTerminal      string
	DestinationTerminal string
	DurationMinutes     int
	DistanceKM          int
}

type serviceSeed struct {
	RouteKey         string
	ServiceCode      string
	DepartureHour    int
	DepartureMinute  int
	DurationMinutes  int
	SeatCount        int
	Price            int
	BusType          string
	Corporation      string
	SuperCorporation string
	IsVIP            bool
}

var seedCities = []string{
	"Shanghai",
	"Hangzhou",
	"Suzhou",
	"Nanjing",
	"Ningbo",
}

var seedTerminals = []terminalSeed{
	{City: "Shanghai", Name: "Shanghai South"},
	{City: "Shanghai", Name: "Shanghai Hongqiao"},
	{City: "Hangzhou", Name: "Hangzhou East"},
	{City: "Hangzhou", Name: "Hangzhou West"},
	{City: "Suzhou", Name: "Suzhou North"},
	{City: "Nanjing", Name: "Nanjing South"},
	{City: "Ningbo", Name: "Ningbo South"},
}

var seedRoutes = []routeSeed{
	{OriginTerminal: "Shanghai South", DestinationTerminal: "Hangzhou East", DurationMinutes: 120, DistanceKM: 180},
	{OriginTerminal: "Hangzhou East", DestinationTerminal: "Shanghai South", DurationMinutes: 120, DistanceKM: 180},
	{OriginTerminal: "Shanghai Hongqiao", DestinationTerminal: "Suzhou North", DurationMinutes: 95, DistanceKM: 110},
	{OriginTerminal: "Suzhou North", DestinationTerminal: "Nanjing South", DurationMinutes: 150, DistanceKM: 215},
	{OriginTerminal: "Nanjing South", DestinationTerminal: "Shanghai Hongqiao", DurationMinutes: 210, DistanceKM: 300},
	{OriginTerminal: "Shanghai South", DestinationTerminal: "Ningbo South", DurationMinutes: 180, DistanceKM: 230},
	{OriginTerminal: "Ningbo South", DestinationTerminal: "Hangzhou West", DurationMinutes: 140, DistanceKM: 165},
	{OriginTerminal: "Hangzhou West", DestinationTerminal: "Shanghai Hongqiao", DurationMinutes: 130, DistanceKM: 175},
}

var seedServices = []serviceSeed{
	{RouteKey: routeKey("Shanghai South", "Hangzhou East"), ServiceCode: "TM-SH-HZ-0800", DepartureHour: 8, DepartureMinute: 0, DurationMinutes: 120, SeatCount: 16, Price: 88, BusType: "standard", Corporation: "Transit Express"},
	{RouteKey: routeKey("Shanghai South", "Hangzhou East"), ServiceCode: "TM-SH-HZ-1500", DepartureHour: 15, DepartureMinute: 0, DurationMinutes: 120, SeatCount: 16, Price: 92, BusType: "standard", Corporation: "Transit Express"},
	{RouteKey: routeKey("Hangzhou East", "Shanghai South"), ServiceCode: "TM-HZ-SH-0930", DepartureHour: 9, DepartureMinute: 30, DurationMinutes: 120, SeatCount: 16, Price: 86, BusType: "standard", Corporation: "Transit Express"},
	{RouteKey: routeKey("Shanghai Hongqiao", "Suzhou North"), ServiceCode: "TM-SH-SZ-0740", DepartureHour: 7, DepartureMinute: 40, DurationMinutes: 95, SeatCount: 16, Price: 58, BusType: "standard", Corporation: "Delta Coach"},
	{RouteKey: routeKey("Suzhou North", "Nanjing South"), ServiceCode: "TM-SZ-NJ-1300", DepartureHour: 13, DepartureMinute: 0, DurationMinutes: 150, SeatCount: 16, Price: 76, BusType: "standard", Corporation: "Delta Coach"},
	{RouteKey: routeKey("Nanjing South", "Shanghai Hongqiao"), ServiceCode: "TM-NJ-SH-1620", DepartureHour: 16, DepartureMinute: 20, DurationMinutes: 210, SeatCount: 16, Price: 118, BusType: "vip", Corporation: "Metro Star", IsVIP: true},
	{RouteKey: routeKey("Shanghai South", "Ningbo South"), ServiceCode: "TM-SH-NB-0730", DepartureHour: 7, DepartureMinute: 30, DurationMinutes: 180, SeatCount: 16, Price: 98, BusType: "standard", Corporation: "Coastal Line"},
	{RouteKey: routeKey("Ningbo South", "Hangzhou West"), ServiceCode: "TM-NB-HZ-1120", DepartureHour: 11, DepartureMinute: 20, DurationMinutes: 140, SeatCount: 16, Price: 72, BusType: "standard", Corporation: "Coastal Line"},
	{RouteKey: routeKey("Hangzhou West", "Shanghai Hongqiao"), ServiceCode: "TM-HZ-SH-1810", DepartureHour: 18, DepartureMinute: 10, DurationMinutes: 130, SeatCount: 16, Price: 84, BusType: "standard", Corporation: "Metro Star"},
}

func routeKey(originTerminal, destinationTerminal string) string {
	return originTerminal + "->" + destinationTerminal
}

func StartDemoDataScheduler(ctx context.Context, db *sql.DB) {
	go func() {
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				runSeed(ctx, db, "hourly")
			}
		}
	}()
}

func EnsureDemoData(ctx context.Context, db *sql.DB) error {
	return ensureDemoData(ctx, db)
}

func runSeed(ctx context.Context, db *sql.DB, source string) {
	if err := ensureDemoData(ctx, db); err != nil {
		log.Error().Err(err).Str("source", source).Msg("demo data seeding failed")
		return
	}

	log.Info().Str("source", source).Msg("demo data seeding completed")
}

func ensureDemoData(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}

	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	if err = ensureCities(ctx, tx); err != nil {
		return err
	}

	cityIDs, err := loadCityIDs(ctx, tx)
	if err != nil {
		return err
	}

	if err = ensureTerminals(ctx, tx, cityIDs); err != nil {
		return err
	}

	terminalIDs, err := loadTerminalIDs(ctx, tx)
	if err != nil {
		return err
	}

	if err = ensureRoutes(ctx, tx, terminalIDs); err != nil {
		return err
	}

	routeIDs, err := loadRouteIDs(ctx, tx, terminalIDs)
	if err != nil {
		return err
	}

	if err = ensureFutureBuses(ctx, tx, routeIDs); err != nil {
		return err
	}

	return tx.Commit()
}

func ensureCities(ctx context.Context, tx *sql.Tx) error {
	stmt, err := tx.PrepareContext(ctx, `INSERT IGNORE INTO cities (name) VALUES (?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, city := range seedCities {
		if _, err := stmt.ExecContext(ctx, city); err != nil {
			return err
		}
	}

	return nil
}

func loadCityIDs(ctx context.Context, tx *sql.Tx) (map[string]int32, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, name FROM cities`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make(map[string]int32)
	for rows.Next() {
		var id int32
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		result[name] = id
	}

	return result, rows.Err()
}

func ensureTerminals(ctx context.Context, tx *sql.Tx, cityIDs map[string]int32) error {
	stmt, err := tx.PrepareContext(ctx, `INSERT IGNORE INTO terminals (city_id, name) VALUES (?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, terminal := range seedTerminals {
		cityID, ok := cityIDs[terminal.City]
		if !ok {
			return fmt.Errorf("missing city id for terminal %s", terminal.Name)
		}

		if _, err := stmt.ExecContext(ctx, cityID, terminal.Name); err != nil {
			return err
		}
	}

	return nil
}

func loadTerminalIDs(ctx context.Context, tx *sql.Tx) (map[string]int32, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, name FROM terminals`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make(map[string]int32)
	for rows.Next() {
		var id int32
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		result[name] = id
	}

	return result, rows.Err()
}

func ensureRoutes(ctx context.Context, tx *sql.Tx, terminalIDs map[string]int32) error {
	stmt, err := tx.PrepareContext(ctx, `
		INSERT IGNORE INTO routes (origin_terminal_id, destination_terminal_id, duration, distance)
		VALUES (?, ?, ?, ?)
	`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, route := range seedRoutes {
		originID, ok := terminalIDs[route.OriginTerminal]
		if !ok {
			return fmt.Errorf("missing origin terminal id for %s", route.OriginTerminal)
		}

		destinationID, ok := terminalIDs[route.DestinationTerminal]
		if !ok {
			return fmt.Errorf("missing destination terminal id for %s", route.DestinationTerminal)
		}

		if _, err := stmt.ExecContext(ctx, originID, destinationID, route.DurationMinutes, route.DistanceKM); err != nil {
			return err
		}
	}

	return nil
}

func loadRouteIDs(ctx context.Context, tx *sql.Tx, terminalIDs map[string]int32) (map[string]int32, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, origin_terminal_id, destination_terminal_id FROM routes`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	reverseTerminalIDs := make(map[int32]string)
	for name, id := range terminalIDs {
		reverseTerminalIDs[id] = name
	}

	result := make(map[string]int32)
	for rows.Next() {
		var id int32
		var originID int32
		var destinationID int32
		if err := rows.Scan(&id, &originID, &destinationID); err != nil {
			return nil, err
		}

		result[routeKey(reverseTerminalIDs[originID], reverseTerminalIDs[destinationID])] = id
	}

	return result, rows.Err()
}

func ensureFutureBuses(ctx context.Context, tx *sql.Tx, routeIDs map[string]int32) error {
	insertBusSQL := `
		INSERT INTO buses (
			route_id,
			departure_time,
			arrival_time,
			sale_open_at,
			capacity,
			price,
			bus_type,
			corporation,
			super_corporation,
			service_number,
			is_vip
		)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE
			route_id = VALUES(route_id),
			departure_time = VALUES(departure_time),
			arrival_time = VALUES(arrival_time),
			sale_open_at = VALUES(sale_open_at),
			capacity = VALUES(capacity),
			price = VALUES(price),
			bus_type = VALUES(bus_type),
			corporation = VALUES(corporation),
			super_corporation = VALUES(super_corporation),
			is_vip = VALUES(is_vip)
	`

	insertSeatSQL := `
		INSERT IGNORE INTO bus_seats (bus_id, seat_number, status)
		VALUES (?, ?, 'available')
	`

	selectBusIDSQL := `SELECT id FROM buses WHERE service_number = ?`

	now := time.Now().In(beijingLocation)
	for offset := 1; offset <= 10; offset++ {
		day := now.AddDate(0, 0, offset)
		for _, service := range seedServices {
			routeID, ok := routeIDs[service.RouteKey]
			if !ok {
				return fmt.Errorf("missing route id for %s", service.RouteKey)
			}

			departureAt := time.Date(day.Year(), day.Month(), day.Day(), service.DepartureHour, service.DepartureMinute, 0, 0, beijingLocation)
			arrivalAt := departureAt.Add(time.Duration(service.DurationMinutes) * time.Minute)
			saleDay := departureAt.AddDate(0, 0, -7)
			saleOpenAt := time.Date(saleDay.Year(), saleDay.Month(), saleDay.Day(), 8, 0, 0, 0, beijingLocation)
			serviceNumber := fmt.Sprintf("%s-%s", service.ServiceCode, departureAt.Format("20060102"))

			if _, err := tx.ExecContext(
				ctx,
				insertBusSQL,
				routeID,
				departureAt,
				arrivalAt,
				saleOpenAt,
				service.SeatCount,
				service.Price,
				service.BusType,
				nullIfEmpty(service.Corporation),
				nullIfEmpty(service.SuperCorporation),
				serviceNumber,
				service.IsVIP,
			); err != nil {
				return err
			}

			var busID int32
			if err := tx.QueryRowContext(ctx, selectBusIDSQL, serviceNumber).Scan(&busID); err != nil {
				return err
			}

			for seatNumber := 1; seatNumber <= service.SeatCount; seatNumber++ {
				if _, err := tx.ExecContext(ctx, insertSeatSQL, busID, seatNumber); err != nil {
					return err
				}
			}
		}
	}

	return nil
}

func nullIfEmpty(value string) any {
	if value == "" {
		return nil
	}
	return value
}
