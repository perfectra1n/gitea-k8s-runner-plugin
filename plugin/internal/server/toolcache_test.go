package server

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// runPrepare runs prepareScript with a real /bin/sh against a temporary
// layout: d stands in for toolCachePath, env is main's environment.
func runPrepare(t *testing.T, root string, env ...string) string {
	t.Helper()
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh")
	}
	d := filepath.Join(root, "w", "_tool")
	cmd := exec.Command("/bin/sh", "-c", prepareScript, "sh", d, filepath.Join(root, "w", "_act"), filepath.Join(root, "w", "_temp"))
	cmd.Env = append([]string{"PATH=" + os.Getenv("PATH")}, env...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("prepare script: %v: %s", err, stderr.String())
	}
	for _, dir := range []string{"_act", "_temp"} {
		if fi, err := os.Stat(filepath.Join(root, "w", dir)); err != nil || !fi.IsDir() {
			t.Errorf("%s not created: %v", dir, err)
		}
	}
	return strings.TrimSpace(string(out))
}

func assertSymlink(t *testing.T, link, target string) {
	t.Helper()
	got, err := os.Readlink(link)
	if err != nil || got != target {
		t.Errorf("%s -> %q (%v), want a symlink to %s", link, got, err, target)
	}
}

func assertDir(t *testing.T, p string) {
	t.Helper()
	fi, err := os.Lstat(p)
	if err != nil || !fi.IsDir() {
		t.Errorf("%s must be a plain directory: %v %v", p, fi, err)
	}
}

