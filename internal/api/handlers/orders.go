package handlers

import (
	"net/http"
	"net/url"
	"time"

	"github.com/go-playground/validator/v10"
	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/Books-QAQ/tickets/internal/cache"
	db "github.com/Books-QAQ/tickets/internal/db/sqlc"
	"github.com/Books-QAQ/tickets/internal/payment"
	"github.com/Books-QAQ/tickets/internal/queue"
	"github.com/Books-QAQ/tickets/internal/token"
	"github.com/Books-QAQ/tickets/internal/util"
	"github.com/Books-QAQ/tickets/internal/worker"
	"github.com/rs/zerolog/log"
)

// OrderHandler 复用 TicketHandler 的座位校验、开售校验、鉴权等能力。
type OrderHandler struct {
	*TicketHandler
	mq       *queue.RabbitMQ
	payments map[string]payment.Provider
}

func NewOrderHandler(store *db.Store, redisClient *redis.Client, tokenMaker token.Maker, config util.Config, mq *queue.RabbitMQ, providers ...payment.Provider) *OrderHandler {
	payments := make(map[string]payment.Provider)
	for _, p := range providers {
		payments[p.Name()] = p
	}
	return &OrderHandler{
		TicketHandler: NewTicketHandler(store, redisClient, tokenMaker, config),
		mq:            mq,
		payments:      payments,
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

// PayOrder 发起支付：
//   - channel=mock（默认）：保持原行为，条件更新订单 pending→paid 并出票；
//   - channel=alipay：调用支付宝生成支付跳转 URL 返回，由异步回调/主动查单完成出票。
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

	// 快失败：订单已过期直接拒绝，不必等 DB 条件更新兜底
	if order.Status == "pending" && time.Now().After(order.ExpiredAt) {
		return c.Status(fiber.StatusGone).JSON(fiber.Map{"error": "order has expired"})
	}

	channel := req.Channel
	if channel == "" {
		channel = "mock"
	}

	// 渠道白名单：仅 mock / alipay 已实现；wechat 等未接入渠道直接拒绝，
	// 避免未识别渠道落入 mock 分支导致"下单即出票"。
	if channel != "mock" && channel != "alipay" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "unsupported payment channel: " + channel})
	}

	// 支付宝渠道：生成支付二维码，不出票
	if channel == "alipay" {
		provider, ok := h.payments["alipay"]
		if !ok {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "alipay channel not available"})
		}
		result, err := provider.CreatePayment(c.Context(), payment.Order{
			OrderNo: order.OrderNo,
			Amount:  order.Amount,
			Subject: "车票订单 " + order.OrderNo,
		}, h.config.ALIPAYNOTIFYURL, h.config.ALIPAYRETURNURL)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to create alipay payment: " + err.Error()})
		}
		// 关键：把渠道写回订单，后续主动查单（GetOrderStatus）据此走支付宝渠道，
		// 否则 pay_channel 为 NULL 会默认回 mock 渠道导致"下单即出票"。
		if err := h.store.SetOrderChannel(c.Context(), orderNo, "alipay"); err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to persist payment channel: " + err.Error()})
		}
		return c.Status(http.StatusOK).JSON(fiber.Map{
			"order_no": orderNo,
			"channel":  "alipay",
			"qr_code":  result.QrCode,
			"status":   order.Status,
		})
	}

	// mock 渠道：直接出票（保持原有行为）
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

// settlePaidOrder 在确认买家已付款后，调用 SettleOrderTx 完成出票（幂等）。
// 用 SettleOrderTx 而非 PayOrderTx：此时钱已真实扣除，即使订单恰好过期也必须出票。
// 供主动查单（GetOrderStatus）与异步回调（AlipayNotify）共用。
func (h *OrderHandler) settlePaidOrder(c *fiber.Ctx, orderNo string, channel string) (db.PayOrderTxResult, error) {
	order, err := h.store.GetOrderByNo(c.Context(), orderNo)
	if err != nil {
		return db.PayOrderTxResult{}, err
	}

	result, err := h.store.SettleOrderTx(c.Context(), db.PayOrderTxParams{
		OrderNo: orderNo,
		UserID:  order.UserID,
		BusID:   order.BusID,
		SeatID:  order.SeatID,
		Channel: channel,
	})
	if err != nil {
		return result, err
	}

	// 释放预占锁 + 失效线路缓存
	_ = cache.ReleaseSeatHold(c.Context(), h.redis, order.BusID, order.SeatID, worker.SeatHoldOwner(order.UserID, orderNo))
	h.invalidateRoutesCache(c.Context(), 0, order.BusID)

	return result, nil
}

