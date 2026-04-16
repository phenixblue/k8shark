package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/viper"
)

const minDiscoveryStartupDuration = 5 * time.Second

// Resource describes a single Kubernetes resource type to capture.
type Resource struct {
	// All enables auto-discovery expansion for this resource entry. When true,
	// Group/Version/Resource are ignored and discovered resources are added
	// according to Scope and Namespaces.
	All bool `mapstructure:"all"`
	// Scope filters discovered resources when All=true: "namespaced" or
	// "cluster". Empty means both.
	Scope string `mapstructure:"scope"`
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
	// Dedup controls response-body deduplication for this resource. Nil means
	// enabled by default; set dedup: false to force writing every poll.
	Dedup *bool `mapstructure:"dedup"`
	// Watch enables a Kubernetes watch stream for this resource in addition to
	// polling. Watch events are captured with low-latency timestamps.
	Watch bool `mapstructure:"watch"`
	// AutoDiscovered is set to true by the engine when this entry was generated
	// via auto-discovery (all: true). It is not a user-facing config field.
	AutoDiscovered bool `mapstructure:"-"`
}

// DedupEnabled reports whether polling responses for this resource should be
// deduplicated when body bytes are identical to the prior poll.
func (r Resource) DedupEnabled() bool {
	return r.Dedup == nil || *r.Dedup
}

// RedactionRule describes a single field-level redaction rule.
type RedactionRule struct {
	// FieldPath is a JSONPath-like expression identifying the field(s) to
	// redact. Supports dot-notation, array wildcards ([*]), and recursive
	// descent (**). Examples: "data.api-key", "spec.containers[*].env[*].value",
	// "**.password".
	FieldPath string `mapstructure:"fieldPath"`
	// Kind restricts the rule to a specific resource kind (e.g. "Pod",
	// "ConfigMap"). Use "*" or omit to match all kinds.
	Kind string `mapstructure:"kind"`
	// Namespace restricts the rule to resources in a specific namespace.
	// Omit to match all namespaces.
	Namespace string `mapstructure:"namespace"`
	// LabelSelector restricts the rule to resources whose metadata.labels match
	// the selector expression (for example: "app=api,tier in (backend)").
	// Omit to match all labels.
	LabelSelector string `mapstructure:"labelSelector"`
	// Replacement is the string value written in place of the redacted field.
	// It will be converted to the appropriate JSON type (see ValueType).
	Replacement string `mapstructure:"replacement"`
	// ValueType optionally overrides type inference. Accepted values: "string",
	// "integer", "number", "bool", "array", "object". When omitted the engine
	// infers the type from the actual captured value.
	ValueType string `mapstructure:"valueType"`
}

// RedactionConfig is the top-level redaction section of the capture config.
type RedactionConfig struct {
	// RedactSecrets, when true, redacts all Kubernetes Secret data and
	// stringData fields (equivalent to --redact-secrets on the CLI).
	RedactSecrets bool `mapstructure:"redactSecrets"`
	// AllowSecrets is a list of "namespace/name" secret keys whose data will
	// be preserved even when RedactSecrets is true.
	AllowSecrets []string `mapstructure:"allowSecrets"`
	// Rules is the list of field-level redaction rules applied to every record.
	Rules []RedactionRule `mapstructure:"rules"`
}

