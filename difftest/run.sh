#!/usr/bin/env bash
# pi-vs-Go differential request-body harness.
#
#   ./run.sh                    regenerate both sides, diff, classify, exit
#   ./run.sh --only <scenario>  just one scenario
#   ./run.sh --list             list scenarios
#
# Per scenario: PASS / KNOWN (accepted debt in known-divergences.json) / FAIL /
# FIXED (stale baseline entry). Exit 0 = all PASS or KNOWN, 1 = any FAIL,
# 3 = no FAIL but a stale baseline entry. See README.md.
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$HERE"
set -a
# shellcheck source=/dev/null
. ./config.env
set +a

ONLY=""
while [ $# -gt 0 ]; do
	case "$1" in
	--only)
		ONLY="${2:-}"
		shift 2
		;;
	--list)
		for f in scenarios/*.json; do basename "$f" .json; done
		exit 0
		;;
	-h | --help)
		sed -n '2,10p' "$0"
		exit 0
		;;
	*)
		echo "unknown flag: $1" >&2
		exit 2
		;;
	esac
done
export DIFF_ONLY="$ONLY"

red() { printf '\033[31m%s\033[0m\n' "$*"; }
green() { printf '\033[32m%s\033[0m\n' "$*"; }
info() { printf '\033[2m%s\033[0m\n' "$*"; }

# --- 1. Make sure real pi is available -------------------------------------

if [ ! -d "$PI_NPM_DIR/node_modules/@earendil-works/pi-ai" ]; then
	info "== installing @earendil-works/pi-ai@$PI_NPM_VERSION into $PI_NPM_DIR"
	mkdir -p "$PI_NPM_DIR"
	cat >"$PI_NPM_DIR/package.json" <<EOF
{"name":"pi-ref","private":true,"version":"1.0.0","dependencies":{"@earendil-works/pi-ai":"$PI_NPM_VERSION","@earendil-works/pi-coding-agent":"$PI_NPM_VERSION"}}
EOF
	(cd "$PI_NPM_DIR" && npm i --silent) || {
		red "npm install failed"
		exit 1
	}
fi

# Authenticity: the lockfile hash must match what the registry serves.
verify_integrity() {
	local pkg="$1" want have
	want="$(npm view "$pkg@$PI_NPM_VERSION" dist.integrity 2>/dev/null | tail -1)"
	have="$(python3 -c "
import json,sys
d=json.load(open('$PI_NPM_DIR/package-lock.json'))
print(d['packages'].get('node_modules/$pkg',{}).get('integrity',''))
" 2>/dev/null)"
	if [ -z "$want" ]; then
		info "integrity: SKIPPED for $pkg (registry unreachable)"
	elif [ "$want" = "$have" ]; then
		info "integrity: OK $pkg@$PI_NPM_VERSION"
	else
		red "integrity: MISMATCH for $pkg@$PI_NPM_VERSION"
		red "  registry: $want"
		red "  lockfile: $have"
		exit 1
	fi
}
verify_integrity "@earendil-works/pi-ai"
verify_integrity "@earendil-works/pi-coding-agent"

# --- 2. Upstream TS at the synced sha (for unreleased changes) --------------
# Scenarios marked "backend": "src" build against pi's TypeScript source,
# because the change they cover was ported before it was ever released and the
# published build simply does not contain it.
#
# Skipped entirely when no scenario asks for it. The suite empties of "src"
# scenarios every time a release ships the surface they cover, and extracting
# then would make the upstream clone and a resolvable PI_UPSTREAM_SHA hard
# requirements of a run that reads neither, which is a fresh-clone and CI
# failure for nothing.

SRC="$HERE/pisrc/$PI_UPSTREAM_SHA"
if grep -lq '"backend"[[:space:]]*:[[:space:]]*"src"' "$HERE"/scenarios/*.json 2>/dev/null; then
	if [ ! -d "$SRC/src" ]; then
		info "== extracting upstream packages/ai @ $PI_UPSTREAM_SHA (read-only, via git archive)"
		rm -rf "$SRC"
		mkdir -p "$SRC"
		git -C "$PI_UPSTREAM_DIR" archive "$PI_UPSTREAM_SHA" packages/ai |
			tar -x -C "$SRC" --strip-components=2 || {
			red "could not extract $PI_UPSTREAM_SHA from $PI_UPSTREAM_DIR"
			exit 1
		}
	fi
	# Node resolves bare imports through $SRC/node_modules: third-party ones
	# (openai, typebox, ...) from the published build, and the workspace
	# packages pi-ai depends on (@earendil-works/pi-telemetry) from the same
	# sha, since src imports what they have there. See pi/srclink.mjs.
	node pi/srclink.mjs "$SRC" "$PI_NPM_DIR/node_modules" "$PI_UPSTREAM_DIR" "$PI_UPSTREAM_SHA" || {
		red "could not link the src backend's node_modules at $PI_UPSTREAM_SHA"
		exit 1
	}
else
	info "== no \"backend\": \"src\" scenarios; skipping the upstream TS extraction"
fi

# --- 3. Regenerate both sides ----------------------------------------------

rm -rf out
mkdir -p out/pi out/go

echo
node pi/capture.mjs || {
	red "pi side failed"
	exit 1
}
echo
(cd port && go run .) || {
	red "go side failed"
	exit 1
}

# --- 4. Diff and classify --------------------------------------------------
# classify.py compares the two sides structurally and checks every difference
# it finds against known-divergences.json, yielding four states per scenario:
#
#   PASS   identical
#   KNOWN  differs, and the difference matches a baseline entry exactly
#          (same scenario AND same JSON path AND same kind of difference)
#   FAIL   differs in any way the baseline does not cover
#   FIXED  a baseline entry that no longer fires — stale debt, must be retired
#
# Exit: 0 = all PASS/KNOWN, 1 = any FAIL, 3 = no FAIL but a stale entry.

scen=()
for f in scenarios/*.json; do
	name="$(python3 -c "import json,sys;print(json.load(open('$f'))['name'])")"
	[ -n "$ONLY" ] && [ "$name" != "$ONLY" ] && continue
	scen+=("$f")
done

if [ "${#scen[@]}" -eq 0 ]; then
	red "no scenario matched --only '$ONLY'"
	exit 2
fi

python3 classify.py "${scen[@]}"
exit $?
