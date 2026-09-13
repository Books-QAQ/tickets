package routes

import (
	"github.com/gofiber/fiber/v2"
	"github.com/Books-QAQ/tickets/internal/api"
	"github.com/Books-QAQ/tickets/internal/api/handlers"
	"github.com/Books-QAQ/tickets/internal/api/middleware"
)

func SetupRoutes(server *api.Server) error {
	server.App.Static("/photos", "./web/photos")
	// 静态资源禁止缓存，避免前端 JS 改动后浏览器仍加载旧版本
	server.App.Static("/assets", "./web/assets", fiber.Static{
		CacheDuration: -1,
	})

	server.App.Get("/", func(c *fiber.Ctx) error {
		c.Set("Cache-Control", "no-cache, no-store, must-revalidate")
		return c.SendFile("./web/index.html")
	})
	server.App.Get("/login", func(c *fiber.Ctx) error {
		c.Set("Cache-Control", "no-cache, no-store, must-revalidate")
		return c.SendFile("./web/login.html")
	})
	server.App.Get("/register", func(c *fiber.Ctx) error {
		c.Set("Cache-Control", "no-cache, no-store, must-revalidate")
		return c.SendFile("./web/register.html")
	})
	server.App.Get("/booking", func(c *fiber.Ctx) error {
		c.Set("Cache-Control", "no-cache, no-store, must-revalidate")
		return c.SendFile("./web/booking.html")
	})
	server.App.Get("/profile", func(c *fiber.Ctx) error {
		c.Set("Cache-Control", "no-cache, no-store, must-revalidate")
		return c.SendFile("./web/profile.html")
	})

	server.App.Post("/register", handlers.NewUserHandler(server.Store, server.Redis, server.TokenMaker, server.Config).RegisterUser)
	server.App.Post("/login", handlers.NewUserHandler(server.Store, server.Redis, server.TokenMaker, server.Config).LoginUser)
	server.App.Post("/tokens/renew_access", handlers.NewTokenHandler(server.Store, server.TokenMaker, server.Config).RenewAccessToken)
	server.App.Post("/pay/alipay/notify", handlers.NewOrderHandler(server.Store, server.Redis, server.TokenMaker, server.Config, server.MQ, server.PaymentProviders...).AlipayNotify)
	server.App.Get("/cities", handlers.NewCityHandler(server.Store, server.Redis, server.TokenMaker, server.Config).ListCities)
	server.App.Get("/terminals", handlers.NewTerminalHandler(server.Store, server.Redis, server.TokenMaker, server.Config).ListTerminals)
	server.App.Get("/routes", handlers.NewRouteHandler(server.Store, server.Redis, server.TokenMaker, server.Config).SearchRoutes)
	server.App.Get("/routes/:route_id/buses/:bus_id/seats", handlers.NewBusHandler(server.Store, server.TokenMaker, server.Config).ListAvailableSeats)
	// —— 智能AI客服：公网入口（§12.1）——
	// 同样**必须在 authGroup 之前注册**：/cs/ask 允许游客使用（JWT 可选），
	// 放到 authGroup 之后会被 JWT 中间件拦成 401（M1 已在 /internal/* 上踩过一次）。
	// 身份解析在 handler 内做：有 Bearer 就解析，没有就是游客（device_id 只存 hash）。
	var csAux handlers.Counter
	if server.CS != nil {
		csAux = server.CS.Aux
	}
	csHandler := handlers.NewCSHandler(server.Store, server.Redis, server.TokenMaker, server.Config, csAux)
	server.App.Post("/cs/ask", csHandler.Ask)
	server.App.Delete("/cs/session", csHandler.DeleteSession)
	server.App.Post("/cs/feedback", csHandler.PostFeedback)
	server.App.Post("/cs/support-tickets", csHandler.PostSupportTicket)

	// —— 智能AI客服：跨语言契约（仅内网 + 内网密钥，§6.1）——
	// 注意：**必须注册在下面的 authGroup 之前**。Fiber 的 group 中间件作用于其后注册的路由，
	// 若放在 authGroup（prefix "/" + JWT 校验）之后，/internal/* 会先被 JWT 中间件拦成 401。
	// 组件装配失败（server.CS == nil）时不注册，避免半残状态被调用。
	if server.CS != nil && server.CS.KB != nil {
		ih := handlers.NewInternalHandler(server.CS.KB, server.CS.Retriever, server.CS.LLM, server.CS.Aux, server.CS.Tools, server.CS.Cache, server.CS.KB.DB)
		internal := server.App.Group("/internal", middleware.InternalKeyMiddleware(server.Config.InternalKey))
		internal.Post("/cache/lookup", ih.CacheLookup)
		internal.Post("/cache/store", ih.CacheStore)
		internal.Post("/classify/pre-intent", ih.PreIntent)
		internal.Post("/classify/rule", ih.ClassifyRule)
		internal.Post("/retrieve", ih.Retrieve)
		internal.Post("/tools/route", ih.RouteTool)
		internal.Post("/tools/:name", ih.ExecTool)
		internal.Post("/citation/verify", ih.VerifyCitation)
		internal.Post("/support-tickets", ih.CreateSupportTicket)
		internal.Post("/session/turns", ih.RecordTurns)
		internal.Post("/metrics", ih.RecordMetrics)
		internal.Get("/metrics/snapshot", ih.MetricsSnapshot)
		internal.Post("/v1/chat/completions", ih.ChatCompletions)
	}

	authGroup := server.App.Group("/", middleware.AuthMiddleware(server.TokenMaker))
	authGroup.Get("/user/info", handlers.NewUserHandler(server.Store, server.Redis, server.TokenMaker, server.Config).GetUserProfile)
	authGroup.Put("/user/update", handlers.NewUserHandler(server.Store, server.Redis, server.TokenMaker, server.Config).UpdateUserProfile)
	authGroup.Post("/user/password_change", handlers.NewUserHandler(server.Store, server.Redis, server.TokenMaker, server.Config).ChangePassword)
	authGroup.Get("/user/tickets", handlers.NewTicketHandler(server.Store, server.Redis, server.TokenMaker, server.Config).ListUserTickets)
	authGroup.Post("/orders", handlers.NewOrderHandler(server.Store, server.Redis, server.TokenMaker, server.Config, server.MQ, server.PaymentProviders...).CreateOrder)
	authGroup.Get("/orders/:orderNo", handlers.NewOrderHandler(server.Store, server.Redis, server.TokenMaker, server.Config, server.MQ, server.PaymentProviders...).GetOrder)
	authGroup.Post("/orders/:orderNo/pay", handlers.NewOrderHandler(server.Store, server.Redis, server.TokenMaker, server.Config, server.MQ, server.PaymentProviders...).PayOrder)
	authGroup.Get("/orders/:orderNo/status", handlers.NewOrderHandler(server.Store, server.Redis, server.TokenMaker, server.Config, server.MQ, server.PaymentProviders...).GetOrderStatus)
	authGroup.Delete("/tickets/:id", handlers.NewTicketHandler(server.Store, server.Redis, server.TokenMaker, server.Config).CancelTicket)

	// 智能AI客服：用户查自己的工单与会话（§10.4 / §12.1；user_id 只来自服务端 JWT 解析）
	authGroup.Get("/cs/support-tickets", csHandler.ListSupportTickets)
	authGroup.Get("/cs/conversations", csHandler.ListConversations)
	authGroup.Get("/cs/conversations/:id/messages", csHandler.ListMessages)
	return nil
}
