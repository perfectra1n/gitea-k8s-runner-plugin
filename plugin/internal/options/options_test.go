package options

import (
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

var defaults = Defaults{Namespace: "ci", Instance: "runner-0"}

func TestParseDefaults(t *testing.T) {
	o, err := Parse(nil, defaults)
	if err != nil {
		t.Fatal(err)
	}
	if o.Namespace != "ci" || o.Instance != "runner-0" {
		t.Errorf("namespace/instance = %q/%q", o.Namespace, o.Instance)
	}
	if o.ReadyTimeout != 10*time.Minute {
		t.Errorf("ReadyTimeout = %v, want 10m", o.ReadyTimeout)
	}
	if o.PullPolicy != corev1.PullIfNotPresent {
		t.Errorf("PullPolicy = %q", o.PullPolicy)
	}
	if o.ServiceResources != nil || len(o.Labels) != 0 || o.Podspec != "" {
		t.Errorf("unexpected non-zero optional fields: %+v", o)
	}
}

func TestParseAll(t *testing.T) {
	o, err := Parse(map[string]string{
		"namespace":         "jobs",
		"podspec":           "/podspecs/small/podspec.yaml",
		"ready_timeout":     "90s",
		"image_pull_policy": "Always",
		"service_resources": "requests: {cpu: 100m}\nlimits: {memory: 256Mi}",
		"labels":            "team=ci, env=${ENV_ID}",
		"instance":          "other",
	}, defaults)
	if err != nil {
		t.Fatal(err)
	}
	if o.Namespace != "jobs" || o.Podspec != "/podspecs/small/podspec.yaml" || o.Instance != "other" {
		t.Errorf("strings: %+v", o)
	}
	if o.ReadyTimeout != 90*time.Second || o.PullPolicy != corev1.PullAlways {
		t.Errorf("timeout/pull: %v %q", o.ReadyTimeout, o.PullPolicy)
	}
	if got := o.ServiceResources.Requests[corev1.ResourceCPU]; got.Cmp(resource.MustParse("100m")) != 0 {
		t.Errorf("cpu request = %s", got.String())
	}
	if got := o.ServiceResources.Limits[corev1.ResourceMemory]; got.Cmp(resource.MustParse("256Mi")) != 0 {
		t.Errorf("memory limit = %s", got.String())
	}
	if o.Labels["team"] != "ci" || o.Labels["env"] != "${ENV_ID}" || len(o.Labels) != 2 {
		t.Errorf("labels = %v", o.Labels)
	}
}

func TestParseErrors(t *testing.T) {
	cases := map[string]map[string]string{
		"unknown key":       {"nmespace": "x"},
		"bad duration":      {"ready_timeout": "soon"},
		"negative duration": {"ready_timeout": "-1s"},
		"bad pull policy":   {"image_pull_policy": "Sometimes"},
		"bad labels":        {"labels": "novalue"},
		"empty label key":   {"labels": "=v"},
		"bad resources":     {"service_resources": "requests: {cpu: lots}"},
		"unknown resources": {"service_resources": "reqests: {}"},
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse(in, defaults); err == nil {
				t.Fatal("want error")
			}
		})
	}
}

func TestParseErrorNamesKey(t *testing.T) {
	_, err := Parse(map[string]string{"ready_timeout": "soon"}, defaults)
	if err == nil || !strings.Contains(err.Error(), "ready_timeout") {
		t.Fatalf("error %v should name the option", err)
	}
}
