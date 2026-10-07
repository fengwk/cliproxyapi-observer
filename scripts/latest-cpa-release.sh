#!/usr/bin/env bash
# Resolves the latest stable CLIProxyAPI release for a single major version.
#
# Compatibility automation must not silently jump to a new major line, so the
# major is explicit and defaults to 8. Drafts and prereleases are ignored and a
# missing match is a hard failure (never an empty/ignored success).
set -euo pipefail

major="${CPA_MAJOR:-8}"
repo="${CPA_REPO:-router-for-me/CLIProxyAPI}"

if [[ ! "$major" =~ ^[0-9]+$ ]]; then
	echo "latest-cpa-release: invalid CPA_MAJOR '$major'" >&2
	exit 1
fi

auth=()
if [[ -n "${GITHUB_TOKEN:-}" ]]; then
	auth=(-H "Authorization: Bearer ${GITHUB_TOKEN}")
fi

url="https://api.github.com/repos/${repo}/releases?per_page=100"
json="$(curl -fsSL "${auth[@]}" -H "Accept: application/vnd.github+json" "$url")"

tag="$(printf '%s' "$json" | jq -r --arg major "$major" '
	[ .[]
	  | select((.draft | not) and (.prerelease | not))
	  | .tag_name
	  | select(test("^v" + $major + "\\.[0-9]+\\.[0-9]+$"))
	]
	| sort_by(split(".") | map(ltrimstr("v") | tonumber))
	| last // empty
')"

if [[ -z "$tag" ]]; then
	echo "latest-cpa-release: no stable v${major} release found for ${repo}" >&2
	exit 1
fi

printf '%s\n' "$tag"
