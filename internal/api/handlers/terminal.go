package handlers

import (
	"math/rand"
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

	// TTL 随机抖动：使基础数据缓存过期时刻错开，避免大量 key 同一瞬间集体失效（防雪崩）
	_ = cache.SetJSON(c.Context(), h.redis, terminalsCacheKey, terminals, 10*time.Minute+time.Duration(rand.Intn(60))*time.Second)

	return c.JSON(terminals)
}
