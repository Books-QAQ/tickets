package middleware

import (
	"fmt"
	"strconv"

	"github.com/gofiber/fiber/v2"
	"github.com/Books-QAQ/tickets/internal/cache"
	"github.com/Books-QAQ/tickets/internal/token"
	"github.com/Books-QAQ/tickets/internal/util"
	"github.com/redis/go-redis/v9"
)

func NewPurchaseRateLimiter(redisClient *redis.Client, config util.Config) fiber.Handler {
	return func(c *fiber.Ctx) error {
		if redisClient == nil || config.PurchaseRateLimitCapacity <= 0 || config.PurchaseRateLimitRefillRate <= 0 {
			return c.Next()
		}

		payload, ok := c.Locals("authorizationPayloadKey").(*token.Payload)
		if !ok || payload == nil {
			return c.Next()
		}

		key := fmt.Sprintf("rate:purchase:user:%s", payload.Username)
		allowed, retryAfter, err := cache.AllowTokenBucket(
			c.Context(),
			redisClient,
			key,
			config.PurchaseRateLimitCapacity,
			config.PurchaseRateLimitRefillRate,
			1,
		)
		if err != nil {
			return c.Next()
		}

		if !allowed {
			c.Set("Retry-After", strconv.Itoa(max(1, int(retryAfter.Seconds()))))
			return c.Status(fiber.StatusTooManyRequests).JSON(fiber.Map{
				"error": "too many purchase attempts, please slow down",
			})
		}

		return c.Next()
	}
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
