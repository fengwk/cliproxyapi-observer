#!/usr/bin/env bash
# Packages one native plugin build as a CLIProxyAPI plugin-store archive.
#
#   package-plugin.sh <version> <goos> <goarch> <outdir> [repo-root]
#
# Produces <outdir>/cliproxyapi-observer_<version>_<goos>_<goarch>.zip
# plus a matching .zip.sha256 file. The archive layout mirrors what the host
# installer expects: the single dynamic library at the archive root, named with
# the plugin id, together with LICENSE, NOTICE, README.md and build metadata.
set -euo pipefail

version="${1:-}"
goos="${2:-}"
goarch="${3:-}"
outdir="${4:-}"
root="${5:-$(pwd)}"

if [[ -z "$version" || -z "$goos" || -z "$goarch" || -z "$outdir" ]]; then
	echo "usage: package-plugin.sh <version> <goos> <goarch> <outdir> [repo-root]" >&2
	exit 2
fi

if [[ ! "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.+-]+)?$ ]]; then
	echo "package-plugin: refusing unknown version '$version'" >&2
	exit 1
fi

plugin_id="cliproxyapi-observer"

case "$goos" in
linux) ext="so" ;;
darwin) ext="dylib" ;;
windows) ext="dll" ;;
*)
	echo "package-plugin: unsupported goos '$goos'" >&2
	exit 1
	;;
esac

lib="${root}/dist/${goos}/${goarch}/${plugin_id}.${ext}"
if [[ ! -f "$lib" ]]; then
	echo "package-plugin: native library not found at '${lib}'" >&2
	exit 1
fi

for asset in LICENSE NOTICE README.md; do
	if [[ ! -f "${root}/${asset}" ]]; then
		echo "package-plugin: required archive asset '${asset}' is missing" >&2
		exit 1
	fi
done

sdk="$(awk '$1 == "github.com/router-for-me/CLIProxyAPI/v8" { print $1 " " $2; exit }' "${root}/go.mod")"
if [[ -z "$sdk" ]]; then
	echo "package-plugin: CLIProxyAPI SDK version not found in go.mod" >&2
	exit 1
fi

commit="${COMMIT:-${GITHUB_SHA:-}}"
if [[ -z "$commit" ]]; then
	commit="$(git -C "$root" rev-parse HEAD 2>/dev/null || true)"
fi
if [[ -z "$commit" ]]; then
	echo "package-plugin: commit id unavailable (set COMMIT or GITHUB_SHA)" >&2
	exit 1
fi

archive="${plugin_id}_${version}_${goos}_${goarch}.zip"
mkdir -p "$outdir"
# zip runs from inside the staging directory, so a relative outdir (e.g.
# "dist/pkg" from `make package`) would be resolved against the stage and the
# archive would be written to the wrong place or lost with the stage. Pin the
# output directory to an absolute path before archiving.
outdir="$(cd "$outdir" && pwd)"
out="${outdir}/${archive}"

stage="$(mktemp -d)"
trap 'rm -rf "$stage"' EXIT

cp "$lib" "${stage}/${plugin_id}.${ext}"
cp "${root}/LICENSE" "${root}/NOTICE" "${root}/README.md" "$stage/"

cat >"${stage}/metadata.json" <<EOF
{
  "id": "${plugin_id}",
  "version": "${version}",
  "goos": "${goos}",
  "goarch": "${goarch}",
  "sdk": "${sdk}",
  "commit": "${commit}"
}
EOF

rm -f "$out"
( cd "$stage" && zip -X -q "$out" "${plugin_id}.${ext}" LICENSE NOTICE README.md metadata.json )

if command -v sha256sum >/dev/null 2>&1; then
	digest="$(sha256sum "$out" | awk '{print $1}')"
else
	digest="$(shasum -a 256 "$out" | awk '{print $1}')"
fi
printf '%s  %s\n' "$digest" "$archive" >"${out}.sha256"

printf 'package-plugin: %s\n' "$out" >&2
