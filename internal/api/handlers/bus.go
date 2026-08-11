package handlers

import (
	"database/sql"
	"net/http"
	"strconv"

	"github.com/gofiber/fiber/v2"
	db "github.com/Books-QAQ/tickets/internal/db/sqlc"
	"github.com/Books-QAQ/tickets/internal/token"
	"github.com/Books-QAQ/tickets/internal/util"
)

type BusHandler struct {
	store      *db.Store
	tokenMaker token.Maker
	config     util.Config
}

type seatResponse struct {
	SeatID     int32  `json:"seat_id"`
	SeatNumber int32  `json:"seat_number"`
	Status     string `json:"status"`
}

func NewBusHandler(store *db.Store, tokenMaker token.Maker, config util.Config) *BusHandler {
	return &BusHandler{
		store:      store,
		tokenMaker: tokenMaker,
		config:     config,
	}
}

func (h *BusHandler) ListAvailableSeats(c *fiber.Ctx) error {
	routeID, err := strconv.ParseInt(c.Params("route_id"), 10, 32)
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid route id"})
	}

	busID, err := strconv.ParseInt(c.Params("bus_id"), 10, 32)
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid bus id"})
	}

	_, err = h.store.CheckBusRouteAssociation(c.Context(), db.CheckBusRouteAssociationParams{
		ID:   int32(routeID),
		ID_2: int32(busID),
	})
	if err != nil {
		if err == sql.ErrNoRows {
			return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": "Bus or Route not found or they do not match"})
		}
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "Failed to validate bus and route association"})
	}

	seats, err := h.store.GetBusSeats(c.Context(), int32(busID))
	if err != nil {
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "Failed to fetch seats"})
	}

	response := make([]seatResponse, 0, len(seats))
	for _, seat := range seats {
		response = append(response, seatResponse{
			SeatID:     seat.ID,
			SeatNumber: seat.SeatNumber,
			Status:     seat.Status,
		})
	}

	return c.Status(http.StatusOK).JSON(response)
}
