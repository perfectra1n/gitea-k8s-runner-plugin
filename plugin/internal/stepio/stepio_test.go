package stepio

import (
	"bytes"
	"errors"
	"reflect"
	"testing"
)

func frames() []byte {
	var b []byte
	b = AppendFrame(b, KindStdout, []byte("hello "))
	b = AppendFrame(b, KindStderr, []byte("oops\n"))
	b = AppendFrame(b, KindStdout, []byte("world\n"))
	return append(b, ExitFrame(7)...)
}

func TestDecoderRoundTrip(t *testing.T) {
	var out, errb bytes.Buffer
	d := &Decoder{Stdout: &out, Stderr: &errb}
	raw := frames()
	if _, err := d.Write(raw); err != nil {
		t.Fatal(err)
	}
	if out.String() != "hello world\n" || errb.String() != "oops\n" {
		t.Fatalf("stdout %q, stderr %q", out.String(), errb.String())
	}
	code, ok := d.Exit()
	if !ok || code != 7 {
		t.Fatalf("exit = %d, %v; want 7, true", code, ok)
	}
	if d.Offset() != int64(len(raw)) {
		t.Fatalf("offset = %d, want %d", d.Offset(), len(raw))
	}
}

// A stream may break anywhere, including inside a header; resuming from
// Offset() must yield exactly the same output as one unbroken read.
func TestDecoderResumesAtEveryByte(t *testing.T) {
	raw := frames()
	for cut := range len(raw) + 1 {
		var out, errb bytes.Buffer
		d := &Decoder{Stdout: &out, Stderr: &errb}
		if _, err := d.Write(raw[:cut]); err != nil {
			t.Fatalf("cut %d: %v", cut, err)
		}
		if _, ok := d.Exit(); ok && cut < len(raw) {
			t.Fatalf("cut %d: exit before the exit record completed", cut)
		}
		for _, b := range raw[d.Offset():] {
			if _, err := d.Write([]byte{b}); err != nil {
				t.Fatalf("cut %d: %v", cut, err)
			}
		}
		if out.String() != "hello world\n" || errb.String() != "oops\n" {
			t.Fatalf("cut %d: stdout %q, stderr %q", cut, out.String(), errb.String())
		}
		if code, ok := d.Exit(); !ok || code != 7 {
			t.Fatalf("cut %d: exit = %d, %v", cut, code, ok)
		}
	}
}

func TestDecoderNegativeExit(t *testing.T) {
	d := &Decoder{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}
	if _, err := d.Write(ExitFrame(-1)); err != nil {
		t.Fatal(err)
	}
	if code, ok := d.Exit(); !ok || code != -1 {
		t.Fatalf("exit = %d, %v", code, ok)
	}
}

func TestDecoderRejectsCorruption(t *testing.T) {
	for name, raw := range map[string][]byte{
		"unknown kind":   {9, 0, 0, 0, 1, 'x'},
		"oversized":      {KindStdout, 0xff, 0, 0, 0},
		"bad exit size":  {KindExit, 0, 0, 0, 1, 0},
		"data after end": append(ExitFrame(0), AppendFrame(nil, KindStdout, []byte("x"))...),
	} {
		d := &Decoder{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}
		if _, err := d.Write(raw); !errors.Is(err, ErrCorrupt) {
			t.Errorf("%s: err = %v, want ErrCorrupt", name, err)
		}
	}
}

func TestRunArgvRoundTrip(t *testing.T) {
	argv := RunArgv("/h/step", "/w/.p/steps/3", "/w/.p/exec-3.pid", "/w/ws", map[string]string{
		"B": "2", "A": "x=y -- z", "": "dropped", "BAD=NAME": "dropped",
	}, []string{"sh", "-c", "echo -- hi", "--"})
	want := []string{"/h/step", "run", "/w/.p/steps/3", "/w/.p/exec-3.pid", "/w/ws", "A=x=y -- z", "B=2", "--", "sh", "-c", "echo -- hi", "--"}
	if !reflect.DeepEqual(argv, want) {
		t.Fatalf("argv = %q\nwant   %q", argv, want)
	}
	spec, err := ParseRun(argv[2:])
	if err != nil {
		t.Fatal(err)
	}
	wantSpec := RunSpec{
		Dir: "/w/.p/steps/3", Pidfile: "/w/.p/exec-3.pid", Workdir: "/w/ws",
		Env: []string{"A=x=y -- z", "B=2"}, Cmd: []string{"sh", "-c", "echo -- hi", "--"},
	}
	if !reflect.DeepEqual(spec, wantSpec) {
		t.Fatalf("spec = %+v\nwant   %+v", spec, wantSpec)
	}
}

func TestParseRunErrors(t *testing.T) {
	for name, args := range map[string][]string{
		"too short":    {"/d", "/p"},
		"no separator": {"/d", "/p", "", "A=1", "sh"},
		"no command":   {"/d", "/p", "", "--"},
		"bad env":      {"/d", "/p", "", "NOEQUALS", "--", "sh"},
		"relative dir": {"d", "/p", "", "--", "sh"},
	} {
		if _, err := ParseRun(args); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestFollowArgv(t *testing.T) {
	got := FollowArgv("/h/step", "/w/s/1", 1234)
	want := []string{"/h/step", "follow", "/w/s/1", "1234"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("argv = %q, want %q", got, want)
	}
	dir, off, err := ParseFollow(got[2:])
	if err != nil || dir != "/w/s/1" || off != 1234 {
		t.Fatalf("ParseFollow = %q, %d, %v", dir, off, err)
	}
	if _, _, err := ParseFollow([]string{"/w", "-1"}); err == nil {
		t.Fatal("negative offset accepted")
	}
}
