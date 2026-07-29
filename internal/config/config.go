package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/go-viper/mapstructure/v2"
	"github.com/spf13/viper"
	"gopkg.in/yaml.v3"
)

const minDiscoveryStartupDuration = 5 * time.Second

// CurrentConfigVersion is the config schema version this build understands.
// Bump only on a breaking, structurally-incompatible schema change — an
// additive field never needs a bump (mirrors the .kshrk archive format's
// CurrentFormatVersion convention; see internal/archive/format).
const CurrentConfigVersion = 1

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
	// PreviousLogs, when true, also captures the previous-container log for
	// each container using ?previous=true. The same tail-line count from Logs
	// applies. Useful for diagnosing CrashLoopBackOff pods where the current
	// container is starting fresh and the interesting output lives in the
	// terminated previous container.
	PreviousLogs bool `mapstructure:"previousLogs"`
	// Dedup controls response-body deduplication for this resource. Nil means
	// enabled by default; set dedup: false to force writing every poll.
	Dedup *bool `mapstructure:"dedup"`
	// Watch enables a Kubernetes watch stream for this resource in addition to
	// polling. Watch events are captured with low-latency timestamps.
	Watch bool `mapstructure:"watch"`
	// AutoDiscovered is set to true by the engine when this entry was generated
	// via auto-discovery (all: true). It is not a user-facing config field.
	AutoDiscovered bool `mapstructure:"-"`
	// WildcardNamespaces is set to true by the engine when this entry was
	// originally configured with a wildcard ("*") in Namespaces. After wildcard
	// expansion the engine still needs to know the original intent so watch
	// streams can use a single cluster-wide watch instead of one per expanded
	// namespace. Not a user-facing config field.
	WildcardNamespaces bool `mapstructure:"-"`
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
	// Version pins this config to a schema version, letting the schema
	// evolve without silently misinterpreting an older or newer file. Omit
	// for version 1 (the implicit default for every config predating this
	// field). See CurrentConfigVersion.
	Version int `mapstructure:"version"`
	// DurationRaw is the human-readable total capture duration (e.g. "10m").
	DurationRaw string `mapstructure:"duration"`
	// Duration is parsed from DurationRaw.
	Duration time.Duration `mapstructure:"-"`
	// Output is the path to write the resulting .kshrk file.
	Output string `mapstructure:"output"`
	// Kubeconfig is the path to the kubeconfig to use. Empty = default resolution.
	Kubeconfig string `mapstructure:"kubeconfig"`
	// Resources is the list of resources to capture.
	Resources []Resource `mapstructure:"resources"`
	// AutoDiscover, when true, causes the capture engine to walk /apis at
	// capture time and automatically add every discovered non-core resource
	// type to the poll loop, supplementing any explicit Resources entries.
	AutoDiscover bool `mapstructure:"autoDiscover"`
	// AutoDiscoverExcludeGroups is an optional list of API groups to skip
	// during auto-discovery (e.g. "metrics.k8s.io"). System groups that
	// produce noisy or unusable data are excluded by default regardless of
	// this setting; see defaultAutoDiscoverExcludeGroups.
	AutoDiscoverExcludeGroups []string `mapstructure:"autoDiscoverExcludeGroups"`
	// Redaction holds field-level redaction rules applied during capture and
	// post-capture redact workflows.
	Redaction RedactionConfig `mapstructure:"redaction"`
	// UI holds settings for the `kshrk ui` web explorer. CLI flags override
	// these when provided.
	UI UIConfig `mapstructure:"ui"`
}

// UIConfig configures the `kshrk ui` servers. Ports are strings so "0" (a
// random available port, the default) can be expressed; set them to pin a
// consistent port across runs.
type UIConfig struct {
	// Port is the local web UI port. Empty or "0" means a random port.
	Port string `mapstructure:"port"`
	// APIPort is the mock Kubernetes API server port. Empty or "0" means random.
	APIPort string `mapstructure:"apiPort"`
}

// Load reads k8shark capture config from configFile, an explicit source
// (not the global viper singleton — see the override step below for the one
// deliberate, narrow exception). An empty configFile means no config file
// at all (e.g. an --auto-discover-only capture with no resources: list);
// Load then returns a config built purely from env/flag overrides and
// defaults.
func Load(configFile string) (*Config, error) {
	var cfg Config

	if configFile != "" {
		data, err := os.ReadFile(configFile)
		if err != nil {
			return nil, fmt.Errorf("reading config file %q: %w", configFile, err)
		}
		var raw map[string]any
		if err := yaml.Unmarshal(data, &raw); err != nil {
			return nil, fmt.Errorf("parsing config file %q: %w", configFile, err)
		}
		applyLegacyKeyAliases(raw)

		// Decode twice from the same alias-normalized map: once strictly, so
		// a typo'd or misspelled key (e.g. "previouslogs" instead of
		// "previousLogs") fails validation by name instead of being
		// silently ignored (#220), and once tolerantly to actually populate
		// cfg (case-insensitive, matching this package's historical
		// leniency — safe here since the strict pass already proved every
		// key is a known one spelled correctly).
		if err := strictDecode(raw); err != nil {
			return nil, fmt.Errorf("config file %q: %w", configFile, err)
		}
		if err := mapstructure.Decode(raw, &cfg); err != nil {
			return nil, fmt.Errorf("parsing config file %q: %w", configFile, err)
		}
	}

	// Layer KSHRK_-prefixed environment variable overrides on top of the file
	// (see cmd/root.go's initConfig for the SetEnvPrefix/AutomaticEnv setup).
	// CLI flags are applied by each command after Load returns (see
	// cmd/capture.go), so the effective precedence is flag > env > file >
	// default. Deliberately reads the global viper singleton only for this
	// narrow purpose — env/flag override resolution — not for the config
	// file's own content, which is parsed directly above (#220).
	if v := viper.GetString("output"); v != "" {
		cfg.Output = v
	}
	if v := viper.GetString("kubeconfig"); v != "" {
		cfg.Kubeconfig = v
	}
	if v := viper.GetString("duration"); v != "" {
		cfg.DurationRaw = v
	}
	if viper.IsSet("autoDiscover") {
		cfg.AutoDiscover = viper.GetBool("autoDiscover")
	}

	if cfg.Version > CurrentConfigVersion {
		return nil, fmt.Errorf("config version %d is newer than this build of kshrk understands (max %d) — upgrade kshrk", cfg.Version, CurrentConfigVersion)
	}

	return &cfg, nil
}

