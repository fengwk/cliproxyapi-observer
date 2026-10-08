#!/usr/bin/env bash
# Prints the plugin version used for ldflags and release metadata.
#
# Precedence:
#   1. PLUGIN_VERSION (release workflow passes the pushed tag)
#   2. exact git tag on HEAD (vX.Y.Z)
#   3. <default>-dev+<short-sha> so local builds never claim a released version
#
# The version is never taken from the CLIProxyAPI SDK; the host version and the
# plugin version are independent contracts.
set -euo pipefail

default_version="0.1.0"

version="${PLUGIN_VERSION:-}"
if [[ -z "$version" ]]; then
	if tag="$(git describe --tags --exact-match 2>/dev/null)"; then
		version="$tag"
	else
		sha="$(git rev-parse --short HEAD 2>/dev/null || true)"
		if [[ -n "$sha" ]]; then
			version="${default_version}-dev+${sha}"
		else
			version="${default_version}-dev"
		fi
	fi
fi

version="${version#v}"
version="${version#V}"

if [[ ! "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.+-]+)?$ ]]; then
	echo "plugin-version: refusing unknown version '$version'" >&2
	exit 1
fi

printf '%s\n' "$version"
