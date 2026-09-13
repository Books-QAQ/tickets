package middleware

import (
	"crypto/subtle"
	"strings"

	"github.com/gofiber/fiber/v2"
	"github.com/rs/zerolog/log"
)

// AdminAuthMiddleware 管理端鉴权（§16）：`Authorization: Bearer <ADMIN_TOKEN>`。
//
// 三条纪律：
//  1. **未配置 token 时拒绝启动**（在 main 里判定，见 `util.Config.AdminEnabled`）——
//     绝不能"配了管理端却默默放行"；
//  2. 比较用**恒定时间**（防时序侧信道）；
//  3. 失败一律 401 且**不回显任何配置信息**（不告诉调用者"token 没配"）。
func AdminAuthMiddleware(adminToken string) fiber.Handler {
	want := []byte(adminToken)
	return func(c *fiber.Ctx) error {
		if len(want) == 0 {
			// 走到这里说明装配有误：管理端已启用但 token 为空 → 拒绝而不是放行
			log.Error().Msg("管理端已启用但 ADMIN_TOKEN 为空，拒绝访问")
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
		}
		raw := c.Get("Authorization")
		if !strings.HasPrefix(strings.ToLower(raw), "bearer ") {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
		}
		got := []byte(strings.TrimSpace(raw[7:]))
		if subtle.ConstantTimeCompare(got, want) != 1 {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
		}
		return c.Next()
	}
}
