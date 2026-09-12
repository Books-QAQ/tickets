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
	"github.com/Books-QAQ/tickets/internal/payment"
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

	// 智能AI客服组件（M1）：任一依赖不可用时**降级**而不是启动失败（降级在响应里可见）
	csComponents, err := api.BuildCSComponents(dbConn, config)
	if err != nil {
		log.Error().Err(err).Msg("cannot build AI customer-service components; /internal/* disabled")
		csComponents = nil
	}

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
	server.WithCS(csComponents)

	// 注入支付渠道：mock（始终可用）+ 支付宝（未配置密钥时降级禁用）
	mockProvider := payment.NewMockProvider()
	alipayProvider, err := payment.NewAlipayProvider(payment.AlipayConfig{
		AppID:          config.ALIPAYAPPID,
		PrivateKey:     config.ALIPAYPRIVATEKEY,
		AlipayPubKey:   config.ALIPAYPUBLICKEY,
		IsProduction:   config.ALIPAYISPRODUCTION,
		TimeoutExpress: orderExpireToAlipay(config.OrderExpireDuration),
	})
	if err != nil {
		log.Error().Err(err).Msg("cannot init alipay provider, alipay channel disabled")
		alipayProvider, _ = payment.NewAlipayProvider(payment.AlipayConfig{})
	}
	server.PaymentProviders = []payment.Provider{mockProvider, alipayProvider}

	appCtx := context.Background()

	if err := bootstrap.EnsureDemoData(appCtx, dbConn); err != nil {
		log.Fatal().Err(err).Msg("cannot seed demo data")
	}

	bootstrap.StartDemoDataScheduler(appCtx, dbConn)
	if mq != nil {
		worker.StartOrderExpiryConsumer(appCtx, store, mq, alipayProvider)
	}
	worker.StartOrderExpiryFallbackScanner(appCtx, store, config, alipayProvider)

	if err := routes.SetupRoutes(server); err != nil {
		log.Fatal().Err(err).Msg("failed to set up routes")
	}

	if err := server.Start(config.APPPORT); err != nil {
		log.Fatal().Err(err).Msg("cannot start server")
	}
}

// orderExpireToAlipay 将订单过期时长换算成支付宝 timeout_express 格式。
// 支付宝要求：1m～15d，m-分钟、h-小时、d-天，不接受小数点。
// 我们统一用分钟表示（如 15m），与订单 expired_at 对齐。
func orderExpireToAlipay(d time.Duration) string {
	if d <= 0 {
		return ""
	}
	minutes := int64(d.Minutes())
	if minutes < 1 {
		minutes = 1 // 支付宝最小 1m
	}
	if minutes > 15*24*60 {
		return "15d" // 支付宝最大 15d
	}
	return fmt.Sprintf("%dm", minutes)
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
