// Package config loads ccgw runtime configuration from environment variables.
package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

// Config holds all runtime configuration for the gateway.
type Config struct {
	// ListenAddr is the TCP address the gateway listens on (env CCGW_LISTEN_ADDR).
	ListenAddr string
	// Region is the Vertex AI region (env CCGW_REGION). "global" maps to
	// aiplatform.googleapis.com; any other value maps to <region>-aiplatform.googleapis.com.
	Region string
	// VertexProjectID is the GCP project that hosts the Vertex AI quota (env
	// CCGW_VERTEX_PROJECT_ID). Required.
	VertexProjectID string
	// WriteTimeout is the HTTP server write timeout (env CCGW_WRITE_TIMEOUT).
	// Claude Code sessions stream for a long time, so the default is generous.
	WriteTimeout time.Duration
	// LogUsageToStdout controls whether the stdout usage sink is enabled
	// (env CCGW_LOG_USAGE_STDOUT).
	LogUsageToStdout bool
}

// FromEnv loads configuration from environment variables, applying defaults
// and validating required fields.
func FromEnv() (*Config, error) {
	c := &Config{
		ListenAddr:       getenvDefault("CCGW_LISTEN_ADDR", ":8080"),
		Region:           getenvDefault("CCGW_REGION", "global"),
		VertexProjectID:  os.Getenv("CCGW_VERTEX_PROJECT_ID"),
		WriteTimeout:     30 * time.Minute,
		LogUsageToStdout: true,
	}

	if v := os.Getenv("CCGW_WRITE_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return nil, fmt.Errorf("CCGW_WRITE_TIMEOUT: %w", err)
		}
		c.WriteTimeout = d
	}

	if v := os.Getenv("CCGW_LOG_USAGE_STDOUT"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return nil, fmt.Errorf("CCGW_LOG_USAGE_STDOUT: %w", err)
		}
		c.LogUsageToStdout = b
	}

	if c.VertexProjectID == "" {
		return nil, fmt.Errorf("CCGW_VERTEX_PROJECT_ID is required")
	}

	return c, nil
}

// UpstreamHost returns the Vertex AI hostname for the configured region.
// The "global" region uses the unprefixed aiplatform.googleapis.com endpoint;
// all other regions use the per-region prefix.
func (c *Config) UpstreamHost() string {
	if c.Region == "" || c.Region == "global" {
		return "aiplatform.googleapis.com"
	}
	return c.Region + "-aiplatform.googleapis.com"
}

// getenvDefault returns the value of env var key, or def when it is unset/empty.
func getenvDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
