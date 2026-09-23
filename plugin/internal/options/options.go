// Package options parses the per-plugin options the runner forwards verbatim
// in CreateRequest.backend_options (runner config `plugins.<name>.options`).
package options

import (
	"fmt"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

// Option keys accepted in backend_options.
const (
	KeyNamespace        = "namespace"
	KeyPodspec          = "podspec"
	KeyReadyTimeout     = "ready_timeout"
	KeyImagePullPolicy  = "image_pull_policy"
	KeyServiceResources = "service_resources"
	KeyLabels           = "labels"
	KeyInstance         = "instance"
)

// DefaultReadyTimeout bounds Create→Ready when neither flags nor options set it.
const DefaultReadyTimeout = 10 * time.Minute

// Options is the effective configuration for one environment.
type Options struct {
	Namespace string
	// Podspec is the default podspec path; a non-empty label_arg overrides it.
	Podspec          string
	ReadyTimeout     time.Duration
	PullPolicy       corev1.PullPolicy
	ServiceResources *corev1.ResourceRequirements
	// Labels are extra pod labels; values may contain ${ENV_ID}.
	Labels   map[string]string
	Instance string
}

// Defaults come from the plugin's flags and environment and apply to any key
// the runner does not set.
type Defaults struct {
	Namespace    string
	Podspec      string
	Instance     string
	ReadyTimeout time.Duration
}

// Parse merges raw backend options over the defaults. Unknown keys are errors
// so a typo in the runner config fails the job loudly instead of being ignored.
func Parse(raw map[string]string, d Defaults) (Options, error) {
	o := Options{
		Namespace:    d.Namespace,
		Podspec:      d.Podspec,
		Instance:     d.Instance,
		ReadyTimeout: d.ReadyTimeout,
		PullPolicy:   corev1.PullIfNotPresent,
	}
	if o.ReadyTimeout == 0 {
		o.ReadyTimeout = DefaultReadyTimeout
	}
	keys := make([]string, 0, len(raw))
	for k := range raw {
		keys = append(keys, k)
	}
	sort.Strings(keys) // deterministic error for multiple bad keys
	for _, k := range keys {
		v := raw[k]
		if err := o.set(k, v); err != nil {
			return Options{}, fmt.Errorf("backend option %q: %w", k, err)
		}
	}
	return o, nil
}

func (o *Options) set(key, v string) error {
	switch key {
	case KeyNamespace:
		o.Namespace = v
	case KeyPodspec:
		o.Podspec = v
	case KeyInstance:
		o.Instance = v
	case KeyReadyTimeout:
		d, err := time.ParseDuration(v)
		if err != nil {
			return err
		}
		if d <= 0 {
			return fmt.Errorf("must be positive, got %s", v)
		}
		o.ReadyTimeout = d
	case KeyImagePullPolicy:
		switch p := corev1.PullPolicy(v); p {
		case corev1.PullAlways, corev1.PullIfNotPresent, corev1.PullNever:
			o.PullPolicy = p
		default:
			return fmt.Errorf("want Always, IfNotPresent or Never, got %q", v)
		}
	case KeyServiceResources:
		var r corev1.ResourceRequirements
		if err := yaml.UnmarshalStrict([]byte(v), &r); err != nil {
			return err
		}
		o.ServiceResources = &r
	case KeyLabels:
		l, err := ParseLabels(v)
		if err != nil {
			return err
		}
		o.Labels = l
	default:
		return fmt.Errorf("unknown option")
	}
	return nil
}

// ParseLabels parses "k=v,k=v" (whitespace around entries is ignored).
func ParseLabels(s string) (map[string]string, error) {
	out := map[string]string{}
	for _, kv := range strings.Split(s, ",") {
		kv = strings.TrimSpace(kv)
		if kv == "" {
			continue
		}
		k, v, ok := strings.Cut(kv, "=")
		k = strings.TrimSpace(k)
		if !ok || k == "" {
			return nil, fmt.Errorf("want key=value, got %q", kv)
		}
		out[k] = strings.TrimSpace(v)
	}
	return out, nil
}