func TestPrepareScriptToolCache(t *testing.T) {
	t.Run("no image tool cache keeps the shared-volume directory", func(t *testing.T) {
		root := t.TempDir()
		d := filepath.Join(root, "w", "_tool")
		if got := runPrepare(t, root); got != d {
			t.Errorf("resolved %q, want %q", got, d)
		}
		assertDir(t, d)
	})

	t.Run("RUNNER_TOOL_CACHE wins over AGENT_TOOLSDIRECTORY", func(t *testing.T) {
		root := t.TempDir()
		img, agent := filepath.Join(root, "opt", "rtc"), filepath.Join(root, "opt", "agent")
		for _, p := range []string{img, agent} {
			if err := os.MkdirAll(p, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		got := runPrepare(t, root, "RUNNER_TOOL_CACHE="+img, "AGENT_TOOLSDIRECTORY="+agent)
		if got != img {
			t.Errorf("resolved %q, want %q", got, img)
		}
		assertSymlink(t, filepath.Join(root, "w", "_tool"), img)
	})

	t.Run("AGENT_TOOLSDIRECTORY is the fallback and is created if missing", func(t *testing.T) {
		root := t.TempDir()
		agent := filepath.Join(root, "opt", "hostedtoolcache")
		got := runPrepare(t, root, "AGENT_TOOLSDIRECTORY="+agent)
		if got != agent {
			t.Errorf("resolved %q, want %q", got, agent)
		}
		assertDir(t, agent)
		assertSymlink(t, filepath.Join(root, "w", "_tool"), agent)
	})

	t.Run("rerun is idempotent", func(t *testing.T) {
		root := t.TempDir()
		img := filepath.Join(root, "opt", "rtc")
		runPrepare(t, root, "RUNNER_TOOL_CACHE="+img)
		if got := runPrepare(t, root, "RUNNER_TOOL_CACHE="+img); got != img {
			t.Errorf("second run resolved %q, want %q", got, img)
		}
		assertSymlink(t, filepath.Join(root, "w", "_tool"), img)
	})

	t.Run("an existing tool cache (a mounted volume) is left alone", func(t *testing.T) {
		root := t.TempDir()
		d := filepath.Join(root, "w", "_tool")
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if got := runPrepare(t, root, "RUNNER_TOOL_CACHE="+filepath.Join(root, "opt")); got != d {
			t.Errorf("resolved %q, want %q", got, d)
		}
		assertDir(t, d)
	})

	t.Run("a relative path is ignored", func(t *testing.T) {
		root := t.TempDir()
		d := filepath.Join(root, "w", "_tool")
		if got := runPrepare(t, root, "RUNNER_TOOL_CACHE=relative/cache"); got != d {
			t.Errorf("resolved %q, want %q", got, d)
		}
		assertDir(t, d)
	})

	t.Run("an unwritable image tool cache is not used", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root can write anywhere")
		}
		root := t.TempDir()
		img := filepath.Join(root, "opt", "ro")
		if err := os.MkdirAll(img, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(img, 0o555); err != nil {
			t.Fatal(err)
		}
		d := filepath.Join(root, "w", "_tool")
		if got := runPrepare(t, root, "RUNNER_TOOL_CACHE="+img); got != d {
			t.Errorf("resolved %q, want %q", got, d)
		}
		assertDir(t, d)
	})
}

func TestResolvedToolCache(t *testing.T) {
	for out, want := range map[string]string{
		"/opt/hostedtoolcache\n":          "/opt/hostedtoolcache",
		"noise\n/opt/hostedtoolcache\n\n": "/opt/hostedtoolcache",
		"":                                toolCachePath,
		"not a path\n":                    toolCachePath,
	} {
		if got := resolvedToolCache(out); got != want {
			t.Errorf("resolvedToolCache(%q) = %q, want %q", out, got, want)
		}
	}
}

// Start runs one exec that creates the layout and resolves the tool cache,
// reports an image tool cache in the progress stream, and keeps reporting
// /__w/_tool in the layout (the symlink makes it point at the image's).
func TestStartResolvesImageToolCache(t *testing.T) {
	fb := &fakeBackend{exec: func(_ context.Context, cmd []string, _ io.Reader, stdout, _ io.Writer) (int, error) {
		if isPrepare(cmd) {
			_, _ = io.WriteString(stdout, "/opt/hostedtoolcache\n")
		}
		return 0, nil
	}}
	c, _ := setup(t, fb)
	resp := create(t, c, nil)
	if resp.GetToolCachePath() != toolCachePath {
		t.Errorf("tool_cache_path = %q, want %q", resp.GetToolCachePath(), toolCachePath)
	}
	_, progress, err := start(t, c, resp.GetEnvironmentId())
	if err != nil {
		t.Fatal(err)
	}
	var prepares [][]string
	for _, cmd := range fb.execCalls() {
		if isPrepare(cmd) {
			prepares = append(prepares, cmd)
		}
	}
	if len(prepares) != 1 {
		t.Fatalf("want one prepare exec, got %q", prepares)
	}
	if args := prepares[0][4:]; !slices.Equal(args, []string{toolCachePath, actPath, tempPath}) {
		t.Errorf("prepare args = %q", args)
	}
	if p := strings.Join(progress, ""); !strings.Contains(p, "tool cache /__w/_tool -> /opt/hostedtoolcache") {
		t.Errorf("progress = %q", progress)
	}
}

func TestStartDefaultToolCacheIsQuiet(t *testing.T) {
	fb := &fakeBackend{exec: func(_ context.Context, cmd []string, _ io.Reader, stdout, _ io.Writer) (int, error) {
		if isPrepare(cmd) {
			_, _ = io.WriteString(stdout, toolCachePath+"\n")
		}
		return 0, nil
	}}
	c, _ := setup(t, fb)
	_, progress, err := start(t, c, create(t, c, nil).GetEnvironmentId())
	if err != nil {
		t.Fatal(err)
	}
	if p := strings.Join(progress, ""); strings.Contains(p, "tool cache") {
		t.Errorf("default tool cache must not be announced: %q", progress)
	}
}

func isPrepare(cmd []string) bool { return len(cmd) > 2 && cmd[2] == prepareScript }
