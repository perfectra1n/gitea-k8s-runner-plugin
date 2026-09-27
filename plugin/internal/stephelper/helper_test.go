//go:build linux

package stephelper

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/perfectra1n/gitea-k8s-runner-plugin/plugin/internal/stepio"
)

// run re-execs os.Executable() as the supervisor: here that is the test
// binary, so dispatch to Main before the testing package parses flags.
// TestSecretsStayOffCommandLines also runs the binary as the helper itself.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "run", "supervise", "follow":
			drainGrace = 300 * time.Millisecond
			os.Exit(Main(os.Args[1:], os.Stdout, os.Stderr))
		}
	}
	pollInterval = 10 * time.Millisecond
	livenessEvery = 5
	// In-process runs must not exec over the test binary.
	becomeFollower = func(dir string, stdout, stderr io.Writer) int { return follow(dir, 0, stdout, stderr) }
	os.Exit(m.Run())
}

type result struct {
	stdout, stderr string
	exit           int
	exited         bool
	helperCode     int
	helperErr      string
}

func decode(t *testing.T, raw []byte) (*stepio.Decoder, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	var out, errb bytes.Buffer
	d := &stepio.Decoder{Stdout: &out, Stderr: &errb}
	if _, err := d.Write(raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return d, &out, &errb
}

func runStep(t *testing.T, dir string, env map[string]string, workdir string, cmd ...string) result {
	t.Helper()
	var raw, herr bytes.Buffer
	argv := stepio.RunArgv("helper", dir, "", workdir, env, cmd)
	code := Main(argv[1:], &raw, &herr)
	d, out, errb := decode(t, raw.Bytes())
	exit, ok := d.Exit()
	return result{out.String(), errb.String(), exit, ok, code, herr.String()}
}

func TestRunRecordsOutputAndExitCode(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "steps", "1")
	r := runStep(t, dir, nil, "", "sh", "-c", "echo out; echo err >&2; exit 3")
	if r.helperCode != 0 || !r.exited || r.exit != 3 {
		t.Fatalf("helper %d (%s), exit %d/%v", r.helperCode, r.helperErr, r.exit, r.exited)
	}
	if r.stdout != "out\n" || r.stderr != "err\n" {
		t.Fatalf("stdout %q, stderr %q", r.stdout, r.stderr)
	}
}

func TestRunAppliesEnvWorkdirAndStepPATH(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "only-in-step-path"), []byte("#!/bin/sh\necho found \"$FOO\" \"$(pwd)\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(root, "a", "b")
	r := runStep(t, filepath.Join(root, "s", "1"), map[string]string{
		"PATH": bin + ":" + os.Getenv("PATH"), "FOO": "a b $(x)",
	}, work, "only-in-step-path")
	real, _ := filepath.EvalSymlinks(work)
	if r.exit != 0 || (r.stdout != "found a b $(x) "+work+"\n" && r.stdout != "found a b $(x) "+real+"\n") {
		t.Fatalf("exit %d, stdout %q, stderr %q", r.exit, r.stdout, r.stderr)
	}
}

func TestRunMissingCommandIs127(t *testing.T) {
	r := runStep(t, filepath.Join(t.TempDir(), "s", "1"), nil, "", "no-such-command-xyz")
	if !r.exited || r.exit != 127 || !strings.Contains(r.stderr, "no-such-command-xyz") {
		t.Fatalf("exit %d/%v, stderr %q", r.exit, r.exited, r.stderr)
	}
}

func TestRunSignalledIs128PlusSignal(t *testing.T) {
	r := runStep(t, filepath.Join(t.TempDir(), "s", "1"), nil, "", "sh", "-c", "kill -9 $$")
	if r.exit != 137 {
		t.Fatalf("exit %d, want 137", r.exit)
	}
}

// A process left running with the step's stdout must neither hang the step
// nor be killed when the step ends.
func TestRunDoesNotWaitForBackgroundProcesses(t *testing.T) {
	root := t.TempDir()
	pidf := filepath.Join(root, "bg.pid")
	start := time.Now()
	r := runStep(t, filepath.Join(root, "s", "1"), nil, "", "sh", "-c", "sleep 30 & echo $! > "+pidf+"; echo hi")
	if r.exit != 0 || r.stdout != "hi\n" {
		t.Fatalf("exit %d, stdout %q", r.exit, r.stdout)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("took %s", d)
	}
	b, _ := os.ReadFile(pidf)
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	if _, _, ok := procStat(pid); !ok {
		t.Fatalf("background process %d was killed", pid)
	}
	p, _ := os.FindProcess(pid)
	_ = p.Kill()
}

