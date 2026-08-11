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

const citiesCacheKey = "cities:all"

type CityHandler struct {
	store      *db.Store
	redis      *redis.Client
	tokenMaker token.Maker
	config     util.Config
}

func NewCityHandler(store *db.Store, redisClient *redis.Client, tokenMaker token.Maker, config util.Config) *CityHandler {
	return &CityHandler{
		store:      store,
		redis:      redisClient,
		tokenMaker: tokenMaker,
		config:     config,
	}
}

func (h *CityHandler) ListCities(c *fiber.Ctx) error {
	cities, found, err := cache.GetJSON[[]db.City](c.Context(), h.redis, citiesCacheKey)
	if err == nil && found {
		return c.Status(http.StatusOK).JSON(cities)
	}

	cities, err = h.store.GetAllCities(c.Context())
	if err != nil {
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{
			"error": "failed to fetch cities",
		})
	}

	_ = cache.SetJSON(c.Context(), h.redis, citiesCacheKey, cities, 10*time.Minute)

	return c.Status(http.StatusOK).JSON(cities)
}