// GetOrderStatus 查询订单支付状态：
// 若本地订单已 paid 直接返回；若 pending，则通过渠道主动查单，
// 查到已付款则结算出票（幂等）。前端付款后轮询此接口。
func (h *OrderHandler) GetOrderStatus(c *fiber.Ctx) error {
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

	// 已支付：直接返回
	if order.Status == "paid" {
		return c.Status(http.StatusOK).JSON(fiber.Map{"order_no": orderNo, "status": "paid"})
	}

	// 非 pending（已取消/已关闭）：直接返回现状
	if order.Status != "pending" {
		return c.Status(http.StatusOK).JSON(fiber.Map{"order_no": orderNo, "status": order.Status})
	}

	// pending：通过渠道主动查单
	channel := order.PayChannel.String
	if channel == "" {
		// 渠道未记录（下单后尚未发起支付）：保持 pending，绝不可默认 mock 出票
		return c.Status(http.StatusOK).JSON(fiber.Map{"order_no": orderNo, "status": order.Status})
	}
	provider, ok := h.payments[channel]
	if !ok {
		// 未知渠道：保持 pending
		return c.Status(http.StatusOK).JSON(fiber.Map{"order_no": orderNo, "status": order.Status})
	}

	paid, _, err := provider.QueryOrder(c.Context(), orderNo)
	if err != nil {
		// 查单失败（如支付宝未配置/网络抖动）：保持 pending，前端继续轮询
		return c.Status(http.StatusOK).JSON(fiber.Map{"order_no": orderNo, "status": order.Status})
	}
	if !paid {
		return c.Status(http.StatusOK).JSON(fiber.Map{"order_no": orderNo, "status": "pending"})
	}

	result, err := h.settlePaidOrder(c, orderNo, channel)
	if err != nil {
		// 补出票失败：若是支付宝已付款但订单已关，退款兜底
		if channel == "alipay" {
			h.refundPaidOrder(c, orderNo)
		}
		return c.Status(http.StatusConflict).JSON(fiber.Map{"error": err.Error()})
	}

	return c.Status(http.StatusOK).JSON(fiber.Map{
		"order_no":  orderNo,
		"status":    result.Status,
		"ticket_id": result.TicketID,
	})
}

// AlipayNotify 支付宝异步回调入口（无需登录，支付宝服务器直接 POST）。
// 验签通过且支付成功后结算出票。本地开发收不到回调时，靠 GetOrderStatus 主动查单兜底。
func (h *OrderHandler) AlipayNotify(c *fiber.Ctx) error {
	provider, ok := h.payments["alipay"]
	if !ok {
		return c.Status(fiber.StatusServiceUnavailable).SendString("fail")
	}

	values, err := url.ParseQuery(string(c.Body()))
	if err != nil {
		return c.Status(http.StatusBadRequest).SendString("fail")
	}

	orderNo, paid, err := provider.VerifyNotify(values)
	if err != nil {
		// 验签失败：拒绝处理
		return c.Status(http.StatusBadRequest).SendString("fail")
	}
	if !paid {
		return c.SendString("success")
	}

	// 补出票；若失败（典型：关单已先抢走订单，钱悬空）则退款兜底
	if _, err := h.settlePaidOrder(c, orderNo, "alipay"); err != nil {
		// 退款兜底：支付宝已扣款但订单无法出票，退还给买家
		h.refundPaidOrder(c, orderNo)
		return c.SendString("success")
	}

	return c.SendString("success")
}

// refundPaidOrder 退款兜底：支付宝已扣款但订单已关（钱悬空）时退给买家。
// 幂等由 provider.Refund（OutRequestNo=orderNo）保证，可安全重入。
func (h *OrderHandler) refundPaidOrder(c *fiber.Ctx, orderNo string) {
	order, err := h.store.GetOrderByNo(c.Context(), orderNo)
	if err != nil {
		log.Error().Err(err).Str("order_no", orderNo).Msg("refund: cannot load order")
		return
	}
	// 仅对已关单的订单退款；pending/paid 交由正常出票流程处理
	if order.Status != "canceled" {
		return
	}

	provider, ok := h.payments["alipay"]
	if !ok {
		log.Error().Str("order_no", orderNo).Msg("refund: alipay channel not available")
		return
	}
	if err := provider.Refund(c.Context(), orderNo, order.Amount, "订单超时关闭，自动退款"); err != nil {
		log.Error().Err(err).Str("order_no", orderNo).Msg("refund failed")
		return
	}
	log.Info().Str("order_no", orderNo).Int32("amount", order.Amount).Msg("refunded paid-but-canceled order")
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