// Config holds the full capture configuration.
type Config struct {
	// DurationRaw is the human-readable total capture duration (e.g. "10m").
	DurationRaw string `mapstructure:"duration"`
	// Duration is parsed from DurationRaw.
	Duration time.Duration `mapstructure:"-"`
	// Output is the path to write the resulting .khsrk file.
	Output string `mapstructure:"output"`
	// Kubeconfig is the path to the kubeconfig to use. Empty = default resolution.
	Kubeconfig string `mapstructure:"kubeconfig"`
	// Resources is the list of resources to capture.
	Resources []Resource `mapstructure:"resources"`
	// AutoDiscover, when true, causes the capture engine to walk /apis at
	// capture time and automatically add every discovered non-core resource
	// type to the poll loop, supplementing any explicit Resources entries.
	AutoDiscover bool `mapstructure:"auto_discover"`
	// AutoDiscoverExcludeGroups is an optional list of API groups to skip
	// during auto-discovery (e.g. "metrics.k8s.io"). System groups that
	// produce noisy or unusable data are excluded by default regardless of
	// this setting; see defaultAutoDiscoverExcludeGroups.
	AutoDiscoverExcludeGroups []string `mapstructure:"auto_discover_exclude_groups"`
	// Redaction holds field-level redaction rules applied during capture and
	// post-capture redact workflows.
	Redaction RedactionConfig `mapstructure:"redaction"`
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
		c.Output = fmt.Sprintf("k8shark-%s.khsrk", time.Now().UTC().Format("20060102-150405"))
	}

	if c.Kubeconfig == "" {
		if kc := os.Getenv("KUBECONFIG"); kc != "" {
			c.Kubeconfig = kc
		}
		// empty string is fine — client-go will use ~/.kube/config
	}

	if len(c.Resources) == 0 && !c.AutoDiscover {
		return fmt.Errorf("no resources defined in config; add at least one entry under 'resources:' or set 'auto_discover: true'")
	}

	if c.requiresDiscoveryStartup() && c.Duration < minDiscoveryStartupDuration {
		return fmt.Errorf(
			"duration %q is too short when using discovery/wildcard capture; set duration >= %s or disable auto_discover, all=true, and namespaces ['*']",
			c.DurationRaw,
			minDiscoveryStartupDuration,
		)
	}

	for i := range c.Resources {
		r := &c.Resources[i]
		if r.All {
			r.Scope = strings.ToLower(strings.TrimSpace(r.Scope))
			if r.Scope != "" && r.Scope != "namespaced" && r.Scope != "cluster" {
				return fmt.Errorf("resources[%d]: invalid scope %q for all=true (must be namespaced, cluster, or empty)", i, r.Scope)
			}
			if r.IntervalRaw == "" {
				r.IntervalRaw = "30s"
			}
			iv, err := time.ParseDuration(r.IntervalRaw)
			if err != nil {
				return fmt.Errorf("resources[%d] (all=true): invalid interval %q: %w", i, r.IntervalRaw, err)
			}
			r.Interval = iv
			if r.Logs < 0 {
				return fmt.Errorf("resources[%d] (all=true): 'logs' must be >= 0", i)
			}
			continue
		}
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

func (c *Config) requiresDiscoveryStartup() bool {
	if c.AutoDiscover {
		return true
	}
	for _, r := range c.Resources {
		if r.All {
			return true
		}
		for _, ns := range r.Namespaces {
			if ns == "*" {
				return true
			}
		}
	}
	return false
}

// knownClusterScoped is the set of well-known cluster-scoped resource kinds.
// Resources in this set that also specify namespaces: in the config are likely
// a mistake — the capture engine auto-corrects at runtime but warns the user.
var knownClusterScoped = map[string]bool{
	"nodes":                           true,
	"namespaces":                      true,
	"persistentvolumes":               true,
	"storageclasses":                  true,
	"clusterroles":                    true,
	"clusterrolebindings":             true,
	"apiservices":                     true,
	"ingressclasses":                  true,
	"priorityclasses":                 true,
	"runtimeclasses":                  true,
	"volumeattachments":               true,
	"csidrivers":                      true,
	"csinodes":                        true,
	"mutatingwebhookconfigurations":   true,
	"validatingwebhookconfigurations": true,
	"customresourcedefinitions":       true,
	"certificatesigningrequests":      true,
}

// IsClusterScoped reports whether resource is a well-known cluster-scoped resource.
func IsClusterScoped(resource string) bool { return knownClusterScoped[resource] }

// Warnings returns a list of non-fatal advisory messages about cfg.
// Validate must be called before Warnings so that Duration and Interval fields
// are populated.
func Warnings(cfg *Config) []string {
	var ws []string

	if cfg.Duration > 2*time.Hour {
		ws = append(ws, fmt.Sprintf(
			"capture duration %s is very long and may produce a large archive", cfg.Duration))
	}

	if cfg.Output != "" && cfg.Output != "-" {
		if _, err := os.Stat(cfg.Output); err == nil {
			ws = append(ws, fmt.Sprintf(
				"output file %q already exists and will be overwritten", cfg.Output))
		}
	}

	for i, r := range cfg.Resources {
		if r.Interval > 0 && r.Interval < 5*time.Second {
			ws = append(ws, fmt.Sprintf(
				"resources[%d] (%s): interval %s is very short and may produce a large archive",
				i, firstNonEmpty(r.Resource, "all"), r.Interval))
		}
		if r.All {
			continue
		}
		if knownClusterScoped[r.Resource] && len(r.Namespaces) > 0 {
			ws = append(ws, fmt.Sprintf(
				"resources[%d] (%s): cluster-scoped resource has 'namespaces:' set — this will be ignored at capture time",
				i, r.Resource))
		}
		// For non-core (CRD-backed) resources we cannot determine cluster-scope
		// offline. Warn when 'namespaces:' is set so the user knows to verify.
		if r.Group != "" && !knownClusterScoped[r.Resource] && len(r.Namespaces) > 0 {
			ws = append(ws, fmt.Sprintf(
				"resources[%d] (%s): non-core resource with 'namespaces:' set — "+
					"if this is a cluster-scoped CRD (e.g. ClusterIssuer, ClusterPolicy) "+
					"remove 'namespaces:' so the cluster-scoped path is captured instead",
				i, r.Resource))
		}
	}
	return ws
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
