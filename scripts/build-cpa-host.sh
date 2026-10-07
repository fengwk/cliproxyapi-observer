#!/usr/bin/env bash
# Builds a CLIProxyAPI host binary from the released module source at <tag>.
#
#   build-cpa-host.sh <tag> <output-path>
#
# The host module source is downloaded into the Go module cache (never cloned
# into this repository) and built there with -mod=readonly so neither the host
# nor the plugin go.mod is modified. The plugin keeps its pinned SDK version;
# only the host binary under test changes.
set -euo pipefail

tag="${1:-}"
out="${2:-}"

if [[ -z "$tag" || -z "$out" ]]; then
	echo "usage: build-cpa-host.sh <tag> <output-path>" >&2
	exit 2
fi

if [[ ! "$tag" =~ ^v([0-9]+)\.[0-9]+\.[0-9]+$ ]]; then
	echo "build-cpa-host: invalid release tag '$tag'" >&2
	exit 1
fi

major="${BASH_REMATCH[1]}"
expected_major="${CPA_EXPECTED_MAJOR:-8}"
if [[ "$major" != "$expected_major" ]]; then
	echo "build-cpa-host: refusing unexpected CPA major 'v${major}' (expected v${expected_major})" >&2
	exit 1
fi

module="github.com/router-for-me/CLIProxyAPI/v${major}"
info="$(go mod download -json "${module}@${tag}")"
dir="$(printf '%s' "$info" | jq -r '.Dir // empty')"

if [[ -z "$dir" || ! -f "${dir}/go.mod" ]]; then
	echo "build-cpa-host: module source for ${module}@${tag} not found" >&2
	exit 1
fi

module_line="$(awk '$1 == "module" { print $2; exit }' "${dir}/go.mod")"
if [[ "$module_line" != "$module" ]]; then
	echo "build-cpa-host: module path mismatch: got '${module_line}', want '${module}'" >&2
	exit 1
fi

selected="$(go list -m "${module}@${tag}" 2>/dev/null || true)"
if [[ "$selected" != "${module} ${tag}" ]]; then
	echo "build-cpa-host: selected version mismatch: got '${selected}', want '${module} ${tag}'" >&2
	exit 1
fi

mkdir -p "$(dirname "$out")"
( cd "$dir" && go build -mod=readonly -o "$out" ./cmd/server )

if [[ ! -x "$out" ]]; then
	echo "build-cpa-host: build did not produce an executable at '${out}'" >&2
	exit 1
fi

printf 'build-cpa-host: %s@%s -> %s\n' "$module" "$tag" "$out" >&2
