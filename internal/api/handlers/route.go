package handlers

import (
	"net/http"
	"strconv"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/Books-QAQ/tickets/internal/cache"
	db "github.com/Books-QAQ/tickets/internal/db/sqlc"
	"github.com/Books-QAQ/tickets/internal/token"
	"github.com/Books-QAQ/tickets/internal/util"
	"github.com/redis/go-redis/v9"
)

type RouteHandler struct {
	store      *db.Store
	redis      *redis.Client
	tokenMaker token.Maker
	config     util.Config
}

type routeResponse struct {
	RouteID         int32     `json:"route_id"`
	BusID           int32     `json:"bus_id"`
	OriginCity      string    `json:"origin_city"`
	DestinationCity string    `json:"destination_city"`
	DepartureTime   time.Time `json:"departure_time"`
	ArrivalTime     time.Time `json:"arrival_time"`
	SaleOpenAt      time.Time `json:"sale_open_at"`
	CanPurchase     bool      `json:"can_purchase"`
	AvailableSeats  int64     `json:"available_seats"`
	Price           int32     `json:"price"`
}

func NewRouteHandler(store *db.Store, redisClient *redis.Client, tokenMaker token.Maker, config util.Config) *RouteHandler {
	return &RouteHandler{
		store:      store,
		redis:      redisClient,
		tokenMaker: tokenMaker,
		config:     config,
	}
}

func parseDepartureDate(value string) (time.Time, error) {
	if parsed, err := time.ParseInLocation("2006-01-02", value, beijingLocation); err == nil {
		return parsed, nil
	}

	if parsed, err := time.Parse(time.RFC3339, value); err == nil {
		return parsed.In(beijingLocation), nil
	}

	if parsed, err := time.ParseInLocation("2006-01-02T15:04:05", value, beijingLocation); err == nil {
		return parsed, nil
	}

	return time.Time{}, fiber.NewError(http.StatusBadRequest, "invalid departure date, use YYYY-MM-DD")
}

func (h *RouteHandler) SearchRoutes(c *fiber.Ctx) error {
	originTerminalID, err := strconv.ParseInt(c.Query("origin_city_id"), 10, 32)
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid origin terminal"})
	}

	destinationTerminalID, err := strconv.ParseInt(c.Query("destination_city_id"), 10, 32)
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid destination terminal"})
	}

	departureDate, err := parseDepartureDate(c.Query("departure_time"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": err.Error()})
	}

	cacheKey := cache.RoutesQueryCacheKey(int32(originTerminalID), int32(destinationTerminalID), departureDate)
	cachedRoutes, found, err := cache.GetJSON[[]routeResponse](c.Context(), h.redis, cacheKey)
	if err == nil && found {
		return c.Status(http.StatusOK).JSON(cachedRoutes)
	}

	routes, err := h.store.ListRoutes(c.Context(), db.ListRoutesParams{
		OriginTerminalID:      int32(originTerminalID),
		DestinationTerminalID: int32(destinationTerminalID),
		DepartureTime:         departureDate,
	})
	if err != nil {
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "Failed to fetch routes"})
	}

	response := make([]routeResponse, 0, len(routes))
	nowInBeijing := time.Now().In(beijingLocation)
	for _, route := range routes {
		saleOpenAt, err := h.store.GetBusSaleOpenAt(c.Context(), route.BusID)
		if err != nil {
			return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "Failed to fetch route sale window"})
		}

		response = append(response, routeResponse{
			RouteID:         route.RouteID,
			BusID:           route.BusID,
			OriginCity:      route.OriginTerminalName,
			DestinationCity: route.DestinationTerminalName,
			DepartureTime:   route.DepartureTime,
			ArrivalTime:     route.ArrivalTime,
			SaleOpenAt:      saleOpenAt,
			CanPurchase:     !nowInBeijing.Before(saleOpenAt.In(beijingLocation)),
			AvailableSeats:  route.AvailableSeats,
			Price:           route.Price,
		})
	}

	_ = cache.SetJSON(c.Context(), h.redis, cacheKey, response, 3*time.Minute)

	return c.Status(http.StatusOK).JSON(response)
}
