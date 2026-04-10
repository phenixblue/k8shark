package config

import (
	"fmt"
	"os"
	"time"

	"github.com/spf13/viper"
)

// Resource describes a single Kubernetes resource type to capture.
type Resource struct {
	// Group is the API group (empty string = core group).
	Group string `mapstructure:"group"`
	// Version is the API version (e.g. "v1", "v1beta1").
	Version string `mapstructure:"version"`
	// Resource is the plural resource name (e.g. "pods", "deployments").
	Resource string `mapstructure:"resource"`
	// Namespaces is the list of namespaces to query. Empty means cluster-scoped
	// or all namespaces depending on the resource.
	Namespaces []string `mapstructure:"namespaces"`
	// IntervalRaw is the human-readable polling interval (e.g. "30s").
	IntervalRaw string `mapstructure:"interval"`
	// Interval is parsed from IntervalRaw.
	Interval time.Duration `mapstructure:"-"`
	// Logs is the number of tail lines to capture from each pod's log when this
	// resource is "pods". 0 (default) disables log capture. For example, set
	// logs: 200 to capture the last 200 lines from every pod at capture time.
	Logs int `mapstructure:"logs"`
}

// Config holds the full capture configuration.
type Config struct {
	// DurationRaw is the human-readable total capture duration (e.g. "10m").
	DurationRaw string `mapstructure:"duration"`
	// Duration is parsed from DurationRaw.
	Duration time.Duration `mapstructure:"-"`
	// Output is the path to write the resulting .tar.gz file.
	Output string `mapstructure:"output"`
	// Kubeconfig is the path to the kubeconfig to use. Empty = default resolution.
	Kubeconfig string `mapstructure:"kubeconfig"`
	// Resources is the list of resources to capture.
	Resources []Resource `mapstructure:"resources"`
}

// Load reads k8shark capture config. If configFile is empty, viper uses
// whatever was already loaded via initConfig in cmd/.
func Load(configFile string) (*Config, error) {
	if configFile != "" {
		viper.SetConfigFile(configFile)
		if err := viper.ReadInConfig(); err != nil {
			return nil, fmt.Errorf("reading config file %q: %w", configFile, err)
		}
	}

	var cfg Config
	if err := viper.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("unmarshalling config: %w", err)
	}

	return &cfg, nil
}

// Validate parses duration/interval raw strings and checks required fields.
func (c *Config) Validate() error {
	if c.DurationRaw == "" {
		c.DurationRaw = "10m"
	}
	d, err := time.ParseDuration(c.DurationRaw)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", c.DurationRaw, err)
	}
	c.Duration = d

	if c.Output == "" {
		c.Output = fmt.Sprintf("k8shark-%s.tar.gz", time.Now().UTC().Format("20060102-150405"))
	}

	if c.Kubeconfig == "" {
		if kc := os.Getenv("KUBECONFIG"); kc != "" {
			c.Kubeconfig = kc
		}
		// empty string is fine — client-go will use ~/.kube/config
	}

	if len(c.Resources) == 0 {
		return fmt.Errorf("no resources defined in config; add at least one entry under 'resources:'")
	}

	for i := range c.Resources {
		r := &c.Resources[i]
		if r.Resource == "" {
			return fmt.Errorf("resources[%d]: 'resource' field is required", i)
		}
		if r.Version == "" {
			return fmt.Errorf("resources[%d] (%s): 'version' field is required", i, r.Resource)
		}
		if r.IntervalRaw == "" {
			r.IntervalRaw = "30s"
		}
		iv, err := time.ParseDuration(r.IntervalRaw)
		if err != nil {
			return fmt.Errorf("resources[%d] (%s): invalid interval %q: %w", i, r.Resource, r.IntervalRaw, err)
		}
		r.Interval = iv
		if r.Logs < 0 {
			return fmt.Errorf("resources[%d] (%s): 'logs' must be >= 0", i, r.Resource)
		}
	}

	return nil
}
