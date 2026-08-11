package handlers

import (
	"net/http"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/Books-QAQ/tickets/internal/cache"
	db "github.com/Books-QAQ/tickets/internal/db/sqlc"
	"github.com/Books-QAQ/tickets/internal/token"
	"github.com/Books-QAQ/tickets/internal/util"
	"github.com/redis/go-redis/v9"
)

const terminalsCacheKey = "terminals:all"

type TerminalHandler struct {
	store      *db.Store
	redis      *redis.Client
	tokenMaker token.Maker
	config     util.Config
}

func NewTerminalHandler(store *db.Store, redisClient *redis.Client, tokenMaker token.Maker, config util.Config) *TerminalHandler {
	return &TerminalHandler{
		store:      store,
		redis:      redisClient,
		tokenMaker: tokenMaker,
		config:     config,
	}
}

func (h *TerminalHandler) ListTerminals(c *fiber.Ctx) error {
	terminals, found, err := cache.GetJSON[[]db.ListTerminalsRow](c.Context(), h.redis, terminalsCacheKey)
	if err == nil && found {
		return c.Status(http.StatusOK).JSON(terminals)
	}

	terminals, err = h.store.ListTerminals(c.Context())
	if err != nil {
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "Failed to fetch terminals"})
	}

	_ = cache.SetJSON(c.Context(), h.redis, terminalsCacheKey, terminals, 10*time.Minute)

	return c.JSON(terminals)
}
