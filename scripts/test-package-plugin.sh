#!/usr/bin/env bash
# Regression tests for scripts/package-plugin.sh.
#
# A throwaway repository skeleton (module manifest, archive assets and a fake
# dynamic library) stands in for the real plugin, so packaging is exercised
# without a C toolchain. The relative outdir case mirrors `make package`
# ("dist/pkg"), which used to break because zip runs from inside the staging
# directory; the absolute outdir case guards the canonicalized path. Both cases
# verify the archive member layout and the emitted sha256 checksum.
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
packager="${script_dir}/package-plugin.sh"

if ! command -v zip >/dev/null 2>&1 || ! command -v unzip >/dev/null 2>&1; then
	echo "test-package-plugin: zip and unzip are required" >&2
	exit 2
fi

failures=0
case_number=0

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

# Minimal standalone repository that package-plugin.sh can package.
repo="${work}/repo"
mkdir -p "${repo}/dist/linux/amd64"
cat >"${repo}/go.mod" <<'EOF'
module github.com/fengwk/cliproxyapi-observer

go 1.26.0

require (
	github.com/router-for-me/CLIProxyAPI/v8 v8.0.16
)
EOF
printf 'license text\n' >"${repo}/LICENSE"
printf 'notice text\n' >"${repo}/NOTICE"
printf '# readme\n' >"${repo}/README.md"
printf 'fake dynamic library\n' >"${repo}/dist/linux/amd64/cliproxyapi-observer.so"

version="0.1.0"
archive="cliproxyapi-observer_${version}_linux_amd64.zip"

run_package_case() {
	local description="$1" outdir_arg="$2" outdir_abs="$3"
	case_number=$((case_number + 1))
	local out="${outdir_abs}/${archive}"
	local ok=1 reason=""

	rm -rf "$outdir_abs"
	local rc=0
	(
		cd "$repo" &&
			COMMIT=deadbeef bash "$packager" "$version" linux amd64 "$outdir_arg" "$repo"
	) >/dev/null 2>&1 || rc=$?

	if [[ "$rc" -ne 0 ]]; then
		ok=0
		reason="packager exited $rc"
	elif [[ ! -f "$out" ]]; then
		ok=0
		reason="archive missing at ${out}"
	else
		local expected members
		expected=$'LICENSE\nNOTICE\nREADME.md\ncliproxyapi-observer.so\nmetadata.json'
		members="$(unzip -Z1 "$out" | LC_ALL=C sort)"
		if [[ "$members" != "$expected" ]]; then
			ok=0
			reason="unexpected archive members: ${members//$'\n'/, }"
		fi
	fi

	if [[ "$ok" -eq 1 ]]; then
		local lib_ok='fake dynamic library'
		if [[ "$(unzip -p "$out" cliproxyapi-observer.so)" != "$lib_ok" ]]; then
			ok=0
			reason="dynamic library payload mismatch"
		fi
	fi

	if [[ "$ok" -eq 1 ]]; then
		local meta
		meta="$(unzip -p "$out" metadata.json)"
		if [[ "$meta" != *"\"version\": \"${version}\""* || "$meta" != *"\"commit\": \"deadbeef\""* ]]; then
			ok=0
			reason="metadata.json missing version/commit"
		fi
	fi

	if [[ "$ok" -eq 1 ]]; then
		if ! (cd "$outdir_abs" && sha256sum -c "${archive}.sha256") >/dev/null 2>&1; then
			ok=0
			reason="sha256 verification failed"
		fi
	fi

	if [[ "$ok" -eq 1 ]]; then
		printf 'ok   %d - %s\n' "$case_number" "$description"
	else
		printf 'FAIL %d - %s (%s)\n' "$case_number" "$description" "$reason"
		failures=$((failures + 1))
	fi
}

# Relative outdir exactly as `make package` passes it: must land in the repo,
# not inside the (removed) staging directory.
run_package_case "relative outdir is written to the requested directory" \
	"dist/pkg" "${repo}/dist/pkg"

# Absolute outdir must keep working.
run_package_case "absolute outdir is written to the requested directory" \
	"${work}/abs/pkg" "${work}/abs/pkg"

if [[ "$failures" -ne 0 ]]; then
	printf '\n%d test(s) failed\n' "$failures" >&2
	exit 1
fi
printf '\nall %d package-plugin tests passed\n' "$case_number"
