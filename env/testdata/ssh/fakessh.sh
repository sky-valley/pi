#!/bin/sh
# A stand-in for ssh: call n records its arguments (NUL-separated) in
# $FAKE_SSH_DIR/n.argv and its stdin in n.stdin, then answers with n.out,
# n.err and the exit code in n.code. A call with an n.nostdin file leaves
# stdin alone (the daemon's launch keeps it open).
dir=$FAKE_SSH_DIR
n=0
while [ -e "$dir/$n.argv" ]; do n=$((n + 1)); done
for argument in "$@"; do printf '%s\0' "$argument"; done > "$dir/$n.argv"
if [ ! -e "$dir/$n.code" ]; then
	echo "fake ssh: no answer for call $n" >&2
	exit 99
fi
if [ ! -e "$dir/$n.nostdin" ]; then cat > "$dir/$n.stdin"; fi
[ -e "$dir/$n.out" ] && cat "$dir/$n.out"
[ -e "$dir/$n.err" ] && cat "$dir/$n.err" >&2
exit "$(cat "$dir/$n.code")"
