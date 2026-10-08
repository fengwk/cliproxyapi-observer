#!/usr/bin/env python3
"""Validate native archives and publish exactly one immutable GitHub Release.

The release workflow builds the archives from the pushed tag and uploads them as
artifacts. This helper runs from the trusted default branch checkout, resolves
the tag's commit independently through the API (it never trusts ``GITHUB_SHA``),
validates the three expected native ZIPs, their checksums, root layout and
metadata, and only then publishes the release.

Safety rules:

* An already **published** release is a no-op: assets are never uploaded,
  replaced or clobbered, and a tag is never moved or deleted.
* A **draft** release may be recovered only when it is owned by
  ``github-actions[bot]`` (the retry path). A human-owned or unknown draft is
  refused.
* A new release is created as a **draft** containing every asset, and is
  published only after the draft upload succeeds.

Python standard library only.
"""

import argparse
import hashlib
import json
import os
import re
import subprocess
import sys
import zipfile

PLUGIN_ID = "cliproxyapi-observer"
AUTOMATION_AUTHOR = "github-actions[bot]"
TAG_RE = re.compile(r"^v(\d+)\.(\d+)\.(\d+)$")

# goos, goarch, native library extension: the three shipped native targets.
TARGETS = (
    ("linux", "amd64", "so"),
    ("linux", "arm64", "so"),
    ("darwin", "arm64", "dylib"),
)
STATIC_MEMBERS = frozenset({"LICENSE", "NOTICE", "README.md", "metadata.json"})


class PublishError(Exception):
    """A fail-closed validation or publication problem."""


def sha256(path):
    digest = hashlib.sha256()
    with open(path, "rb") as handle:
        for chunk in iter(lambda: handle.read(65536), b""):
            digest.update(chunk)
    return digest.hexdigest()


def run_gh(bin_path, args):
    proc = subprocess.run([bin_path] + list(args), capture_output=True, text=True)
    if proc.returncode != 0:
        detail = (proc.stderr or proc.stdout or "").strip().splitlines()
        raise PublishError("gh %s failed: %s" % (args[0], detail[0] if detail else "unknown error"))
    return proc.stdout


def resolve_tag_commit(bin_path, repo, tag):
    out = run_gh(bin_path, ["api", "repos/%s/commits/%s" % (repo, tag)])
    try:
        data = json.loads(out)
    except ValueError:
        raise PublishError("malformed tag resolution response for %s" % tag)
    sha = data.get("sha")
    if not isinstance(sha, str) or not sha:
        raise PublishError("could not resolve the commit for tag %s" % tag)
    return sha


def validate_archive(path, version, commit, goos, goarch, ext):
    name = os.path.basename(path)
    try:
        with zipfile.ZipFile(path) as archive:
            members = archive.namelist()
            expected = STATIC_MEMBERS | {PLUGIN_ID + "." + ext}
            # ``set`` alone would accept a duplicated member (e.g. two
            # metadata.json entries), so require an exact unique count as well.
            if len(members) != len(expected) or set(members) != expected:
                raise PublishError("%s has an unexpected archive layout" % name)
            try:
                metadata = json.loads(archive.read("metadata.json").decode("utf-8"))
            except (KeyError, ValueError, UnicodeDecodeError):
                raise PublishError("%s has malformed metadata.json" % name)
    except zipfile.BadZipFile:
        raise PublishError("%s is not a valid zip archive" % name)
    if not isinstance(metadata, dict):
        raise PublishError("%s metadata.json is not an object" % name)
    if metadata.get("id") != PLUGIN_ID:
        raise PublishError("%s metadata id does not match the plugin" % name)
    if metadata.get("version") != version:
        raise PublishError("%s metadata version does not match %s" % (name, version))
    if metadata.get("commit") != commit:
        raise PublishError("%s metadata commit does not match the tagged commit" % name)
    if metadata.get("goos") != goos or metadata.get("goarch") != goarch:
        raise PublishError("%s metadata platform does not match" % name)


def validate_checksums(path, archives):
    if not os.path.isfile(path):
        raise PublishError("checksums.txt is missing")
    expected = {os.path.basename(archive) for archive in archives}
    recorded = {}
    with open(path, "r", encoding="utf-8") as handle:
        for line in handle:
            parts = line.split()
            if not parts:
                continue
            if len(parts) != 2:
                raise PublishError("checksums.txt is malformed")
            digest, entry = parts[0].lower(), parts[1]
            # Rejecting any entry that is not exactly one expected file also
            # rejects duplicate spellings, directories and path escapes.
            if entry not in expected:
                raise PublishError("checksums.txt references an unexpected entry %r" % entry)
            if entry in recorded:
                raise PublishError("checksums.txt has a duplicate entry for %s" % entry)
            recorded[entry] = digest
    missing = sorted(expected - set(recorded))
    if missing:
        raise PublishError("checksums.txt has no entry for %s" % ", ".join(missing))
    for archive in archives:
        name = os.path.basename(archive)
        if recorded[name] != sha256(archive):
            raise PublishError("checksum mismatch for %s" % name)


