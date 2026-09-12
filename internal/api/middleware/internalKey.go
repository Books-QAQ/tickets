package middleware

import (
	"crypto/subtle"
	"strings"

	"github.com/gofiber/fiber/v2"
)

// InternalKeyHeader 内网调用密钥头（§6.1）
const InternalKeyHeader = "X-Internal-Key"

// InternalKeyMiddleware 校验内网密钥。**不允许降级放行**：
// 未配置密钥时直接拒绝（宁可服务不可用，也不把内部能力暴露出去）。
func InternalKeyMiddleware(expected string) fiber.Handler {
	return func(c *fiber.Ctx) error {
		if expected == "" {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{
				"error": fiber.Map{"kind": "unauthorized", "message": "internal key 未配置，拒绝内网调用"},
			})
		}
		got := strings.TrimSpace(c.Get(InternalKeyHeader))
		if subtle.ConstantTimeCompare([]byte(got), []byte(expected)) != 1 {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
				"error": fiber.Map{"kind": "unauthorized", "message": "internal key 校验失败"},
			})
		}
		return c.Next()
	}
}
