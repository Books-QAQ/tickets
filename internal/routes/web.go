package routes

import (
	"github.com/gofiber/fiber/v2"
	"github.com/Books-QAQ/tickets/internal/api"
	"github.com/Books-QAQ/tickets/internal/api/handlers"
	"github.com/Books-QAQ/tickets/internal/api/middleware"
)

func SetupRoutes(server *api.Server) error {
	server.App.Static("/photos", "./web/photos")
	server.App.Static("/assets", "./web/assets")

	server.App.Get("/", func(c *fiber.Ctx) error {
		return c.SendFile("./web/index.html")
	})
	server.App.Get("/login", func(c *fiber.Ctx) error {
		return c.SendFile("./web/login.html")
	})
	server.App.Get("/register", func(c *fiber.Ctx) error {
		return c.SendFile("./web/register.html")
	})
	server.App.Get("/booking", func(c *fiber.Ctx) error {
		return c.SendFile("./web/booking.html")
	})
	server.App.Get("/profile", func(c *fiber.Ctx) error {
		return c.SendFile("./web/profile.html")
	})

	server.App.Post("/register", handlers.NewUserHandler(server.Store, server.Redis, server.TokenMaker, server.Config).RegisterUser)
	server.App.Post("/login", handlers.NewUserHandler(server.Store, server.Redis, server.TokenMaker, server.Config).LoginUser)
	server.App.Post("/tokens/renew_access", handlers.NewTokenHandler(server.Store, server.TokenMaker, server.Config).RenewAccessToken)
	server.App.Get("/cities", handlers.NewCityHandler(server.Store, server.Redis, server.TokenMaker, server.Config).ListCities)
	server.App.Get("/terminals", handlers.NewTerminalHandler(server.Store, server.Redis, server.TokenMaker, server.Config).ListTerminals)
	server.App.Get("/routes", handlers.NewRouteHandler(server.Store, server.Redis, server.TokenMaker, server.Config).SearchRoutes)
	server.App.Get("/routes/:route_id/buses/:bus_id/seats", handlers.NewBusHandler(server.Store, server.TokenMaker, server.Config).ListAvailableSeats)

	authGroup := server.App.Group("/", middleware.AuthMiddleware(server.TokenMaker))
	purchaseLimiter := middleware.NewPurchaseRateLimiter(server.Redis, server.Config)
	authGroup.Get("/user/info", handlers.NewUserHandler(server.Store, server.Redis, server.TokenMaker, server.Config).GetUserProfile)
	authGroup.Put("/user/update", handlers.NewUserHandler(server.Store, server.Redis, server.TokenMaker, server.Config).UpdateUserProfile)
	authGroup.Post("/user/password_change", handlers.NewUserHandler(server.Store, server.Redis, server.TokenMaker, server.Config).ChangePassword)
	authGroup.Get("/user/tickets", handlers.NewTicketHandler(server.Store, server.Redis, server.TokenMaker, server.Config).ListUserTickets)
	authGroup.Post("/routes/reserve", purchaseLimiter, handlers.NewTicketHandler(server.Store, server.Redis, server.TokenMaker, server.Config).ReserveSeat)
	authGroup.Post("/routes/purchase", purchaseLimiter, handlers.NewTicketHandler(server.Store, server.Redis, server.TokenMaker, server.Config).PurchaseTicket)
	authGroup.Post("/routes/purchase_async", purchaseLimiter, handlers.NewTicketHandler(server.Store, server.Redis, server.TokenMaker, server.Config).PurchaseTicketAsync)
	authGroup.Get("/purchase-tasks/:id", handlers.NewTicketHandler(server.Store, server.Redis, server.TokenMaker, server.Config).GetPurchaseTaskStatus)
	authGroup.Post("/orders", handlers.NewOrderHandler(server.Store, server.Redis, server.TokenMaker, server.Config, server.MQ).CreateOrder)
	authGroup.Get("/orders/:orderNo", handlers.NewOrderHandler(server.Store, server.Redis, server.TokenMaker, server.Config, server.MQ).GetOrder)
	authGroup.Post("/orders/:orderNo/pay", handlers.NewOrderHandler(server.Store, server.Redis, server.TokenMaker, server.Config, server.MQ).PayOrder)
	authGroup.Get("/routes/reserve", purchaseLimiter, handlers.NewTicketHandler(server.Store, server.Redis, server.TokenMaker, server.Config).ReserveSeat)
	authGroup.Get("/routes/purchase", purchaseLimiter, handlers.NewTicketHandler(server.Store, server.Redis, server.TokenMaker, server.Config).PurchaseTicket)
	authGroup.Delete("/tickets/:id", handlers.NewTicketHandler(server.Store, server.Redis, server.TokenMaker, server.Config).CancelTicket)
	return nil
}
