package handlers

import (
	"net/http"
	"strconv"

	"github.com/gofiber/fiber/v2"

	db "github.com/Books-QAQ/tickets/internal/db/sqlc"
	"github.com/Books-QAQ/tickets/internal/token"
)

// CSHandler 智能AI客服的用户侧接口（公网，走 JWT）。
type CSHandler struct {
	store *db.Store
}

func NewCSHandler(store *db.Store) *CSHandler {
	return &CSHandler{store: store}
}

// GET /cs/support-tickets —— 用户查自己的客服工单（§10.4 V1 范围）
//
// 鉴权纪律（照抄「用户查自己对话记录」的写法）：
//   - **接口签名上没有 user_id 入参**，一律由服务端从 JWT 解析；
//   - SQL 强制 WHERE user_id = ?，因此换他人 token 必然查不到（横向越权在 SQL 层就不成立）；
//   - 管理端全量查询是另一套（ADMIN_TOKEN），不在本接口。
func (h *CSHandler) ListSupportTickets(c *fiber.Ctx) error {
	payload, ok := c.Locals("authorizationPayloadKey").(*token.Payload)
	if !ok || payload == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	user, err := h.store.GetUserByUsername(c.Context(), payload.Username)
	if err != nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "user not found"})
	}

	limit := 20
	if v := c.Query("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 100 {
			limit = n
		}
	}

	rows, err := h.store.ListSupportTicketsByUser(c.Context(), user.ID, limit)
	if err != nil {
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "failed to list support tickets"})
	}

	items := make([]fiber.Map, 0, len(rows))
	for _, t := range rows {
		items = append(items, fiber.Map{
			"ticket_no":  t.TicketNo,
			"category":   t.Category,
			"path":       t.Path,
			"status":     t.Status,
			"summary":    t.Summary,
			"created_at": t.CreatedAt,
		})
	}
	return c.JSON(fiber.Map{"tickets": items, "count": len(items)})
}
