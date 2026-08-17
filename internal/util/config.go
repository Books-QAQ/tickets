package util

import (
	"time"

	"github.com/spf13/viper"
)

// Config stores all configuration of the application.
// The values are read by viper from a config file or environment variable.
type Config struct {
	APPPORT                     string        `mapstructure:"APP_PORT"`
	APPNAME                     string        `mapstructure:"APP_NAME"`
	APPDEBUG                    string        `mapstructure:"APP_DEBUG"`
	DBCONNECTION                string        `mapstructure:"DB_CONNECTION"`
	DBHOST                      string        `mapstructure:"DB_HOST"`
	DBPORT                      string        `mapstructure:"DB_PORT"`
	DBDATABASE                  string        `mapstructure:"DB_DATABASE"`
	DBUSERNAME                  string        `mapstructure:"DB_USERNAME"`
	DBPASSWORD                  string        `mapstructure:"DB_PASSWORD"`
	REDISHOST                   string        `mapstructure:"REDIS_HOST"`
	REDISPORT                   string        `mapstructure:"REDIS_PORT"`
	REDISPASSWORD               string        `mapstructure:"REDIS_PASSWORD"`
	REDISDB                     int           `mapstructure:"REDIS_DB"`
	LoginRateLimitWindow        time.Duration `mapstructure:"LOGIN_RATE_LIMIT_WINDOW"`
	LoginRateLimitMaxIP         int64         `mapstructure:"LOGIN_RATE_LIMIT_MAX_IP"`
	LoginRateLimitMaxUser       int64         `mapstructure:"LOGIN_RATE_LIMIT_MAX_USER"`
	PurchaseRateLimitCapacity   int64         `mapstructure:"PURCHASE_RATE_LIMIT_CAPACITY"`
	PurchaseRateLimitRefillRate float64       `mapstructure:"PURCHASE_RATE_LIMIT_REFILL_RATE"`
	SeatHoldTTL                 time.Duration `mapstructure:"SEAT_HOLD_TTL"`
	PurchaseTaskTTL             time.Duration `mapstructure:"PURCHASE_TASK_TTL"`
	PurchaseWorkerCount         int           `mapstructure:"PURCHASE_WORKER_COUNT"`
	OrderExpireDuration         time.Duration `mapstructure:"ORDER_EXPIRE_DURATION"`
	OrderExpireInterval         time.Duration `mapstructure:"ORDER_EXPIRE_INTERVAL"`
	RabbitMQURL                 string        `mapstructure:"RABBITMQ_URL"`
	TOKENSECRETKEY              string        `mapstructure:"TOKEN_SECRET_KEY"`
	AccessTokenDuration         time.Duration `mapstructure:"ACCESS_TOKEN_DURATION"`
	RefreshTokenDuration        time.Duration `mapstructure:"REFRESH_TOKEN_DURATION"`
	MigrationURL                string        `mapstructure:"MIGRATION_URL"`
}

// LoadConfig reads configuration from file or environment variables.
func LoadConfig(path string) (config Config, err error) {
	viper.AddConfigPath(path)
	viper.SetConfigName("app")
	viper.SetConfigType("env")

	viper.AutomaticEnv()

	err = viper.ReadInConfig()
	if err != nil {
		return
	}

	err = viper.Unmarshal(&config)
	return
}
