package api

import (
	"fmt"

	"github.com/gofiber/fiber/v2"
	db "github.com/Books-QAQ/tickets/internal/db/sqlc"
	"github.com/Books-QAQ/tickets/internal/token"
	"github.com/Books-QAQ/tickets/internal/util"
	"github.com/redis/go-redis/v9"
)

type Server struct {
	Config     util.Config
	Store      *db.Store
	Redis      *redis.Client
	TokenMaker token.Maker
	App        *fiber.App
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