// legacyConfigKeyAliases maps a deprecated config key (dot-path for a
// nested section) to its current, camelCase replacement. Both spellings are
// accepted for one minor release (#220); a future release will drop the
// legacy spelling.
var legacyConfigKeyAliases = map[string]string{
	"auto_discover":                "autoDiscover",
	"auto_discover_exclude_groups": "autoDiscoverExcludeGroups",
	"ui.api_port":                  "ui.apiPort",
}

// applyLegacyKeyAliases renames every legacy key present in raw (see
// legacyConfigKeyAliases) to its canonical spelling in place, preferring an
// already-present canonical value if both are somehow set. raw's keys and
// nesting come directly from yaml.Unmarshal, so casing is exactly as
// written in the file (unlike viper's internal settings, which are
// lowercased — see strictDecode's doc comment for why that matters). Keys
// are dot-paths (e.g. "ui.api_port"); only one level of nesting is
// supported, which is all legacyConfigKeyAliases currently needs.
func applyLegacyKeyAliases(raw map[string]any) {
	for legacyPath, canonicalPath := range legacyConfigKeyAliases {
		m, legacy := resolveParent(raw, legacyPath)
		if m == nil {
			continue
		}
		v, ok := m[legacy]
		if !ok {
			continue
		}
		cm, canonical := ensureParent(raw, canonicalPath)
		if _, exists := cm[canonical]; !exists {
			cm[canonical] = v
		}
		delete(m, legacy)
	}
}

// resolveParent walks a dot-path (e.g. "ui.apiPort") from raw and returns
// the map holding the final segment plus that segment's own key, or (nil,
// "") if an intermediate segment doesn't exist or isn't itself a map.
func resolveParent(raw map[string]any, path string) (map[string]any, string) {
	parts := strings.Split(path, ".")
	m := raw
	for _, p := range parts[:len(parts)-1] {
		next, ok := m[p].(map[string]any)
		if !ok {
			return nil, ""
		}
		m = next
	}
	return m, parts[len(parts)-1]
}

// ensureParent is resolveParent, but creates any missing intermediate map
// instead of failing — used for the canonical (destination) side of an
// alias, where the legacy key's presence doesn't guarantee the canonical
// key's parent section already exists in raw.
func ensureParent(raw map[string]any, path string) (map[string]any, string) {
	parts := strings.Split(path, ".")
	m := raw
	for _, p := range parts[:len(parts)-1] {
		next, ok := m[p].(map[string]any)
		if !ok {
			next = map[string]any{}
			m[p] = next
		}
		m = next
	}
	return m, parts[len(parts)-1]
}

// strictDecode decodes raw against a throwaway Config with ErrorUnused, so
// an unknown key is reported by name, and with exact (case-sensitive) key
// matching, so a wrongly-cased key like "previouslogs" is caught as unknown
// too rather than silently case-folding onto "previousLogs" — mapstructure's
// (and therefore viper's) default MatchName is case-insensitive, which would
// otherwise defeat "one consistent naming convention" by quietly accepting
// any casing. Deliberately does not use viper for this: viper lowercases
// every key it reads, which would destroy the very case information this
// check depends on.
func strictDecode(raw map[string]any) error {
	decoder, err := mapstructure.NewDecoder(&mapstructure.DecoderConfig{
		ErrorUnused: true,
		Result:      &Config{},
		MatchName:   func(mapKey, fieldName string) bool { return mapKey == fieldName },
	})
	if err != nil {
		return fmt.Errorf("building config decoder: %w", err)
	}
	return decoder.Decode(raw)
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
		c.Output = fmt.Sprintf("k8shark-%s.kshrk", time.Now().UTC().Format("20060102-150405"))
	}

	if c.Kubeconfig == "" {
		if kc := os.Getenv("KUBECONFIG"); kc != "" {
			c.Kubeconfig = kc
		}
		// empty string is fine — client-go will use ~/.kube/config
	}

	if len(c.Resources) == 0 && !c.AutoDiscover {
		return fmt.Errorf("no resources defined in config; add at least one entry under 'resources:' or set 'autoDiscover: true'")
	}

	if c.requiresDiscoveryStartup() && c.Duration < minDiscoveryStartupDuration {
		return fmt.Errorf(
			"duration %q is too short when using discovery/wildcard capture; set duration >= %s or disable autoDiscover, all=true, and namespaces ['*']",
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
