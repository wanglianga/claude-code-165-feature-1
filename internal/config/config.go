package config

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// Config 保存服务运行所需的全部配置，均通过环境变量注入。
type Config struct {
	HTTPAddr            string
	DatabaseURL         string
	MigrationsDir       string
	RedisAddr           string
	RedisPassword       string
	KafkaBrokers        []string
	KafkaTopic          string
	SeedDemo            bool
	TokenTTL            time.Duration
	ExpiryJobInterval   time.Duration
	ReservationTTL      time.Duration
	LargeReleaseRatio   float64 // 超过机构日接收能力该比例即视为“大批量临时释放”
	LargeReleaseMaxWait time.Duration
	// 冷链迟到降级判定参数
	DegradeAvgSpeedKmh     float64       // 城区冷链配送平均时速，估算剩余在途时间
	DegradeSafetyMargin    time.Duration // 送达受助点后须保留的最短安全食用窗口
	DegradeTempPenaltyPerC float64       // 现场温度每超存储上限 1℃ 的货架寿命折损系数
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envBool(key string, def bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}

func envDur(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def
	}
	return d
}

func envFloat(key string, def float64) float64 {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return def
	}
	return f
}

func Load() Config {
	var brokers []string
	if s := env("KAFKA_BROKERS", ""); s != "" {
		for _, b := range strings.Split(s, ",") {
			if b = strings.TrimSpace(b); b != "" {
				brokers = append(brokers, b)
			}
		}
	}
	return Config{
		HTTPAddr:               env("HTTP_ADDR", ":8080"),
		DatabaseURL:            env("DATABASE_URL", "postgres://postgres:postgres@localhost:5432/donation?sslmode=disable"),
		MigrationsDir:          env("MIGRATIONS_DIR", "migrations"),
		RedisAddr:              env("REDIS_ADDR", "localhost:6379"),
		RedisPassword:          env("REDIS_PASSWORD", ""),
		KafkaBrokers:           brokers,
		KafkaTopic:             env("KAFKA_TOPIC", "donation-events"),
		SeedDemo:               envBool("SEED_DEMO", true),
		TokenTTL:               envDur("TOKEN_TTL", 24*time.Hour),
		ExpiryJobInterval:      envDur("EXPIRY_JOB_INTERVAL", 30*time.Second),
		ReservationTTL:         envDur("RESERVATION_TTL", 30*time.Minute),
		LargeReleaseRatio:      0.5,
		LargeReleaseMaxWait:    envDur("LARGE_RELEASE_MAX_WAIT", 3*time.Hour),
		DegradeAvgSpeedKmh:     envFloat("DEGRADE_AVG_SPEED_KMH", 30),
		DegradeSafetyMargin:    envDur("DEGRADE_SAFETY_MARGIN", 2*time.Hour),
		DegradeTempPenaltyPerC: envFloat("DEGRADE_TEMP_PENALTY_PER_C", 0.12),
	}
}
