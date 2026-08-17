package main

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/mysql"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/Books-QAQ/tickets/internal/api"
	"github.com/Books-QAQ/tickets/internal/bootstrap"
	"github.com/Books-QAQ/tickets/internal/cache"
	db "github.com/Books-QAQ/tickets/internal/db/sqlc"
	"github.com/Books-QAQ/tickets/internal/queue"
	"github.com/Books-QAQ/tickets/internal/routes"
	"github.com/Books-QAQ/tickets/internal/util"
	"github.com/Books-QAQ/tickets/internal/worker"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

func main() {
	config, err := util.LoadConfig(".")
	if err != nil {
		log.Fatal().Err(err).Msg("cannot load config")
	}

	if config.APPDEBUG == "true" {
		log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr})
	}

	dsn := fmt.Sprintf("%s:%s@tcp(%s:%s)/%s?parseTime=true&multiStatements=true",
		config.DBUSERNAME,
		config.DBPASSWORD,
		config.DBHOST,
		config.DBPORT,
		config.DBDATABASE,
	)
	dsn = dsn + "&loc=Asia%2FShanghai"

	dbConn, err := sql.Open("mysql", dsn)
	if err != nil {
		log.Fatal().Err(err).Msg("cannot connect to db")
	}
	defer dbConn.Close()

	if err := dbConn.Ping(); err != nil {
		log.Fatal().Err(err).Msg("cannot ping db")
	}

	migrateDSN := fmt.Sprintf(
		"mysql://%s:%s@tcp(%s:%s)/%s?multiStatements=true",
		url.QueryEscape(config.DBUSERNAME),
		url.QueryEscape(config.DBPASSWORD),
		config.DBHOST,
		config.DBPORT,
		config.DBDATABASE,
	)

	runDBMigration(config.MigrationURL, migrateDSN)

	store := db.NewStore(dbConn)

	redisClient, err := cache.NewRedisClient(config)
	if err != nil {
		log.Fatal().Err(err).Msg("cannot connect to redis")
	}
	defer redisClient.Close()

	// 连接 RabbitMQ（DLX+TTL 延迟队列），失败则降级为 nil，靠兜底扫描关单
	var mq *queue.RabbitMQ
	if config.RabbitMQURL != "" {
		mq, err = queue.NewRabbitMQ(config.RabbitMQURL)
		if err != nil {
			log.Error().Err(err).Msg("cannot connect to rabbitmq, fallback to periodic scan")
			mq = nil
		} else {
			defer mq.Close()
			expireTTL := config.OrderExpireDuration
			if expireTTL <= 0 {
				expireTTL = 15 * time.Minute
			}
			if err := mq.SetupOrderExpiry(expireTTL); err != nil {
				log.Error().Err(err).Msg("cannot setup order expiry queue, fallback to periodic scan")
			}
		}
	}

	server, err := api.NewServer(config, store, redisClient)
	if err != nil {
		log.Fatal().Err(err).Msg("cannot create server")
	}
	server.MQ = mq

	appCtx := context.Background()

	if err := bootstrap.EnsureDemoData(appCtx, dbConn); err != nil {
		log.Fatal().Err(err).Msg("cannot seed demo data")
	}

	bootstrap.StartDemoDataScheduler(appCtx, dbConn)
	worker.StartPurchaseWorker(appCtx, store, redisClient, config)
	if mq != nil {
		worker.StartOrderExpiryConsumer(appCtx, store, mq)
	}
	worker.StartOrderExpiryFallbackScanner(appCtx, store, config)

	if err := routes.SetupRoutes(server); err != nil {
		log.Fatal().Err(err).Msg("failed to set up routes")
	}

	if err := server.Start(config.APPPORT); err != nil {
		log.Fatal().Err(err).Msg("cannot start server")
	}
}

func runDBMigration(migrationURL string, dbSource string) {
	migration, err := migrate.New(migrationURL, dbSource)
	if err != nil {
		log.Fatal().Err(err).Msg("cannot create new migrate instance")
	}

	if err = migration.Up(); err != nil && err != migrate.ErrNoChange {
		log.Fatal().Err(err).Msg("failed to run migrate up")
	}

	log.Info().Msg("db migrated successfully")
}