// breakingWriter accepts limit bytes and then fails, like an exec stream
// whose connection was reset.
type breakingWriter struct {
	mu    sync.Mutex
	buf   bytes.Buffer
	limit int
}

var errReset = errors.New("connection reset by peer")

func (w *breakingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	room := w.limit - w.buf.Len()
	if room <= 0 {
		return 0, errReset
	}
	if len(p) > room {
		w.buf.Write(p[:room])
		return room, errReset
	}
	return w.buf.Write(p)
}

func TestFollowResumesABrokenStream(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "s", "1")
	pidfile := filepath.Join(root, "exec.pid")
	script := `for i in 1 2 3 4 5 6 7 8; do echo "line $i"; echo "err $i" >&2; sleep 0.05; done; exit 4`
	bw := &breakingWriter{limit: 23} // inside the second frame's header
	argv := stepio.RunArgv("helper", dir, pidfile, "", nil, []string{"sh", "-c", script})
	var herr bytes.Buffer
	if code := Main(argv[1:], bw, &herr); code != 1 || !strings.Contains(herr.String(), "connection reset") {
		t.Fatalf("broken run: code %d, stderr %q", code, herr.String())
	}
	var out, errb bytes.Buffer
	d := &stepio.Decoder{Stdout: &out, Stderr: &errb}
	if _, err := d.Write(bw.buf.Bytes()); err != nil {
		t.Fatal(err)
	}
	// The step keeps running while nobody follows it.
	if b, err := os.ReadFile(pidfile); err != nil || strings.TrimSpace(string(b)) == "" {
		t.Fatalf("pidfile: %q, %v", b, err)
	}
	var raw bytes.Buffer
	fargv := stepio.FollowArgv("helper", dir, d.Offset())
	if code := Main(fargv[1:], &raw, &herr); code != 0 {
		t.Fatalf("follow: code %d, stderr %q", code, herr.String())
	}
	if _, err := d.Write(raw.Bytes()); err != nil {
		t.Fatal(err)
	}
	var wantOut, wantErr strings.Builder
	for i := 1; i <= 8; i++ {
		fmt.Fprintf(&wantOut, "line %d\n", i)
		fmt.Fprintf(&wantErr, "err %d\n", i)
	}
	if out.String() != wantOut.String() || errb.String() != wantErr.String() {
		t.Fatalf("stdout %q\nstderr %q", out.String(), errb.String())
	}
	if code, ok := d.Exit(); !ok || code != 4 {
		t.Fatalf("exit %d/%v, want 4", code, ok)
	}
}

func TestFollowReportsAVanishedSupervisor(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	logged := stepio.AppendFrame(nil, stepio.KindStdout, []byte("partial\n"))
	if err := os.WriteFile(filepath.Join(dir, logFile), logged, 0o644); err != nil {
		t.Fatal(err)
	}
	// Our own pid with a wrong start time: a pid reused after a restart.
	if err := os.WriteFile(filepath.Join(dir, supervisorFile), []byte(fmt.Sprintf("%d 1", os.Getpid())), 0o644); err != nil {
		t.Fatal(err)
	}
	var raw, herr bytes.Buffer
	if code := follow(dir, 0, &raw, &herr); code != exitSupervisorGone {
		t.Fatalf("code %d, stderr %q", code, herr.String())
	}
	if !bytes.Equal(raw.Bytes(), logged) || !strings.Contains(herr.String(), "supervisor process is gone") {
		t.Fatalf("raw %q, stderr %q", raw.Bytes(), herr.String())
	}
}

func TestFollowMissingLog(t *testing.T) {
	var herr bytes.Buffer
	if code := follow(t.TempDir(), 0, &bytes.Buffer{}, &herr); code != exitNoLog {
		t.Fatalf("code %d", code)
	}
}

