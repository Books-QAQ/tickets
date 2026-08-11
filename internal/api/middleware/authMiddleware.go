package middleware

import (
	"strings"

	"github.com/gofiber/fiber/v2"
	"github.com/Books-QAQ/tickets/internal/token"
)

const (
	authorizationHeaderKey  = "authorization"
	authorizationPayloadKey = "authorization_payload"
)

// AuthMiddleware validates a bearer token from the Authorization header.
func AuthMiddleware(tokenMaker token.Maker) fiber.Handler {
	return func(c *fiber.Ctx) error {
		authHeader := c.Get(authorizationHeaderKey)
		if authHeader == "" {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "missing authorization header"})
		}

		fields := strings.Fields(authHeader)
		if len(fields) != 2 {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "invalid authorization header format"})
		}

		if !strings.EqualFold(fields[0], "Bearer") {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "unsupported authorization type"})
		}

		payload, err := tokenMaker.VerifyToken(fields[1])
		if err != nil {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "invalid token"})
		}

		c.Locals("authorizationPayloadKey", payload)
		return c.Next()
	}
}
