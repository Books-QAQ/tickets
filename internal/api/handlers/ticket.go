package handlers

import (
	"context"
	"database/sql"
	"net/http"
	"time"

	"github.com/go-playground/validator/v10"
	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/Books-QAQ/tickets/internal/cache"
	db "github.com/Books-QAQ/tickets/internal/db/sqlc"
	"github.com/Books-QAQ/tickets/internal/token"
	"github.com/Books-QAQ/tickets/internal/util"
	"github.com/Books-QAQ/tickets/internal/worker"
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

type PurchaseTaskStatusRequest struct {
	RequestID string `params:"id" validate:"required"`
}

type reserveSeatRequest struct {
	RouteID int32 `json:"route_id" validate:"required"`
	BusID   int32 `json:"bus_id" validate:"required"`
	SeatID  int32 `json:"seat_id" validate:"required"`
}

type reserveSeatResponse struct {
	TicketID   int32     `json:"ticket_id"`
	BusID      int32     `json:"bus_id"`
	SeatID     int32     `json:"seat_id"`
	ReservedAt time.Time `json:"reserved_at"`
}

type asyncPurchaseAcceptedResponse struct {
	RequestID string    `json:"request_id"`
	Status    string    `json:"status"`
	ExpiresAt time.Time `json:"expires_at"`
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

func (h *TicketHandler) seatHoldTTL() time.Duration {
	if h.config.SeatHoldTTL > 0 {
		return h.config.SeatHoldTTL
	}
	return 2 * time.Minute
}

func (h *TicketHandler) taskTTL() time.Duration {
	if h.config.PurchaseTaskTTL > 0 {
		return h.config.PurchaseTaskTTL
	}
	return 30 * time.Minute
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

func (h *TicketHandler) ReserveSeat(c *fiber.Ctx) error {
	var req reserveSeatRequest
	if err := c.BodyParser(&req); err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "Invalid request parameters"})
	}

	validate := validator.New()
	if err := validate.Struct(&req); err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": err.Error()})
	}

	if err := h.validateSeatRequest(c, req); err != nil {
		return err
	}

	if err := h.ensureBusOnSale(c, req.BusID); err != nil {
		return err
	}

	user, err := h.currentUser(c)
	if err != nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": err.Error()})
	}

	reservation, err := h.store.ReserveTicketTx(c.Context(), db.ReserveTicketTxParams{
		UserID: user.ID,
		BusID:  req.BusID,
		SeatID: req.SeatID,
	})
	if err != nil {
		return fiber.NewError(http.StatusInternalServerError, "Failed to reserve seat: "+err.Error())
	}

	h.invalidateRoutesCache(c.Context(), req.RouteID, req.BusID)

	return c.Status(http.StatusOK).JSON(reservation)
}

func (h *TicketHandler) PurchaseTicket(c *fiber.Ctx) error {
	var req reserveSeatRequest
	if err := c.BodyParser(&req); err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "Invalid request parameters"})
	}

	validate := validator.New()
	if err := validate.Struct(&req); err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": err.Error()})
	}

	if err := h.validateSeatRequest(c, req); err != nil {
		return err
	}

	if err := h.ensureBusOnSale(c, req.BusID); err != nil {
		return err
	}

	user, err := h.currentUser(c)
	if err != nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": err.Error()})
	}

	result, err := h.store.PurchaseTicketTx(c.Context(), db.PurchaseTicketTxParams{
		UserID: user.ID,
		BusID:  req.BusID,
		SeatID: req.SeatID,
	})
	if err != nil {
		return fiber.NewError(http.StatusInternalServerError, "Failed to purchase ticket")
	}

	h.invalidateRoutesCache(c.Context(), req.RouteID, req.BusID)

	response := reserveSeatResponse{
		TicketID:   result.TicketID,
		BusID:      result.BusID,
		SeatID:     result.SeatID,
		ReservedAt: result.ReservedAt,
	}

	return c.Status(http.StatusOK).JSON(response)
}

func (h *TicketHandler) PurchaseTicketAsync(c *fiber.Ctx) error {
	var req reserveSeatRequest
	if err := c.BodyParser(&req); err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "Invalid request parameters"})
	}

	validate := validator.New()
	if err := validate.Struct(&req); err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": err.Error()})
	}

	if err := h.validateSeatRequest(c, req); err != nil {
		return err
	}

	if err := h.ensureBusOnSale(c, req.BusID); err != nil {
		return err
	}

	user, err := h.currentUser(c)
	if err != nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": err.Error()})
	}

	requestID := uuid.NewString()
	owner := worker.SeatHoldOwner(user.ID, requestID)
	holdTTL := h.seatHoldTTL()

	claimed, err := cache.AcquireSeatHold(c.Context(), h.redis, req.BusID, req.SeatID, owner, holdTTL)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to acquire seat hold"})
	}
	if !claimed {
		return c.Status(fiber.StatusConflict).JSON(fiber.Map{"error": "seat is temporarily held by another request"})
	}

	task := worker.PurchaseTaskStatus{
		RequestID: requestID,
		UserID:    user.ID,
		Status:    "queued",
		BusID:     req.BusID,
		SeatID:    req.SeatID,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}

	taskKey := cache.PurchaseTaskKey(requestID)
	if err := cache.SetJSON(c.Context(), h.redis, taskKey, task, h.taskTTL()); err != nil {
		_ = cache.ReleaseSeatHold(c.Context(), h.redis, req.BusID, req.SeatID, owner)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to create purchase task"})
	}

	message := worker.PurchaseRequestMessage{
		RequestID: requestID,
		UserID:    user.ID,
		RouteID:   req.RouteID,
		BusID:     req.BusID,
		SeatID:    req.SeatID,
	}

	shard := worker.PurchaseShard(req.SeatID, h.config.PurchaseWorkerCount)
	if err := cache.EnqueueJSON(c.Context(), h.redis, cache.PurchaseQueueKeyForShard(shard), message); err != nil {
		_ = cache.ReleaseSeatHold(c.Context(), h.redis, req.BusID, req.SeatID, owner)
		_ = cache.DeleteKey(c.Context(), h.redis, taskKey)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to enqueue purchase task"})
	}

	return c.Status(http.StatusAccepted).JSON(asyncPurchaseAcceptedResponse{
		RequestID: requestID,
		Status:    "queued",
		ExpiresAt: time.Now().Add(holdTTL),
	})
}

func (h *TicketHandler) GetPurchaseTaskStatus(c *fiber.Ctx) error {
	var req PurchaseTaskStatusRequest
	if err := c.ParamsParser(&req); err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "Invalid request parameters"})
	}

	user, err := h.currentUser(c)
	if err != nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": err.Error()})
	}

	task, found, err := cache.GetJSON[worker.PurchaseTaskStatus](c.Context(), h.redis, cache.PurchaseTaskKey(req.RequestID))
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to fetch purchase task"})
	}
	if !found {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "purchase task not found"})
	}
	if task.UserID != user.ID {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "You do not have permission to view this purchase task"})
	}

	return c.Status(http.StatusOK).JSON(task)
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
