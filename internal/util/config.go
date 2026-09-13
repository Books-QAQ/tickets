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
	OrderExpireDuration         time.Duration `mapstructure:"ORDER_EXPIRE_DURATION"`
	OrderExpireInterval         time.Duration `mapstructure:"ORDER_EXPIRE_INTERVAL"`
	RabbitMQURL                 string        `mapstructure:"RABBITMQ_URL"`
	TOKENSECRETKEY              string        `mapstructure:"TOKEN_SECRET_KEY"`
	AccessTokenDuration         time.Duration `mapstructure:"ACCESS_TOKEN_DURATION"`
	RefreshTokenDuration        time.Duration `mapstructure:"REFRESH_TOKEN_DURATION"`
	MigrationURL                string        `mapstructure:"MIGRATION_URL"`
	// 支付宝支付配置
	ALIPAYAPPID                 string `mapstructure:"ALIPAY_APP_ID"`
	ALIPAYPRIVATEKEY            string `mapstructure:"ALIPAY_PRIVATE_KEY"`
	ALIPAYPUBLICKEY             string `mapstructure:"ALIPAY_PUBLIC_KEY"`
	ALIPAYISPRODUCTION          bool   `mapstructure:"ALIPAY_IS_PRODUCTION"`
	ALIPAYNOTIFYURL             string `mapstructure:"ALIPAY_NOTIFY_URL"`
	ALIPAYRETURNURL             string `mapstructure:"ALIPAY_RETURN_URL"`
	// —— 智能AI客服（M1）——
	InternalKey      string  `mapstructure:"INTERNAL_KEY"`
	QdrantURL        string  `mapstructure:"QDRANT_URL"`
	QdrantAPIKey     string  `mapstructure:"QDRANT_API_KEY"`
	QdrantCollection string  `mapstructure:"QDRANT_COLLECTION"`
	EmbeddingProvider string `mapstructure:"EMBEDDING_PROVIDER"`
	EmbeddingBaseURL  string `mapstructure:"EMBEDDING_BASE_URL"`
	EmbeddingAPIKey   string `mapstructure:"EMBEDDING_API_KEY"`
	EmbeddingModel    string `mapstructure:"EMBEDDING_MODEL"`
	EmbeddingDim      int    `mapstructure:"EMBEDDING_DIM"`
	LLMProvider       string `mapstructure:"LLM_PROVIDER"`
	LLMBaseURL        string `mapstructure:"LLM_BASE_URL"`
	LLMAPIKey         string `mapstructure:"LLM_API_KEY"`
	LLMModel          string `mapstructure:"LLM_MODEL"`
	LLMMockOperational bool  `mapstructure:"LLM_MOCK_OPERATIONAL"`
	RerankProvider    string `mapstructure:"RERANK_PROVIDER"`
	RerankBaseURL     string `mapstructure:"RERANK_BASE_URL"`
	RerankAPIKey      string `mapstructure:"RERANK_API_KEY"`
	RerankModel       string `mapstructure:"RERANK_MODEL"`
	KBPath            string  `mapstructure:"KB_PATH"`
	ThresholdQA       float64 `mapstructure:"RELEVANCE_THRESHOLD_QA"`
	ThresholdProse    float64 `mapstructure:"RELEVANCE_THRESHOLD_PROSE"`
	LiteralThreshold  float64 `mapstructure:"LITERAL_THRESHOLD"`
	PythonBaseURL     string  `mapstructure:"PYTHON_BASE_URL"`
	// 工具层（M2）
	// PENALTY_SEMANTICS_CONFIRMED：19.2 的闸门，**默认 false**。
	// false 时 refund_fee 不下结论（宁可转人工不猜金额，§8.2）；业务确认语义后再置 1。
	PenaltySemanticsConfirmed bool `mapstructure:"PENALTY_SEMANTICS_CONFIRMED"`
	// GUEST_TICKET_ALLOWED：19.3 游客能否建单，默认 false（引导登录）
	GuestTicketAllowed bool `mapstructure:"GUEST_TICKET_ALLOWED"`
}

// IsSet 判断某个键是否被显式配置（文件或环境变量）。
// 用途：布尔开关有"未配置"与"显式关闭"之分，viper 取到的 false 分不清两者。
func IsSet(key string) bool { return viper.IsSet(key) }

// LoadConfig reads configuration from file or environment variables.
func LoadConfig(path string) (config Config, err error) {
	viper.AddConfigPath(path)
	viper.SetConfigName("app")
	viper.SetConfigType("env")

	viper.AutomaticEnv()

	// viper 的 AutomaticEnv 对 Unmarshal 并不可靠：只有配置文件里已存在的 key 才会被填充。
	// 客服相关配置（M1）要支持"只设环境变量也能生效"，必须显式 BindEnv。
	for _, key := range []string{
		"INTERNAL_KEY", "QDRANT_URL", "QDRANT_API_KEY", "QDRANT_COLLECTION",
		"EMBEDDING_PROVIDER", "EMBEDDING_BASE_URL", "EMBEDDING_API_KEY", "EMBEDDING_MODEL", "EMBEDDING_DIM",
		"LLM_PROVIDER", "LLM_BASE_URL", "LLM_API_KEY", "LLM_MODEL", "LLM_MOCK_OPERATIONAL",
		"RERANK_PROVIDER", "RERANK_BASE_URL", "RERANK_API_KEY", "RERANK_MODEL",
		"KB_PATH", "RELEVANCE_THRESHOLD_QA", "RELEVANCE_THRESHOLD_PROSE", "LITERAL_THRESHOLD",
		"PYTHON_BASE_URL",
		// 工具层（M2）
		"PENALTY_SEMANTICS_CONFIRMED", "GUEST_TICKET_ALLOWED",
	} {
		_ = viper.BindEnv(key)
	}

	err = viper.ReadInConfig()
	if err != nil {
		return
	}

	err = viper.Unmarshal(&config)
	return
}
