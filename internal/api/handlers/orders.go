package handlers

import (
	"net/http"
	"time"

	"github.com/go-playground/validator/v10"
	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/Books-QAQ/tickets/internal/cache"
	db "github.com/Books-QAQ/tickets/internal/db/sqlc"
	"github.com/Books-QAQ/tickets/internal/queue"
	"github.com/Books-QAQ/tickets/internal/token"
	"github.com/Books-QAQ/tickets/internal/util"
	"github.com/Books-QAQ/tickets/internal/worker"
	"github.com/rs/zerolog/log"
)

// OrderHandler 复用 TicketHandler 的座位校验、开售校验、鉴权等能力。
type OrderHandler struct {
	*TicketHandler
	mq *queue.RabbitMQ
}

func NewOrderHandler(store *db.Store, redisClient *redis.Client, tokenMaker token.Maker, config util.Config, mq *queue.RabbitMQ) *OrderHandler {
	return &OrderHandler{
		TicketHandler: NewTicketHandler(store, redisClient, tokenMaker, config),
		mq:            mq,
	}
}

type createOrderResponse struct {
	OrderNo   string    `json:"order_no"`
	Amount    int32     `json:"amount"`
	Status    string    `json:"status"`
	ExpiresAt time.Time `json:"expires_at"`
}

type payOrderRequest struct {
	Channel string `json:"channel"` // 可选，模拟支付渠道
}

func (h *OrderHandler) orderExpireTTL() time.Duration {
	if h.config.OrderExpireDuration > 0 {
		return h.config.OrderExpireDuration
	}
	return 15 * time.Minute
}

// CreateOrder 下单：校验 → 生成订单号 → Redis 预占锁（占座）→ 创建 pending 订单。
func (h *OrderHandler) CreateOrder(c *fiber.Ctx) error {
	var req reserveSeatRequest
	if err := c.BodyParser(&req); err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "Invalid request parameters"})
	}

	validate := validator.New()
	if err := validate.Struct(&req); err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": err.Error()})
	}

	if err := h.validateSeatRequest(c, req); err != nil {
		return err
	}
	if err := h.ensureBusOnSale(c, req.BusID); err != nil {
		return err
	}

	user, err := h.currentUser(c)
	if err != nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": err.Error()})
	}

	bus, err := h.store.GetBusByID(c.Context(), req.BusID)
	if err != nil {
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "Failed to fetch bus"})
	}

	orderNo := uuid.NewString()
	holdTTL := h.orderExpireTTL()
	owner := worker.SeatHoldOwner(user.ID, orderNo)

	// ① Redis 锁：快速分流（降级为加速层，不承担正确性）
	// 锁被占 → 快速 409；锁服务异常 → 降级放行，交给 MySQL 条件更新兜底
	claimed, err := cache.AcquireSeatHold(c.Context(), h.redis, req.BusID, req.SeatID, owner, holdTTL)
	if err == nil && !claimed {
		return c.Status(fiber.StatusConflict).JSON(fiber.Map{"error": "seat is temporarily held by another request"})
	}

	// ② MySQL 事务：座位 available→reserved + 创建订单（真相裁决）
	order, err := h.store.CreateOrderTx(c.Context(), db.CreateOrderTxParams{
		OrderNo:   orderNo,
		UserID:    user.ID,
		BusID:     req.BusID,
		SeatID:    req.SeatID,
		Amount:    bus.Price,
		ExpiredAt: time.Now().Add(holdTTL),
	})
	if err != nil {
		_ = cache.ReleaseSeatHold(c.Context(), h.redis, req.BusID, req.SeatID, owner)
		return c.Status(fiber.StatusConflict).JSON(fiber.Map{"error": "seat is no longer available"})
	}

	// ③ 发布关单延迟消息到 DLX+TTL（失败则降级，靠兜底扫描关单）
	if h.mq != nil {
		if err := h.mq.PublishOrderExpiry(c.Context(), queue.OrderExpiryMessage{OrderNo: order.OrderNo}); err != nil {
			log.Error().Err(err).Str("order_no", order.OrderNo).Msg("failed to publish order expiry message")
		}
	}

	return c.Status(http.StatusCreated).JSON(createOrderResponse{
		OrderNo:   order.OrderNo,
		Amount:    order.Amount,
		Status:    order.Status,
		ExpiresAt: order.ExpiredAt,
	})
}

// PayOrder 模拟支付：条件更新订单 pending→paid（幂等），并出票。
func (h *OrderHandler) PayOrder(c *fiber.Ctx) error {
	orderNo := c.Params("orderNo")
	if orderNo == "" {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "missing order_no"})
	}

	var req payOrderRequest
	_ = c.BodyParser(&req) // channel 可选

	user, err := h.currentUser(c)
	if err != nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": err.Error()})
	}

	order, err := h.store.GetOrderByNo(c.Context(), orderNo)
	if err != nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "order not found"})
	}
	if order.UserID != user.ID {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "not your order"})
	}

	channel := req.Channel
	if channel == "" {
		channel = "mock"
	}

	result, err := h.store.PayOrderTx(c.Context(), db.PayOrderTxParams{
		OrderNo: orderNo,
		UserID:  user.ID,
		BusID:   order.BusID,
		SeatID:  order.SeatID,
		Channel: channel,
	})
	if err != nil {
		return c.Status(fiber.StatusConflict).JSON(fiber.Map{"error": err.Error()})
	}

	// 释放预占锁 + 失效线路缓存
	_ = cache.ReleaseSeatHold(c.Context(), h.redis, order.BusID, order.SeatID, worker.SeatHoldOwner(user.ID, orderNo))
	h.invalidateRoutesCache(c.Context(), 0, order.BusID)

	return c.Status(http.StatusOK).JSON(fiber.Map{
		"order_no":  orderNo,
		"status":    result.Status,
		"ticket_id": result.TicketID,
	})
}

// GetOrder 查询订单状态。
func (h *OrderHandler) GetOrder(c *fiber.Ctx) error {
	orderNo := c.Params("orderNo")
	if orderNo == "" {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "missing order_no"})
	}

	user, err := h.currentUser(c)
	if err != nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": err.Error()})
	}

	order, err := h.store.GetOrderByNo(c.Context(), orderNo)
	if err != nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "order not found"})
	}
	if order.UserID != user.ID {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "not your order"})
	}

	return c.Status(http.StatusOK).JSON(order)
}
