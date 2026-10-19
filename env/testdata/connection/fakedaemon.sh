#!/bin/sh
# A misbehaving pi-env daemon for connection_test.go and capture.mts, started
# as `sh fakedaemon.sh <mode> serve --token <hex>`. It answers the hello
# (request 1) without reading it.
mode=$1
token=$4
be32() {
	printf "\\$(printf %03o $((($1 >> 24) & 255)))\\$(printf %03o $((($1 >> 16) & 255)))"
	printf "\\$(printf %03o $((($1 >> 8) & 255)))\\$(printf %03o $(($1 & 255)))"
}
# frame <type> <id> <json>
frame() {
	be32 $((9 + ${#3}))
	printf "\\$(printf %03o "$1")"
	be32 "$2"
	be32 ${#3}
	printf '%s' "$3"
}
hello='{"protocol":1,"version":"0.0.0","os":"linux","arch":"x86_64","home":"/h","tmpdir":"/t","separator":"/","cwd":"/","driveCwds":{},"pid":1}'
case $mode in
exit)
	echo "boom" >&2
	exit 7
	;;
protocol)
	printf 'PI-ENV %s\n' "$token"
	frame 2 1 '{"protocol":2,"version":"9"}'
	;;
noprotocol)
	printf 'PI-ENV %s\n' "$token"
	frame 2 1 '{"version":"9"}'
	;;
corrupt)
	printf 'PI-ENV %s\n' "$token"
	printf '\377\377\377\377'
	;;
short)
	printf 'PI-ENV %s\n' "$token"
	be32 8
	printf '\002\000\000\000\001\000\000\000'
	;;
array)
	printf 'PI-ENV %s\n' "$token"
	frame 2 1 '[1]'
	;;
refuse)
	printf 'PI-ENV %s\n' "$token"
	frame 3 1 '{"code":"EINVAL","message":"hello refused","extra":true}'
	;;
noise)
	printf '  Welcome to the box\nlast login: never \n'
	printf 'PI-ENV %s\n' "$token"
	frame 2 1 "$hello"
	;;
stderr)
	printf 'PI-ENV %s\n' "$token"
	printf 'diagnostic line\n' >&2
	frame 2 1 "$hello"
	;;
posix)
	printf 'PI-ENV %s\n' "$token"
	frame 2 1 '{"protocol":1,"version":"0.0.0","os":"linux","arch":"x86_64","home":"/home/me","tmpdir":"/tmp","separator":"/","cwd":"/srv","driveCwds":{},"pid":1}'
	;;
windows)
	printf 'PI-ENV %s\n' "$token"
	frame 2 1 '{"protocol":1,"version":"0.0.0","os":"windows","arch":"x86_64","home":"C:\\Users\\me","tmpdir":"C:\\Temp","separator":"\\","cwd":"D:\\daemon","driveCwds":{"E:":"E:\\e-cwd","F:":"C:\\elsewhere"},"pid":1}'
	;;
chatty)
	# Shell noise on stdout and a diagnostic on stderr at once.
	printf 'noise\n'
	printf 'diagnostic\n' >&2
	printf 'PI-ENV %s\n' "$token"
	frame 2 1 "$hello"
	;;
silent)
	# A daemon that hangs once it answered: it neither reads nor pings.
	printf 'PI-ENV %s\n' "$token"
	frame 2 1 "$hello"
	exec sleep 3600
	;;
esac
cat > /dev/null
