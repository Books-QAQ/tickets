package api

import (
	"fmt"

	"github.com/gofiber/fiber/v2"
	db "github.com/Books-QAQ/tickets/internal/db/sqlc"
	"github.com/Books-QAQ/tickets/internal/payment"
	"github.com/Books-QAQ/tickets/internal/queue"
	"github.com/Books-QAQ/tickets/internal/token"
	"github.com/Books-QAQ/tickets/internal/util"
	"github.com/redis/go-redis/v9"
)

type Server struct {
	Config     util.Config
	Store      *db.Store
	Redis      *redis.Client
	TokenMaker token.Maker
	MQ         *queue.RabbitMQ
	App        *fiber.App
	// PaymentProviders 支付渠道列表（mock / alipay 等），由 main 注入。
	PaymentProviders []payment.Provider
	// CS 智能AI客服组件（M1），由 main 注入；为 nil 时不注册 /internal/*。
	CS *CSComponents
}

func NewServer(config util.Config, store *db.Store, redisClient *redis.Client) (*Server, error) {
	tokenMaker, err := token.NewJWTMaker(config.TOKENSECRETKEY)
	if err != nil {
		return nil, fmt.Errorf("cannot create token maker: %w", err)
	}

	app := fiber.New()

	server := &Server{
		Config:     config,
		Store:      store,
		Redis:      redisClient,
		TokenMaker: tokenMaker,
		App:        app,
	}

	return server, nil
}

func (server *Server) Start(address string) error {
	return server.App.Listen(fmt.Sprintf(":%s", address))
}
