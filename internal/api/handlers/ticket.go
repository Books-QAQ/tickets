package handlers

import (
	"context"
	"database/sql"
	"net/http"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/Books-QAQ/tickets/internal/cache"
	db "github.com/Books-QAQ/tickets/internal/db/sqlc"
	"github.com/Books-QAQ/tickets/internal/token"
	"github.com/Books-QAQ/tickets/internal/util"
	"github.com/redis/go-redis/v9"
)

var beijingLocation = time.FixedZone("CST", 8*3600)

type TicketHandler struct {
	store      *db.Store
	redis      *redis.Client
	tokenMaker token.Maker
	config     util.Config
}

type CancelTicketRequest struct {
	TicketID int32 `params:"id" validate:"required"`
}

type reserveSeatRequest struct {
	RouteID int32 `json:"route_id" validate:"required"`
	BusID   int32 `json:"bus_id" validate:"required"`
	SeatID  int32 `json:"seat_id" validate:"required"`
}

type ListUserTicketsResponse struct {
	TicketID      int32     `json:"ticket_id"`
	BusID         int32     `json:"bus_id"`
	SeatID        int32     `json:"seat_id"`
	ReservedAt    time.Time `json:"reserved_at"`
	DepartureTime time.Time `json:"departure_time"`
	ArrivalTime   time.Time `json:"arrival_time"`
	Price         int       `json:"price"`
	SeatNumber    int       `json:"seat_number"`
	Status        string    `json:"status"`
}

func NewTicketHandler(store *db.Store, redisClient *redis.Client, tokenMaker token.Maker, config util.Config) *TicketHandler {
	return &TicketHandler{
		store:      store,
		redis:      redisClient,
		tokenMaker: tokenMaker,
		config:     config,
	}
}

func (h *TicketHandler) invalidateRoutesCache(ctx context.Context, routeID, busID int32) {
	bus, err := h.store.GetBusByID(ctx, busID)
	if err != nil {
		return
	}

	targetRouteID := routeID
	if targetRouteID == 0 {
		targetRouteID = bus.RouteID
	}

	route, err := h.store.GetRouteByID(ctx, targetRouteID)
	if err != nil {
		return
	}

	key := cache.RoutesQueryCacheKey(route.OriginTerminalID, route.DestinationTerminalID, bus.DepartureTime)
	_ = cache.DeleteKey(ctx, h.redis, key)
}

func (h *TicketHandler) validateSeatRequest(c *fiber.Ctx, req reserveSeatRequest) error {
	_, err := h.store.CheckBusRouteAssociation(c.Context(), db.CheckBusRouteAssociationParams{
		ID:   req.RouteID,
		ID_2: req.BusID,
	})
	if err != nil {
		if err == sql.ErrNoRows {
			return fiber.NewError(http.StatusNotFound, "Bus or Route not found or they do not match")
		}
		return fiber.NewError(http.StatusInternalServerError, "Failed to validate bus and route association")
	}

	seat, err := h.store.CheckSeatAvailability(c.Context(), db.CheckSeatAvailabilityParams{
		ID:    req.SeatID,
		BusID: req.BusID,
	})
	if err != nil {
		if err == sql.ErrNoRows {
			return fiber.NewError(http.StatusNotFound, "Seat not found")
		}
		return fiber.NewError(http.StatusInternalServerError, "Failed to fetch seat information")
	}

	if seat.Status != "available" {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "Seat is not available for purchase"})
	}

	return nil
}

func (h *TicketHandler) currentUser(c *fiber.Ctx) (db.GetUserByUsernameRow, error) {
	payload := c.Locals("authorizationPayloadKey").(*token.Payload)
	return h.store.GetUserByUsername(c.Context(), payload.Username)
}

func (h *TicketHandler) ensureBusOnSale(c *fiber.Ctx, busID int32) error {
	saleOpenAt, err := h.store.GetBusSaleOpenAt(c.Context(), busID)
	if err != nil {
		if err == sql.ErrNoRows {
			return fiber.NewError(http.StatusNotFound, "Bus not found")
		}
		return fiber.NewError(http.StatusInternalServerError, "Failed to fetch sale window")
	}

	nowInBeijing := time.Now().In(beijingLocation)
	if nowInBeijing.Before(saleOpenAt.In(beijingLocation)) {
		return c.Status(http.StatusForbidden).JSON(fiber.Map{
			"error":        "This bus is not on sale yet",
			"sale_open_at": saleOpenAt,
		})
	}

	return nil
}

func (h *TicketHandler) ListUserTickets(c *fiber.Ctx) error {
	payload := c.Locals("authorizationPayloadKey").(*token.Payload)

	user, err := h.store.GetUserByUsername(c.Context(), payload.Username)
	if err != nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "User not found"})
	}

	tickets, err := h.store.ListUserTickets(c.Context(), user.ID)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "Failed to fetch user tickets"})
	}

	response := make([]ListUserTicketsResponse, 0, len(tickets))
	for _, ticket := range tickets {
		response = append(response, ListUserTicketsResponse{
			TicketID:      ticket.TicketID,
			BusID:         ticket.BusID,
			SeatID:        ticket.SeatID,
			ReservedAt:    ticket.ReservedAt.Time,
			DepartureTime: ticket.DepartureTime,
			ArrivalTime:   ticket.ArrivalTime,
			Price:         int(ticket.Price),
			SeatNumber:    int(ticket.SeatNumber),
			Status:        ticket.ReservationStatus,
		})
	}

	return c.Status(http.StatusOK).JSON(response)
}

func (h *TicketHandler) CancelTicket(c *fiber.Ctx) error {
	var req CancelTicketRequest
	if err := c.ParamsParser(&req); err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "Invalid request parameters"})
	}

	payload := c.Locals("authorizationPayloadKey").(*token.Payload)
	user, err := h.store.GetUserByUsername(c.Context(), payload.Username)
	if err != nil {
		return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": "User not found"})
	}

	ticket, err := h.store.GetTicketByID(c.Context(), req.TicketID)
	if err != nil {
		if err == sql.ErrNoRows {
			return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": "Ticket not found"})
		}
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "Failed to fetch ticket details"})
	}

	if ticket.UserID != user.ID {
		return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": "You do not have permission to cancel this ticket"})
	}

	if ticket.Status == "canceled" {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "Ticket is already canceled"})
	}

	err = h.store.CancelTicketTx(c.Context(), db.CancelTicketParams{
		TicketID: ticket.ID,
		SeatID:   ticket.SeatReservationID,
		UserID:   user.ID,
	})
	if err != nil {
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "Failed to cancel ticket"})
	}

	h.invalidateRoutesCache(c.Context(), 0, ticket.BusID)

	return c.Status(http.StatusOK).JSON(fiber.Map{"message": "Ticket canceled successfully"})
}
