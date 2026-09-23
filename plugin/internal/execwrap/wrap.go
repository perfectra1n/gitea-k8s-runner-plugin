// Package execwrap builds the argv sent over pods/exec for one step. Only
// /bin/sh and env are required in the image. User-controlled values (argv,
// env values, workdir) travel as separate arguments and are never spliced
// into shell source.
package execwrap

import (
	"sort"
	"strconv"
	"strings"
)

// wrapScript: $1 pidfile ("" = none), $2 workdir ("" = unchanged), then
// NAME=value... argv. It records its pid (which the command inherits via
// exec) so a cancelled Exec can be killed, enters the workdir, and execs the
// command through env with the step's variables.
const wrapScript = `p=$1; d=$2; shift 2
if [ -n "$p" ]; then mkdir -p "${p%/*}" 2>/dev/null; echo $$ > "$p" 2>/dev/null; fi
if [ -n "$d" ]; then cd "$d" 2>/dev/null || { mkdir -p "$d" && cd "$d"; } || exit 1; fi
exec env "$@"`

// Wrap returns the argv that runs cmd with env in workdir, recording its pid
// in pidfile when set. Every value is its own argument, so nothing is ever
// re-parsed by the shell.
func Wrap(cmd []string, env map[string]string, workdir, pidfile string) []string {
	keys := make([]string, 0, len(env))
	for k := range env {
		if validName(k) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	argv := []string{"/bin/sh", "-c", wrapScript, "sh", pidfile, workdir}
	for _, k := range keys {
		argv = append(argv, k+"="+env[k])
	}
	return append(argv, cmd...)
}

// Kill returns the argv that terminates the command recorded in pidfile and
// everything it started: SIGTERM to the whole process tree, then SIGKILL to
// whatever is left after grace seconds. The tree comes from
// /proc/<pid>/task/*/children, so no ps/pkill is needed in the image.
func Kill(pidfile string, grace int) []string {
	const script = `p=$(cat "$1" 2>/dev/null) || exit 0
[ -n "$p" ] || exit 0
tree() { for c in $(cat /proc/"$1"/task/*/children 2>/dev/null); do tree "$c"; done; echo "$1"; }
pids=$(tree "$p")
kill -TERM $pids 2>/dev/null
i=0
while [ $i -lt "$2" ]; do
  alive=
  for q in $pids; do kill -0 "$q" 2>/dev/null && alive=1; done
  [ -n "$alive" ] || break
  sleep 1; i=$((i+1))
done
kill -KILL $pids 2>/dev/null
rm -f "$1"
exit 0`
	return []string{"/bin/sh", "-c", script, "sh", pidfile, strconv.Itoa(grace)}
}

// validName reports whether env(1) will treat name=value as an assignment.
func validName(k string) bool {
	return k != "" && !strings.ContainsAny(k, "=\x00")
}
