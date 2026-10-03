#!/bin/sh
# Mutation check (LEC §7.3, M-AL-*): apply each patch, require the tests it
# names to fail, revert. Each patch starts with three header lines that
# git apply ignores:
#
#   # <id>: what the mutation breaks
#   # pkg: packages to test
#   # run: go test -run pattern
#
# A mutation that no longer applies or no longer compiles is reported as
# BROKEN, not as caught: a test that cannot build has caught nothing.
#
#   test/mutations/run.sh              every patch
#   test/mutations/run.sh M-AL-3 ...   the named ones
set -u
root=$(cd "$(dirname "$0")/../.." && pwd) || exit 2
cd "$root" || exit 2
if [ -n "$(git status --porcelain --untracked-files=no)" ]; then
	echo "mutations: the working tree has uncommitted changes; commit them first" >&2
	exit 2
fi

log=$(mktemp)
applied=""
restore() {
	if [ -n "$applied" ]; then
		git apply -R "$applied" || echo "mutations: could not revert $applied; run git checkout -- . by hand" >&2
		applied=""
	fi
}
trap 'restore; rm -f "$log"' EXIT
trap 'exit 130' INT TERM

if [ $# -gt 0 ]; then
	patches=""
	for id in "$@"; do patches="$patches test/mutations/$id.patch"; done
else
	patches=$(ls test/mutations/M-*.patch)
fi

# Green first: a named test that already fails on the clean tree would make
# every mutation of it look caught.
for p in $patches; do
	pkg=$(sed -n 's/^# pkg: //p' "$p")
	run=$(sed -n 's/^# run: //p' "$p")
	# shellcheck disable=SC2086 # pkg is a list of packages
	if ! go test -count=1 -run "$run" $pkg >"$log" 2>&1; then
		echo "mutations: $(basename "$p" .patch): $run fails without the mutation" >&2
		sed 's/^/          /' "$log" >&2
		exit 2
	fi
done

status=0
for p in $patches; do
	id=$(basename "$p" .patch)
	pkg=$(sed -n 's/^# pkg: //p' "$p")
	run=$(sed -n 's/^# run: //p' "$p")
	if [ -z "$pkg" ] || [ -z "$run" ]; then
		echo "BROKEN    $id: missing '# pkg:' or '# run:' header"
		status=1
		continue
	fi
	if ! git apply --check "$p" >"$log" 2>&1; then
		echo "BROKEN    $id: the patch no longer applies"
		sed 's/^/          /' "$log"
		status=1
		continue
	fi
	git apply "$p"
	applied=$p
	# shellcheck disable=SC2086 # pkg is a list of packages
	if go test -count=1 -run "$run" $pkg >"$log" 2>&1; then
		echo "SURVIVED  $id: $run still passes"
		status=1
	elif grep -q -e '\[build failed\]' -e '\[setup failed\]' "$log"; then
		echo "BROKEN    $id: the mutated code does not compile"
		sed 's/^/          /' "$log" | head -20
		status=1
	else
		echo "caught    $id"
		grep -e '^--- FAIL' -e '^    --- FAIL' "$log" | sed 's/^ */          /'
	fi
	restore
done
exit $status
