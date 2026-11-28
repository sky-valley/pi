#!/usr/bin/env bash
# Publishes every release requested under docs/releases/ that is not fully out
# yet: the root tag vX.Y.Z, env/go.mod's require of it, the env/vX.Y.Z tag and
# the GitHub Release. Each step is skipped when it is already done, so a re-run
# resumes a release that failed part way.
#
# pi-sync §5 writes one request per release, docs/releases/vX.Y.Z.md:
#
#   ---
#   version: vX.Y.Z
#   commit: <full sha the root tag names: the sync's ledger/pin-advance commit>
#   title: vX.Y.Z — tracking pi <npm-ver>
#   tag_message: vX.Y.Z — upstream pin <sha>, npm pi-ai <ver> (release <sha>): <headline>
#   env_tag_message: env/vX.Y.Z — pi-env <ver>, requires root vX.Y.Z
#   ---
#   <the GitHub Release body, markdown>
#
# It runs in .github/workflows/release.yml, because the cloud sync can push
# main but not tags. Locally, DRY_RUN=1 .github/scripts/release.sh prints what
# it would do and changes nothing.
set -euo pipefail

TAGGER_NAME="Noam Y. Tenne"
TAGGER_EMAIL="noam@10ne.org"
COMMITTER_NAME="github-actions[bot]"
COMMITTER_EMAIL="41898282+github-actions[bot]@users.noreply.github.com"
REPO="${GITHUB_REPOSITORY:-sky-valley/pi}"
DRY_RUN="${DRY_RUN:-}"
export GOWORK=off

die() { echo "release: $*" >&2; exit 1; }

# run executes its arguments, or under DRY_RUN prints them.
run() {
	if [[ -n $DRY_RUN ]]; then
		printf 'DRY_RUN:'
		printf ' %q' "$@"
		echo
	else
		"$@"
	fi
}

# field prints the value of key in the request file's front matter.
field() {
	awk -v key="$2" '
		NR == 1 { if ($0 != "---") exit 1; next }
		$0 == "---" { exit }
		index($0, key ": ") == 1 { print substr($0, length(key) + 3); found = 1; exit }
		END { if (!found) exit 1 }
	' "$1"
}

# body prints the request file after its front matter.
body() {
	awk 'NR == 1 { next } !done && $0 == "---" { done = 1; next } done' "$1"
}

# push_main pushes the current branch to main, rebasing onto main first if
# something else landed meanwhile.
push_main() {
	local attempt
	for attempt in 1 2 3; do
		if run git push -q origin HEAD:refs/heads/main; then
			return 0
		fi
		run git pull -q --rebase origin main
	done
	die "could not push to main"
}

# env_bump_commit prints the newest commit on main whose env/go.mod requires
# the root at $1, if there is one.
env_bump_commit() {
	local v=$1 c
	for c in $(git log --format=%H -G "sky-valley/pi ${v//./\\.}\$" origin/main -- env/go.mod); do
		if git show "$c:env/go.mod" | grep -qxF "	github.com/sky-valley/pi $v"; then
			echo "$c"
			return
		fi
	done
}

publish() {
	local f=$1 v commit title tag_msg env_tag_msg tagged bump latest notes
	v=$(field "$f" version) || die "$f: no version"
	[[ $v =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || die "$f: version '$v' is not vX.Y.Z"
	[[ $(basename "$f" .md) == "$v" ]] || die "$f: the file name does not match version $v"
	commit=$(field "$f" commit) || die "$f: no commit"
	title=$(field "$f" title) || die "$f: no title"
	tag_msg=$(field "$f" tag_message) || die "$f: no tag_message"
	env_tag_msg=$(field "$f" env_tag_message) || die "$f: no env_tag_message"
	commit=$(git rev-parse -q --verify "$commit^{commit}") || die "$f: commit $commit is not in the repo"
	git merge-base --is-ancestor "$commit" origin/main || die "$f: $commit is not on main"

	# 1. The root tag.
	if tagged=$(git rev-parse -q --verify "refs/tags/$v^{commit}"); then
		[[ $tagged == "$commit" ]] || die "$v already tags $tagged, not $commit"
		echo "$v: tag exists"
	else
		run git -c user.name="$TAGGER_NAME" -c user.email="$TAGGER_EMAIL" tag -a "$v" "$commit" -m "$tag_msg"
		run git push -q origin "refs/tags/$v"
		echo "$v: tagged $commit"
	fi

	# 2. env requires the root tag, and env/vX.Y.Z tags that commit.
	if git rev-parse -q --verify "refs/tags/env/$v" >/dev/null; then
		echo "env/$v: tag exists"
	else
		bump=$(env_bump_commit "$v")
		if [[ -z $bump ]]; then
			(
				cd env
				run go mod edit -require="github.com/sky-valley/pi@$v"
				run env GOPROXY=direct go mod tidy
				run go build ./...
				run go vet ./...
			)
			run git add env/go.mod env/go.sum
			run git -c user.name="$COMMITTER_NAME" -c user.email="$COMMITTER_EMAIL" \
				commit -q -m "build(env): require the root at $v"
			push_main
			bump=$(git rev-parse HEAD)
			echo "env: requires $v in $bump"
		fi
		run git -c user.name="$TAGGER_NAME" -c user.email="$TAGGER_EMAIL" tag -a "env/$v" "$bump" -m "$env_tag_msg"
		run git push -q origin "refs/tags/env/$v"
		echo "env/$v: tagged $bump"
	fi

	# 3. The GitHub Release, marked Latest only when vX.Y.Z is the highest tag.
	if gh release view "$v" -R "$REPO" >/dev/null 2>&1; then
		echo "$v: Release exists"
	else
		latest=false
		if [[ $({ echo "$v"; git tag -l 'v*'; } | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' | sort -V | tail -1) == "$v" ]]; then
			latest=true
		fi
		notes=$(mktemp)
		body "$f" >"$notes"
		[[ -s $notes ]] || die "$f: the Release body is empty"
		run gh release create "$v" -R "$REPO" --verify-tag --title "$title" --notes-file "$notes" --latest="$latest"
		echo "$v: Release published (latest=$latest)"
	fi
}

cd "$(git rev-parse --show-toplevel)"
git fetch -q --tags origin main
shopt -s nullglob
requests=(docs/releases/v*.md)
if [[ ${#requests[@]} -eq 0 ]]; then
	echo "release: no requests under docs/releases/"
	exit 0
fi
for f in $(printf '%s\n' "${requests[@]}" | sort -V); do
	publish "$f"
done
gh release list -R "$REPO" -L 3