func TestRunCollectsFinishedSteps(t *testing.T) {
	steps := filepath.Join(t.TempDir(), "steps")
	done := filepath.Join(steps, "1")
	orphan := filepath.Join(steps, "2")
	live := filepath.Join(steps, "3")
	for _, d := range []string{done, orphan, live} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	_ = os.WriteFile(filepath.Join(done, doneFile), nil, 0o644)
	_ = os.WriteFile(filepath.Join(orphan, supervisorFile), []byte("999999999 1"), 0o644)
	self := os.Getpid()
	_ = os.WriteFile(filepath.Join(live, supervisorFile), []byte(fmt.Sprintf("%d %d", self, startTime(self))), 0o644)

	if r := runStep(t, filepath.Join(steps, "4"), nil, "", "true"); r.exit != 0 {
		t.Fatalf("exit %d", r.exit)
	}
	for d, want := range map[string]bool{done: false, orphan: false, live: true} {
		if exists(d) != want {
			t.Errorf("%s exists = %v, want %v", filepath.Base(d), !want, want)
		}
	}
}

// A repeated run (its first exec's stream broke before anything arrived, the
// first run may still be starting the step) follows the step instead of
// starting it again.
func TestRunIsIdempotent(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "s", "1")
	count := filepath.Join(root, "count")
	script := `echo x >> "$1"; echo ran`
	first := runStep(t, dir, nil, "", "sh", "-c", script, "sh", count)
	second := runStep(t, dir, nil, "", "sh", "-c", script, "sh", count)
	for _, r := range []result{first, second} {
		if r.helperCode != 0 || r.exit != 0 || r.stdout != "ran\n" {
			t.Fatalf("exit %d, stdout %q, helper %d: %s", r.exit, r.stdout, r.helperCode, r.helperErr)
		}
	}
	if b, _ := os.ReadFile(count); string(b) != "x\n" {
		t.Fatalf("step ran %d times", strings.Count(string(b), "x"))
	}
}

func TestInstall(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "bin", "step")
	if code := Main([]string{"install", dest}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("code %d", code)
	}
	fi, err := os.Stat(dest)
	if err != nil || fi.Mode().Perm() != 0o755 || fi.Size() == 0 {
		t.Fatalf("stat: %v, %v", fi, err)
	}
}

func TestMainUsage(t *testing.T) {
	for _, args := range [][]string{nil, {"bogus"}, {"install"}, {"run", "rel", "", "", "--", "x"}, {"follow", "/x"}} {
		if code := Main(args, &bytes.Buffer{}, &bytes.Buffer{}); code != exitUsage {
			t.Errorf("%q: code %d, want %d", args, code, exitUsage)
		}
	}
}

// A run whose stream broke before the log existed may be retried.
func TestRunAcceptsADirWithoutLog(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "s", "1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if r := runStep(t, dir, nil, "", "sh", "-c", "echo ok"); r.exit != 0 || r.stdout != "ok\n" {
		t.Fatalf("exit %d, stdout %q (%s)", r.exit, r.stdout, r.helperErr)
	}
}

// A step's env holds secrets; /proc/<pid>/cmdline is world-readable, so no
// long-lived helper process may carry it on its command line.
func TestSecretsStayOffCommandLines(t *testing.T) {
	const secret = "s3cret-8d1f0c2b"
	root := t.TempDir()
	dir := filepath.Join(root, "s", "1")
	marker := filepath.Join(root, "started")
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	argv := stepio.RunArgv(self, dir, "", "", map[string]string{"TOKEN": secret},
		[]string{"sh", "-c", `touch "$1"; printf '%s\n' "$TOKEN"; sleep 2`, "sh", marker})
	var raw, herr bytes.Buffer
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdout, cmd.Stderr = &raw, &herr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); !exists(marker); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("step never started: %s", herr.String())
		}
	}
	procs, _ := filepath.Glob("/proc/[0-9]*/cmdline")
	for _, p := range procs {
		if b, err := os.ReadFile(p); err == nil && bytes.Contains(b, []byte(secret)) {
			t.Errorf("%s carries the secret: %q", p, bytes.ReplaceAll(b, []byte{0}, []byte{' '}))
		}
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("helper: %v: %s", err, herr.String())
	}
	d, out, _ := decode(t, raw.Bytes())
	if code, ok := d.Exit(); !ok || code != 0 || out.String() != secret+"\n" {
		t.Fatalf("exit %d/%v, stdout %q", code, ok, out.String())
	}
}
