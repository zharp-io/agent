package zharpexporter

import (
	"errors"
	"strings"
	"time"
)

const defaultEndpoint = "https://api.zharp.io/api/v1"

// Config holds all configuration for the Zharp exporter.
type Config struct {
	// APIKey is the workspace API key shown in Zharp dashboard → Settings → API Keys.
	APIKey string `mapstructure:"api_key"`

	// Timeout for individual HTTP requests. Defaults to 10s.
	Timeout time.Duration `mapstructure:"timeout"`

	// BatchSize is the maximum log records per HTTP request. Defaults to 500.
	BatchSize int `mapstructure:"batch_size"`

	// Endpoint is the Zharp API to send to. Empty means Zharp Cloud; set it for self-hosted Zharp.
	Endpoint string `mapstructure:"endpoint"`

	// InventoryFile is what `zharp-collector install` found on this host. Its contents go out
	// with every heartbeat so the dashboard can show what is collected and what needs setup.
	InventoryFile string `mapstructure:"inventory_file"`
}

// baseURL is the API every request goes to, without a trailing slash.
func (c *Config) baseURL() string {
	if e := strings.TrimRight(strings.TrimSpace(c.Endpoint), "/"); e != "" {
		return e
	}
	return defaultEndpoint
}

func (c *Config) Validate() error {
	if c.APIKey == "" {
		return errors.New("zharpexporter: api_key is required")
	}
	return nil
}
