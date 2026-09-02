package config

// Package config loads application configuration from environment variables.
// The 12-factor pattern mirrors audita-v1: every setting is env-var driven.

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds the full runtime configuration for the audita-api monolith.
type Config struct {
	// HTTP server
	Port            string
	BaseURL         string
	CORSAllowedOris []string

	// Database
	DatabaseURL string

	// NATS
	NatsURL string

	// Auth
	JWTPrivateKeyPEM    string
	JWTPublicKeyPEM     string
	JWTAccessTTL        time.Duration
	RefreshTokenTTL     time.Duration
	RefreshCookieSecure bool
	AuthCookieSecure    bool

	// Encryption (AES-GCM envelope key, base64-encoded 32-byte key)
	AppEncryptionKey string

	// Tenant bootstrap
	BootstrapSetupToken string

	// SMTP
	SMTPHost     string
	SMTPPort     int
	SMTPUsername string
	SMTPPassword string
	SMTPFrom     string
	SMTPFromName string
	SMTPAuth     bool
	SMTPTLS      bool

	// Storage
	StorageBackend        string // "local" | "s3"
	StorageLocalBasePath  string
	StorageS3Bucket       string
	StorageS3Region       string
	StorageS3AccessKey    string
	StorageS3SecretKey    string
	UploadMaxSizeBytes    int64
	UploadAllowedMIMEType []string

	// Invites
	InviteExpiryHours int

	// Observability
	OTLPEndpoint             string
	OTLPSamplingProb         float64
	LogLevel                 string
	MetricsEnabled           bool

	// SLA
	SLAWarnHoursBefore int
}

// Load reads configuration from the environment, applying defaults.
func Load() (*Config, error) {
	c := &Config{
		Port:                  getEnv("PORT", "8080"),
		BaseURL:               getEnv("APP_BASE_URL", "http://localhost:7000"),
		CORSAllowedOris:       getEnvSlice("CORS_ALLOWED_ORIGINS", []string{"http://localhost:7000"}),
		DatabaseURL:           getEnv("DATABASE_URL", ""),
		NatsURL:               getEnv("NATS_URL", "nats://localhost:4222"),
		JWTPrivateKeyPEM:      getEnv("JWT_PRIVATE_KEY_PEM", ""),
		JWTPublicKeyPEM:       getEnv("JWT_PUBLIC_KEY_PEM", ""),
		JWTAccessTTL:          getEnvDuration("JWT_EXPIRY_SECONDS", 15*time.Minute),
		RefreshTokenTTL:       getEnvDuration("REFRESH_TOKEN_EXPIRY_DAYS", 14*24*time.Hour),
		RefreshCookieSecure:   getEnvBool("AUTH_COOKIE_SECURE", false),
		AuthCookieSecure:      getEnvBool("AUTH_COOKIE_SECURE", false),
		AppEncryptionKey:      getEnv("APP_ENCRYPTION_KEY", ""),
		BootstrapSetupToken:   getEnv("BOOTSTRAP_SETUP_TOKEN", ""),
		SMTPHost:              getEnv("SMTP_HOST", "localhost"),
		SMTPPort:              getEnvInt("SMTP_PORT", 1025),
		SMTPUsername:          getEnv("SMTP_USERNAME", ""),
		SMTPPassword:          getEnv("SMTP_PASSWORD", ""),
		SMTPFrom:              getEnv("SMTP_FROM", "no-reply@audita.local"),
		SMTPFromName:          getEnv("SMTP_FROM_NAME", "Audita"),
		SMTPAuth:              getEnvBool("SMTP_AUTH", false),
		SMTPTLS:               getEnvBool("SMTP_TLS", false),
		StorageBackend:        getEnv("STORAGE_BACKEND", "local"),
		StorageLocalBasePath:  getEnv("STORAGE_LOCAL_BASE_PATH", "/tmp/uploads"),
		StorageS3Bucket:       getEnv("STORAGE_S3_BUCKET", ""),
		StorageS3Region:       getEnv("STORAGE_S3_REGION", ""),
		StorageS3AccessKey:    getEnv("STORAGE_S3_ACCESS_KEY", ""),
		StorageS3SecretKey:    getEnv("STORAGE_S3_SECRET_KEY", ""),
		UploadMaxSizeBytes:    int64(getEnvInt("UPLOAD_MAX_SIZE_BYTES", 25*1024*1024)),
		UploadAllowedMIMEType: getEnvSlice("UPLOAD_ALLOWED_MIME_TYPES", []string{"image/*", "application/pdf", "text/*"}),
		InviteExpiryHours:     getEnvInt("INVITE_EXPIRY_HOURS", 72),
		OTLPEndpoint:          getEnv("OTEL_EXPORTER_OTLP_ENDPOINT", ""),
		OTLPSamplingProb:      getEnvFloat("OTEL_TRACING_SAMPLING_PROBABILITY", 0.1),
		LogLevel:              getEnv("LOG_LEVEL_APP", "info"),
		MetricsEnabled:        getEnvBool("METRICS_ENABLED", true),
		SLAWarnHoursBefore:    getEnvInt("SLA_WARN_HOURS_BEFORE", 24),
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return c, nil
}

// Validate ensures critical configuration is present and coherent.
func (c *Config) Validate() error {
	if c.DatabaseURL == "" {
		return fmt.Errorf("config: DATABASE_URL is required (e.g. postgres://user:pass@host:5432/audita?sslmode=disable)")
	}
	if c.JWTPrivateKeyPEM == "" {
		return fmt.Errorf("config: JWT_PRIVATE_KEY_PEM is required (PEM-encoded RSA or EC private key)")
	}
	if c.JWTPublicKeyPEM == "" {
		return fmt.Errorf("config: JWT_PUBLIC_KEY_PEM is required (PEM-encoded RSA or EC public key)")
	}
	if c.AppEncryptionKey == "" {
		return fmt.Errorf("config: APP_ENCRYPTION_KEY is required (base64-encoded 32-byte AES key)")
	}
	if c.SMTPPort <= 0 || c.SMTPPort > 65535 {
		return fmt.Errorf("config: SMTP_PORT must be between 1 and 65535, got %d", c.SMTPPort)
	}
	if c.StorageBackend != "local" && c.StorageBackend != "s3" {
		return fmt.Errorf("config: STORAGE_BACKEND must be 'local' or 's3', got %q", c.StorageBackend)
	}
	if c.StorageBackend == "s3" {
		if c.StorageS3Bucket == "" || c.StorageS3Region == "" {
			return fmt.Errorf("config: STORAGE_S3_BUCKET and STORAGE_S3_REGION are required when STORAGE_BACKEND=s3")
		}
	}
	if c.UploadMaxSizeBytes <= 0 {
		return fmt.Errorf("config: UPLOAD_MAX_SIZE_BYTES must be > 0")
	}
	return nil
}

func getEnv(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func getEnvInt(key string, def int) int {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func getEnvInt64(key string, def int64) int64 {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return def
	}
	return n
}

func getEnvFloat(key string, def float64) float64 {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return def
	}
	return f
}

func getEnvBool(key string, def bool) bool {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}

func getEnvDuration(key string, def time.Duration) time.Duration {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return def
	}
	return time.Duration(n) * time.Second
}

func getEnvSlice(key string, def []string) []string {
	v, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(v) == "" {
		return def
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}
