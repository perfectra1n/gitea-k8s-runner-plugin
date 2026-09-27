//go:build linux

// Command gitea-k8s-runner-step is the step helper the plugin installs into
// job pods: it runs each step detached from its exec stream so a broken
// stream can be resumed instead of failing the step. See package stephelper.
package main

import (
	"os"

	"github.com/perfectra1n/gitea-k8s-runner-plugin/plugin/internal/stephelper"
)

func main() { os.Exit(stephelper.Main(os.Args[1:], os.Stdout, os.Stderr)) }
