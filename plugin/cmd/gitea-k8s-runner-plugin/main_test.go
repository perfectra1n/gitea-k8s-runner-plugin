package main

import (
	"testing"
	"time"

	"github.com/perfectra1n/gitea-k8s-runner-plugin/plugin/internal/server"
)

func TestParseListen(t *testing.T) {
	cases := []struct{ in, network, addr string }{
		{"unix:///run/plugin/plugin.sock", "unix", "/run/plugin/plugin.sock"},
		{"tcp://127.0.0.1:7070", "tcp", "127.0.0.1:7070"},
		{"tcp://:7070", "tcp", ":7070"},
	}
	for _, c := range cases {
		n, a, err := parseListen(c.in)
		if err != nil || n != c.network || a != c.addr {
			t.Errorf("parseListen(%q) = %q %q %v", c.in, n, a, err)
		}
	}
	for _, bad := range []string{"", "/run/x.sock", "http://x", "unix://", "unix://relative.sock", "tcp://"} {
		if _, _, err := parseListen(bad); err == nil {
			t.Errorf("parseListen(%q) should fail", bad)
		}
	}
}

func TestParseFlagsDefaultsFromEnv(t *testing.T) {
	env := map[string]string{"POD_NAMESPACE": "ci", "POD_NAME": "runner-7f9c"}
	cfg, err := parseFlags([]string{"--podspec", "/podspecs/default/podspec.yaml", "--ready-timeout", "2m"}, func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.listen != "unix:///run/plugin/plugin.sock" || cfg.defaults.Namespace != "ci" || cfg.defaults.Instance != "runner-7f9c" {
		t.Errorf("cfg = %+v", cfg)
	}
	if cfg.defaults.Podspec != "/podspecs/default/podspec.yaml" || cfg.defaults.ReadyTimeout != 2*time.Minute {
		t.Errorf("defaults = %+v", cfg.defaults)
	}
	if _, err := parseFlags(nil, func(string) string { return "" }); err == nil {
		t.Error("no namespace anywhere should be an error")
	}
	cfg, err = parseFlags([]string{"--namespace", "jobs", "--instance", "a"}, func(string) string { return "" })
	if err != nil || cfg.defaults.Namespace != "jobs" || cfg.defaults.Instance != "a" {
		t.Errorf("explicit flags: %+v %v", cfg, err)
	}
}

func TestParseFlagsStepHelper(t *testing.T) {
	env := func(string) string { return "x" }
	cfg, err := parseFlags(nil, env)
	if err != nil || cfg.defaults.StepHelperImage != "" || cfg.reattachTimeout != server.DefaultReattachTimeout {
		t.Fatalf("defaults: %+v %v", cfg, err)
	}
	cfg, err = parseFlags([]string{"--step-helper-image", "ghcr.io/x/p:1", "--reattach-timeout", "90s"}, env)
	if err != nil || cfg.defaults.StepHelperImage != "ghcr.io/x/p:1" || cfg.reattachTimeout != 90*time.Second {
		t.Fatalf("set: %+v %v", cfg, err)
	}
	if _, err := parseFlags([]string{"--reattach-timeout", "0s"}, env); err == nil {
		t.Error("a zero reattach timeout should be an error")
	}
}