def validate_assets(assets_dir, version, commit):
    if not os.path.isdir(assets_dir):
        raise PublishError("assets directory %r does not exist" % assets_dir)
    archives = []
    for goos, goarch, ext in TARGETS:
        name = "%s_%s_%s_%s.zip" % (PLUGIN_ID, version, goos, goarch)
        path = os.path.join(assets_dir, name)
        if not os.path.isfile(path):
            raise PublishError("expected archive %s is missing" % name)
        validate_archive(path, version, commit, goos, goarch, ext)
        archives.append(path)
    checksums = os.path.join(assets_dir, "checksums.txt")
    validate_checksums(checksums, archives)
    return archives + [checksums]


def get_release(bin_path, repo, tag):
    """Return the release identity for ``tag`` or None when it does not exist.

    The full API object is used (rather than ``gh release view``) because the
    immutable release ``id`` is needed to delete a managed draft without any
    risk of deleting a different release for the same tag.
    """
    proc = subprocess.run(
        [bin_path, "api", "repos/%s/releases/tags/%s" % (repo, tag)],
        capture_output=True,
        text=True,
    )
    if proc.returncode != 0:
        message = ((proc.stderr or "") + (proc.stdout or "")).lower()
        if "404" in message or "not found" in message:
            return None
        raise PublishError("could not read release %s" % tag)
    try:
        raw = json.loads(proc.stdout)
    except ValueError:
        raise PublishError("malformed release response for %s" % tag)
    if not isinstance(raw, dict):
        raise PublishError("malformed release response for %s" % tag)
    return {
        "id": raw.get("id"),
        "tag": raw.get("tag_name"),
        "draft": bool(raw.get("draft")),
        "prerelease": bool(raw.get("prerelease")),
        "author": (raw.get("author") or {}).get("login"),
    }


def delete_release(bin_path, repo, release_id):
    """Delete the draft release by immutable id (never the tag)."""
    run_gh(bin_path, ["api", "--method", "DELETE", "repos/%s/releases/%s" % (repo, release_id)])


def publish(tag, assets_dir, repo):
    if not repo:
        raise PublishError("GH_REPO (or --repo) is required")
    if not TAG_RE.match(tag):
        raise PublishError("tag %r is not a stable vX.Y.Z version" % tag)
    version = tag[1:]
    bin_path = os.environ.get("GH_BIN", "gh").strip() or "gh"

    # The tagged commit is resolved independently of the workflow's environment.
    commit = resolve_tag_commit(bin_path, repo, tag)
    archives = validate_assets(assets_dir, version, commit)

    existing = get_release(bin_path, repo, tag)
    if existing is not None and not existing["draft"]:
        print("publish-release: %s is already published; nothing to do" % tag)
        return
    if existing is not None:
        if existing["author"] != AUTOMATION_AUTHOR:
            raise PublishError(
                "draft release %s is owned by %r; refusing to touch it"
                % (tag, existing["author"])
            )
        if existing["tag"] != tag or existing["id"] is None:
            raise PublishError("draft release %s has an unexpected identity; refusing to touch it" % tag)
        # Re-read immediately before deleting: a concurrent publish becomes a
        # no-op, and a changed identity blocks instead of deleting blindly.
        current = get_release(bin_path, repo, tag)
        if current is not None:
            if not current["draft"]:
                print("publish-release: %s became published; nothing to do" % tag)
                return
            if (
                current["id"] != existing["id"]
                or current["tag"] != tag
                or current["author"] != AUTOMATION_AUTHOR
            ):
                raise PublishError("draft release %s changed identity; refusing to delete it" % tag)
            # A failed upload may leave a managed partial draft. Recreate it by
            # id (never the tag) so the next attempt starts from a clean upload.
            delete_release(bin_path, repo, current["id"])

    run_gh(
        bin_path,
        [
            "release",
            "create",
            tag,
            "--repo",
            repo,
            "--verify-tag",
            "--draft",
            "--title",
            tag,
            "--notes",
            "Native CLIProxyAPI plugin artifacts built from %s. "
            "Verify downloads against checksums.txt." % tag,
        ]
        + archives,
    )
    # Publish only after every asset uploaded successfully.
    run_gh(bin_path, ["release", "edit", tag, "--repo", repo, "--draft=false"])
    print("publish-release: published %s" % tag)


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--tag", required=True, help="existing release tag vX.Y.Z")
    parser.add_argument("--assets", required=True, help="directory with the release assets")
    parser.add_argument("--repo", default=os.environ.get("GH_REPO", ""), help="owner/repo")
    args = parser.parse_args(argv)
    try:
        publish(args.tag, args.assets, args.repo)
    except PublishError as error:
        print("publish-release: %s" % error, file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
