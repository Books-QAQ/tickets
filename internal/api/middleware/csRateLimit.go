package middleware

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/redis/go-redis/v9"

	"github.com/Books-QAQ/tickets/internal/ai/kb"
	"github.com/Books-QAQ/tickets/internal/cache"
)

// CSRateLimitConfig AI 链路限流参数。
//
// 口径（§14.5）：**限流值是"成本保护值"，不是容量上限** ——
// 一条 AI 问答会花掉多次 LLM/embedding 调用，所以它比普通接口更该限流；
// 具体数字由压测后调整，报告里必须写明口径（当前默认值见 DefaultCSRateLimit）。
type CSRateLimitConfig struct {
	Window  time.Duration
	MaxIP   int64
	MaxUser int64
}

// DefaultCSRateLimit 默认值：IP 桶 30/min、身份桶 20/min（成本保护值）
func DefaultCSRateLimit() CSRateLimitConfig {
	return CSRateLimitConfig{Window: time.Minute, MaxIP: 30, MaxUser: 20}
}

// CSRateLimitMiddleware AI 链路双闸限流（IP 桶 + 身份桶）。
//
// "身份桶"用 **凭证的 hash** 做键，而不是解析 JWT 后的 user_id：
//   - 中间件在路由层运行，比 handler 里的身份解析更早，拿不到 user_id；
//   - 用凭证 hash 同样能做到"同一登录用户一个桶"，且对游客天然退化成设备级；
//   - hash 而不是明文 token：Redis 里不出现可用凭证。
//
// **Redis 不可用时放行**（§14.4：Redis 挂了不该把问答打死，只记降级计数）。
func CSRateLimitMiddleware(rdb *redis.Client, cfg CSRateLimitConfig, cnt Counter) fiber.Handler {
	if cfg.Window <= 0 {
		cfg = DefaultCSRateLimit()
	}
	return func(c *fiber.Ctx) error {
		if rdb == nil {
			return c.Next()
		}
		// IP 桶
		allowed, retry, err := cache.AllowFixedWindow(c.Context(), rdb, "rate:cs:ip:"+c.IP(), cfg.MaxIP, cfg.Window)
		if err != nil {
			cnt.Inc("cs_rate_limit_degraded_total") // Redis 异常 → 放行但要看得见
			return c.Next()
		}
		if !allowed {
			cnt.Inc("cs_rate_limited_total:ip")
			return tooMany(c, retry)
		}
		// 身份桶（凭证 hash）
		if raw := c.Get("Authorization"); len(raw) > 16 {
			sum := sha256.Sum256([]byte(raw))
			key := "rate:cs:cred:" + hex.EncodeToString(sum[:8])
			allowed, retry, err = cache.AllowFixedWindow(c.Context(), rdb, key, cfg.MaxUser, cfg.Window)
			if err == nil && !allowed {
				cnt.Inc("cs_rate_limited_total:cred")
				return tooMany(c, retry)
			}
		}
		return c.Next()
	}
}

func tooMany(c *fiber.Ctx, retry time.Duration) error {
	sec := int(retry.Seconds())
	if sec < 1 {
		sec = 1
	}
	c.Set("Retry-After", strconv.Itoa(sec))
	return c.Status(fiber.StatusTooManyRequests).JSON(fiber.Map{
		"error": "请求过于频繁，请稍后再试（AI 问答限流为成本保护，非容量上限）",
	})
}

// Counter 最小计数接口（由 kb.Aux 实现，保持中间件不依赖具体实现）
type Counter interface{ Inc(key string) }

var _ Counter = (*kb.Aux)(nil)
