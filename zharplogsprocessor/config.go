package zharplogsprocessor

import (
	"errors"
	"fmt"
	"time"

	"github.com/zharp-io/agent/logrules"
)

// Config is the zharp_logs processor's configuration.
type Config struct {
	// Rules apply to every log record that passes through: exclude drops a record whose text
	// matches, mask rewrites every match. They run after a source's own rules (labels,
	// annotations) and before the built-in masks.
	Rules []logrules.Rule `mapstructure:"rules"`

	// DefaultMasks turns the built-in masks on (the default): payment card numbers (Luhn
	// checked), bearer tokens, JWTs, AWS keys and private-key blocks.
	DefaultMasks bool `mapstructure:"default_masks"`

	// DisableDefaultMasks turns individual built-in masks off by name (credit_card,
	// bearer_token, jwt, aws_access_key, aws_secret_key, private_key).
	DisableDefaultMasks []string `mapstructure:"disable_default_masks"`

	// Environment is the deployment.environment.name given to data that doesn't carry one.
	Environment string `mapstructure:"environment"`

	// Kubernetes applies the zharp.io/* pod annotation contract to data the k8sattributes
	// processor has tagged with its pod.
	Kubernetes KubernetesConfig `mapstructure:"kubernetes"`
}

// KubernetesConfig is the Kubernetes part of the configuration.
type KubernetesConfig struct {
	Enabled bool `mapstructure:"enabled"`

	// Multiline is how container lines are joined into records when a pod has no
	// zharp.io/logs-multiline annotation: auto (the default), off, or a regex for the first
	// line of each record.
	Multiline string `mapstructure:"multiline"`

	// FlushTimeout is how long a record waits for more lines before it is sent.
	FlushTimeout time.Duration `mapstructure:"flush_timeout"`

	// MaxLines and MaxBytes bound one joined record.
	MaxLines int `mapstructure:"max_lines"`
	MaxBytes int `mapstructure:"max_bytes"`
}

// Validate checks every rule and pattern so a typo fails `validate` instead of at runtime.
func (c *Config) Validate() error {
	if _, err := c.ruleset(); err != nil {
		return fmt.Errorf("zharp_logs: %w", err)
	}
	if _, err := logrules.ParseMultiline(c.Kubernetes.Multiline); err != nil {
		return fmt.Errorf("zharp_logs: kubernetes.%w", err)
	}
	if c.Kubernetes.FlushTimeout < 0 || c.Kubernetes.MaxLines < 0 || c.Kubernetes.MaxBytes < 0 {
		return errors.New("zharp_logs: kubernetes flush_timeout, max_lines and max_bytes must not be negative")
	}
	return nil
}

// ruleset compiles the global rules with the built-in masks.
func (c *Config) ruleset() (*logrules.Ruleset, error) {
	rs, err := logrules.Compile(c.Rules)
	if err != nil {
		return nil, err
	}
	if !c.DefaultMasks {
		return rs, nil
	}
	return rs.WithDefaults(c.DisableDefaultMasks)
}
