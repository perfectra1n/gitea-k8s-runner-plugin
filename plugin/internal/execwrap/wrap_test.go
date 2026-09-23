package execwrap

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The wrapper is plain POSIX sh; run it for real against the local /bin/sh.
func run(t *testing.T, argv []string) (string, int) {
	t.Helper()
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "INHERITED=yes"}
	out, err := cmd.CombinedOutput()
	code := 0
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return string(out), code
}

func TestWrapAppliesEnvAndWorkdir(t *testing.T) {
	dir := t.TempDir()
	out, code := run(t, Wrap(
		[]string{"sh", "-c", `pwd; printf '%s|%s|%s\n' "$FOO" "$BAR" "$INHERITED"`},
		map[string]string{"FOO": "a b", "BAR": `$(echo pwned) "q" 'x'`},
		dir, "",
	))
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	real, _ := filepath.EvalSymlinks(dir)
	if lines[0] != dir && lines[0] != real {
		t.Errorf("pwd = %q, want %q", lines[0], dir)
	}
	if want := `a b|$(echo pwned) "q" 'x'|yes`; lines[1] != want {
		t.Errorf("env line = %q, want %q", lines[1], want)
	}
}

func TestWrapArgvIsNotReinterpreted(t *testing.T) {
	out, code := run(t, Wrap([]string{"printf", "%s\n", "$HOME", "a;b", "*"}, nil, "", ""))
	if code != 0 || out != "$HOME\na;b\n*\n" {
		t.Fatalf("exit %d, out %q", code, out)
	}
}

func TestWrapPropagatesExitCode(t *testing.T) {
	if _, code := run(t, Wrap([]string{"sh", "-c", "exit 3"}, nil, "", "")); code != 3 {
		t.Fatalf("exit = %d, want 3", code)
	}
}

func TestWrapCreatesMissingWorkdir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a", "b")
	out, code := run(t, Wrap([]string{"pwd"}, nil, dir, ""))
	if code != 0 || !strings.HasSuffix(strings.TrimSpace(out), "/a/b") {
		t.Fatalf("exit %d, out %q", code, out)
	}
}

func TestWrapWritesPidfile(t *testing.T) {
	pidfile := filepath.Join(t.TempDir(), "sub", "x.pid")
	out, code := run(t, Wrap([]string{"sh", "-c", "echo $$"}, nil, "", pidfile))
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	b, err := os.ReadFile(pidfile)
	if err != nil {
		t.Fatal(err)
	}
	// exec replaces the wrapper, so the recorded pid is the command's own.
	if strings.TrimSpace(string(b)) != strings.TrimSpace(out) {
		t.Errorf("pidfile %q, command pid %q", b, out)
	}
}

func TestWrapRejectsEnvNamesThatWouldBreakEnv(t *testing.T) {
	argv := Wrap([]string{"true"}, map[string]string{"A=B": "c", "": "x", "OK": "1"}, "", "")
	joined := strings.Join(argv, "\x00")
	if strings.Contains(joined, "A=B=c") || strings.Contains(joined, "\x00=x") {
		t.Errorf("invalid names must be dropped: %q", argv)
	}
	if !strings.Contains(joined, "OK=1") {
		t.Errorf("valid name missing: %q", argv)
	}
}

func TestKillCommandKillsRecordedPid(t *testing.T) {
	pidfile := filepath.Join(t.TempDir(), "x.pid")
	c := exec.Command(Wrap([]string{"sleep", "30"}, nil, "", pidfile)[0], Wrap([]string{"sleep", "30"}, nil, "", pidfile)[1:]...)
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	for range 100 {
		if _, err := os.Stat(pidfile); err == nil {
			break
		}
		exec.Command("sleep", "0.02").Run() //nolint:errcheck // best-effort pause
	}
	k := Kill(pidfile, 0)
	if out, err := exec.Command(k[0], k[1:]...).CombinedOutput(); err != nil {
		t.Fatalf("kill: %v %s", err, out)
	}
	if err := c.Wait(); err == nil {
		t.Fatal("sleep should have been killed")
	}
}

// Cancelling a step must also stop what it started in the background.
func TestKillCommandKillsDescendants(t *testing.T) {
	dir := t.TempDir()
	pidfile := filepath.Join(dir, "x.pid")
	childPid := filepath.Join(dir, "child.pid")
	argv := Wrap([]string{"sh", "-c", `sleep 30 & echo $! > "$0"; wait`, childPid}, nil, "", pidfile)
	c := exec.Command(argv[0], argv[1:]...)
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	var child string
	for range 200 {
		if b, err := os.ReadFile(childPid); err == nil && len(strings.TrimSpace(string(b))) > 0 {
			child = strings.TrimSpace(string(b))
			break
		}
		exec.Command("sleep", "0.02").Run() //nolint:errcheck // best-effort pause
	}
	if child == "" {
		t.Fatal("child never started")
	}
	k := Kill(pidfile, 0)
	if out, err := exec.Command(k[0], k[1:]...).CombinedOutput(); err != nil {
		t.Fatalf("kill: %v %s", err, out)
	}
	_ = c.Wait()
	for range 100 {
		if exec.Command("kill", "-0", child).Run() != nil {
			return // gone
		}
		exec.Command("sleep", "0.02").Run() //nolint:errcheck // best-effort pause
	}
	_ = exec.Command("kill", "-KILL", child).Run()
	t.Fatalf("background child %s survived the kill", child)
}
