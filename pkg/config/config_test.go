package config

import (
	"testing"
	"time"
)

// TestFromEnvDefaults verifies that defaults are applied when only the
// required env var is set.
func TestFromEnvDefaults(t *testing.T) {
	t.Setenv("CCGW_VERTEX_PROJECT_ID", "test-proj")
	// Clear any user-supplied values that would shadow defaults.
	t.Setenv("CCGW_LISTEN_ADDR", "")
	t.Setenv("CCGW_REGION", "")
	t.Setenv("CCGW_WRITE_TIMEOUT", "")
	t.Setenv("CCGW_LOG_USAGE_STDOUT", "")

	c, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if c.ListenAddr != ":8080" {
		t.Errorf("ListenAddr = %q, want :8080", c.ListenAddr)
	}
	if c.Region != "global" {
		t.Errorf("Region = %q, want global", c.Region)
	}
	if c.VertexProjectID != "test-proj" {
		t.Errorf("VertexProjectID = %q, want test-proj", c.VertexProjectID)
	}
	if c.WriteTimeout != 30*time.Minute {
		t.Errorf("WriteTimeout = %v, want 30m", c.WriteTimeout)
	}
	if !c.LogUsageToStdout {
		t.Error("LogUsageToStdout = false, want true")
	}
}

// TestFromEnvRequiresProject verifies that the missing required field is reported.
func TestFromEnvRequiresProject(t *testing.T) {
	t.Setenv("CCGW_VERTEX_PROJECT_ID", "")
	if _, err := FromEnv(); err == nil {
		t.Fatal("expected error when CCGW_VERTEX_PROJECT_ID is empty")
	}
}

// TestFromEnvOverrides verifies that all env vars override defaults.
func TestFromEnvOverrides(t *testing.T) {
	t.Setenv("CCGW_VERTEX_PROJECT_ID", "p")
	t.Setenv("CCGW_LISTEN_ADDR", "127.0.0.1:9999")
	t.Setenv("CCGW_REGION", "europe-west4")
	t.Setenv("CCGW_WRITE_TIMEOUT", "5m")
	t.Setenv("CCGW_LOG_USAGE_STDOUT", "false")

	c, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if c.ListenAddr != "127.0.0.1:9999" {
		t.Errorf("ListenAddr = %q", c.ListenAddr)
	}
	if c.Region != "europe-west4" {
		t.Errorf("Region = %q", c.Region)
	}
	if c.WriteTimeout != 5*time.Minute {
		t.Errorf("WriteTimeout = %v", c.WriteTimeout)
	}
	if c.LogUsageToStdout {
		t.Error("LogUsageToStdout = true, want false")
	}
}

// TestFromEnvInvalidDuration verifies that a bad duration is rejected.
func TestFromEnvInvalidDuration(t *testing.T) {
	t.Setenv("CCGW_VERTEX_PROJECT_ID", "p")
	t.Setenv("CCGW_WRITE_TIMEOUT", "not-a-duration")
	if _, err := FromEnv(); err == nil {
		t.Fatal("expected error for invalid CCGW_WRITE_TIMEOUT")
	}
}

// TestUpstreamHostGlobal verifies the global region maps to the unprefixed host.
func TestUpstreamHostGlobal(t *testing.T) {
	c := &Config{Region: "global"}
	if got := c.UpstreamHost(); got != "aiplatform.googleapis.com" {
		t.Errorf("UpstreamHost(global) = %q", got)
	}
}

// TestUpstreamHostRegional verifies a regional value gets the per-region prefix.
func TestUpstreamHostRegional(t *testing.T) {
	c := &Config{Region: "us-east5"}
	if got := c.UpstreamHost(); got != "us-east5-aiplatform.googleapis.com" {
		t.Errorf("UpstreamHost(us-east5) = %q", got)
	}
}

// TestUpstreamHostEmpty verifies an empty region behaves like global.
func TestUpstreamHostEmpty(t *testing.T) {
	c := &Config{Region: ""}
	if got := c.UpstreamHost(); got != "aiplatform.googleapis.com" {
		t.Errorf("UpstreamHost(empty) = %q", got)
	}
}
