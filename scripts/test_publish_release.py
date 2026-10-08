#!/usr/bin/env python3
"""Offline tests for scripts/publish_release.py.

The publisher validates the three native archives, their checksums and metadata
before creating a draft release and publishing it. These tests build real zip
archives in a temporary directory and drive the script against a fake ``gh``
binary that records every mutation, so the safety rules (no clobbering, no tag
deletion, no human-draft takeover, recoverable partial drafts) are asserted on
the observed commands. No network access is required.
"""

import hashlib
import json
import os
import shutil
import subprocess
import sys
import tempfile
import unittest
import warnings
import zipfile

SCRIPT = os.path.join(os.path.dirname(os.path.abspath(__file__)), "publish_release.py")
REPO = "owner/repo"
PLUGIN_ID = "cliproxyapi-observer"
TAG = "v0.1.2"
VERSION = "0.1.2"
COMMIT = "a" * 40
TARGETS = (("linux", "amd64", "so"), ("linux", "arm64", "so"), ("darwin", "arm64", "dylib"))

FAKE_GH = r'''#!/usr/bin/env python3
"""Fake gh for the publisher: records release mutations, honours failure knobs."""
import json
import os
import re
import sys


def load_state():
    with open(os.environ["FAKE_GH_STATE"], "r", encoding="utf-8") as handle:
        return json.load(handle)


def save_state(state):
    with open(os.environ["FAKE_GH_STATE"], "w", encoding="utf-8") as handle:
        json.dump(state, handle)


def log(argv):
    with open(os.environ["FAKE_GH_LOG"], "a", encoding="utf-8") as handle:
        handle.write(json.dumps(argv) + "\n")


def main():
    argv = sys.argv[1:]
    log(argv)
    state = load_state()

    if argv[:1] == ["api"]:
        method = "GET"
        if "--method" in argv:
            method = argv[argv.index("--method") + 1]
        path = next((a for a in argv if a.startswith("repos/")), "")
        if method == "GET" and path.endswith("/commits/" + state["tag"]):
            sys.stdout.write(json.dumps({"sha": state["commit"]}))
            return 0
        if method == "GET" and path.endswith("/releases/tags/" + state["tag"]):
            release = state.get("existing_release")
            if release is None:
                sys.stderr.write("gh: Not Found (HTTP 404)\n")
                return 1
            sys.stdout.write(json.dumps(release))
            return 0
        if method == "DELETE":
            match = re.fullmatch(r"repos/[^/]+/[^/]+/releases/(\d+)", path)
            current = state.get("existing_release")
            if match and current is not None and str(current.get("id")) == match.group(1):
                if state.get("delete_fail"):
                    sys.stderr.write("delete failed\n")
                    return 1
                state["existing_release"] = None
                state.setdefault("deletes", []).append(argv)
                save_state(state)
                sys.stdout.write("{}")
                return 0
            sys.stderr.write("release not found\n")
            return 1
        sys.stderr.write("unsupported api path\n")
        return 1

    if argv[:1] != ["release"]:
        sys.stderr.write("unsupported command\n")
        return 1

    action = argv[1] if len(argv) > 1 else ""
    tag = argv[2] if len(argv) > 2 else ""
    if action == "create":
        if state.get("create_fail"):
            sys.stderr.write("create failed\n")
            return 1
        state["existing_release"] = {
            "id": state.get("next_id", 101),
            "tag_name": tag,
            "draft": True,
            "prerelease": False,
            "author": {"login": "github-actions[bot]"},
        }
        state.setdefault("creates", []).append(argv)
        save_state(state)
        return 0
    if action == "edit":
        if state.get("edit_fail"):
            sys.stderr.write("edit failed\n")
            return 1
        release = state.get("existing_release")
        if release is not None:
            release["draft"] = False
        state.setdefault("edits", []).append(argv)
        save_state(state)
        return 0
    sys.stderr.write("unsupported release action\n")
    return 1


sys.exit(main())
'''


def write_executable(path, content):
    with open(path, "w", encoding="utf-8") as handle:
        handle.write(content)
    os.chmod(path, 0o755)


def archive_name(version, goos, goarch):
    return "%s_%s_%s_%s.zip" % (PLUGIN_ID, version, goos, goarch)


def write_archive(path, goos, goarch, ext, *, version=VERSION, commit=COMMIT, plugin_id=PLUGIN_ID,
                  metadata_override=None, extra_member=None, metadata_raw=None,
                  duplicate_metadata=False):
    metadata = {
        "id": plugin_id,
        "version": version,
        "goos": goos,
        "goarch": goarch,
        "sdk": "github.com/router-for-me/CLIProxyAPI/v8 v8.0.17",
        "commit": commit,
    }
    if metadata_override:
        metadata.update(metadata_override)
    body = metadata_raw if metadata_raw is not None else json.dumps(metadata)
    with zipfile.ZipFile(path, "w") as archive:
        archive.writestr(PLUGIN_ID + "." + ext, "fake dynamic library\n")
        archive.writestr("LICENSE", "license\n")
        archive.writestr("NOTICE", "notice\n")
        archive.writestr("README.md", "# readme\n")
        archive.writestr("metadata.json", body)
        if duplicate_metadata:
            archive.writestr("metadata.json", body)
        if extra_member:
            archive.writestr(extra_member, "extra\n")


def sha256(path):
    digest = hashlib.sha256()
    with open(path, "rb") as handle:
        digest.update(handle.read())
    return digest.hexdigest()


class PublishTest(unittest.TestCase):
    def setUp(self):
        self.work = tempfile.mkdtemp(prefix="publish-release-")
        self.addCleanup(shutil.rmtree, self.work, ignore_errors=True)
        self.gh_bin = os.path.join(self.work, "gh")
        write_executable(self.gh_bin, FAKE_GH)
        self.assets = os.path.join(self.work, "pkg")
        os.makedirs(self.assets)

    def write_assets(self, *, version=VERSION, commit=COMMIT, targets=TARGETS, plugin_id=PLUGIN_ID,
                     metadata_override=None, extra_member=None, metadata_raw=None,
                     duplicate_metadata=False, write_checksums=True,
                     corrupted_checksum=False, omit_checksum_for=None):
        archives = []
        for goos, goarch, ext in targets:
            path = os.path.join(self.assets, archive_name(version, goos, goarch))
            write_archive(
                path,
                goos,
                goarch,
                ext,
                version=version,
                commit=commit,
                plugin_id=plugin_id,
                metadata_override=metadata_override,
                extra_member=extra_member,
                metadata_raw=metadata_raw,
                duplicate_metadata=duplicate_metadata,
            )
            archives.append(path)
        if write_checksums:
            lines = []
            for path in archives:
                name = os.path.basename(path)
                if omit_checksum_for and name == omit_checksum_for and not corrupted_checksum:
                    continue
                digest = sha256(path)
                if corrupted_checksum and name == omit_checksum_for:
                    digest = "0" * 64
                lines.append("%s  %s" % (digest, name))
            with open(os.path.join(self.assets, "checksums.txt"), "w", encoding="utf-8") as handle:
                handle.write("\n".join(lines) + "\n")
        return archives

    def run_publisher(self, state, reset=True):
        state_path = os.path.join(self.work, "state.json")
        log_path = os.path.join(self.work, "gh.log")
        with open(state_path, "w", encoding="utf-8") as handle:
            json.dump(state, handle)
        if reset:
            open(log_path, "w", encoding="utf-8").close()
        env = dict(os.environ)
        env.update(
            {
                "GH_BIN": self.gh_bin,
                "GH_REPO": REPO,
                "FAKE_GH_STATE": state_path,
                "FAKE_GH_LOG": log_path,
            }
        )
        proc = subprocess.run(
            [sys.executable, SCRIPT, "--tag", TAG, "--assets", self.assets],
            env=env,
            capture_output=True,
            text=True,
        )
        with open(state_path, "r", encoding="utf-8") as handle:
            final = json.load(handle)
        return proc.returncode, proc.stdout, proc.stderr, final

    def base_state(self, **extra):
        state = {"commit": COMMIT, "tag": TAG, "existing_release": None}
        state.update(extra)
        return state

    @staticmethod
    def call_has(argv, needle):
        return any(needle == token or needle in token for token in argv)

    def test_valid_assets_publish_through_draft(self):
        self.write_assets()
        code, out, err, final = self.run_publisher(self.base_state())
        self.assertEqual(code, 0, err)
        creates = final.get("creates", [])
        self.assertEqual(len(creates), 1)
        create = creates[0]
        self.assertIn("--draft", create)
        self.assertIn("--verify-tag", create)
        self.assertNotIn("--clobber", create)
        for goos, goarch, _ in TARGETS:
            self.assertIn(archive_name(VERSION, goos, goarch), " ".join(create))
        self.assertIn("checksums.txt", " ".join(create))
        edits = final.get("edits", [])
        self.assertEqual(len(edits), 1)
        self.assertTrue(self.call_has(edits[0], "--draft=false"))
        # The repository is always explicit.
        self.assertTrue(self.call_has(create, "--repo"))

    def test_bad_checksum_blocks_publication(self):
        self.write_assets(omit_checksum_for=archive_name(VERSION, "linux", "amd64"),
                          corrupted_checksum=True)
        code, out, err, final = self.run_publisher(self.base_state())
        self.assertEqual(code, 1)
        self.assertIn("checksum mismatch", err)
        self.assertEqual(final.get("creates", []), [])

    def test_missing_checksum_entry_blocks_publication(self):
        self.write_assets(omit_checksum_for=archive_name(VERSION, "darwin", "arm64"))
        code, out, err, final = self.run_publisher(self.base_state())
        self.assertEqual(code, 1)
        self.assertIn("no entry", err)
        self.assertEqual(final.get("creates", []), [])

    def test_unexpected_layout_blocks_publication(self):
        self.write_assets(extra_member="nested/extra.txt")
        code, out, err, final = self.run_publisher(self.base_state())
        self.assertEqual(code, 1)
        self.assertIn("layout", err)
        self.assertEqual(final.get("creates", []), [])

    def test_version_mismatch_blocks_publication(self):
        self.write_assets(metadata_override={"version": "9.9.9"})
        code, out, err, final = self.run_publisher(self.base_state())
        self.assertEqual(code, 1)
        self.assertIn("version", err)
        self.assertEqual(final.get("creates", []), [])

    def test_commit_mismatch_blocks_publication(self):
        self.write_assets(metadata_override={"commit": "b" * 40})
        code, out, err, final = self.run_publisher(self.base_state())
        self.assertEqual(code, 1)
        self.assertIn("commit", err)
        self.assertEqual(final.get("creates", []), [])

    def test_missing_architecture_blocks_publication(self):
        self.write_assets(targets=TARGETS[:2])
        code, out, err, final = self.run_publisher(self.base_state())
        self.assertEqual(code, 1)
        self.assertIn("missing", err)
        self.assertEqual(final.get("creates", []), [])

    @staticmethod
    def release_record(draft, author="github-actions[bot]", release_id=101, tag=TAG):
        return {
            "id": release_id,
            "tag_name": tag,
            "draft": draft,
            "prerelease": False,
            "author": {"login": author},
        }

    def test_already_published_is_noop(self):
        self.write_assets()
        published = self.release_record(draft=False)
        code, out, err, final = self.run_publisher(self.base_state(existing_release=published))
        self.assertEqual(code, 0, err)
        self.assertEqual(final.get("creates", []), [])
        self.assertEqual(final.get("edits", []), [])
        self.assertEqual(final.get("deletes", []), [])

    def test_owned_partial_draft_is_recovered_without_touching_tag(self):
        self.write_assets()
        draft = self.release_record(draft=True)
        code, out, err, final = self.run_publisher(self.base_state(existing_release=draft))
        self.assertEqual(code, 0, err)
        deletes = final.get("deletes", [])
        self.assertEqual(len(deletes), 1)
        # Deletion is by immutable id and never deletes the tag.
        self.assertTrue(self.call_has(deletes[0], "/releases/101"))
        self.assertNotIn("--cleanup-tag", deletes[0])
        self.assertEqual(len(final.get("creates", [])), 1)
        self.assertEqual(len(final.get("edits", [])), 1)

    def test_human_draft_is_refused(self):
        self.write_assets()
        draft = self.release_record(draft=True, author="octocat")
        code, out, err, final = self.run_publisher(self.base_state(existing_release=draft))
        self.assertEqual(code, 1)
        self.assertIn("owned by", err)
        self.assertEqual(final.get("deletes", []), [])
        self.assertEqual(final.get("creates", []), [])
        self.assertEqual(final.get("edits", []), [])

    def test_create_failure_is_recoverable(self):
        self.write_assets()
        code, out, err, final = self.run_publisher(self.base_state(create_fail=True))
        self.assertEqual(code, 1)
        self.assertEqual(final.get("edits", []), [])
        # Next attempt: the failure knob is cleared, so it publishes.
        final["create_fail"] = False
        code, out, err, final = self.run_publisher(final, reset=False)
        self.assertEqual(code, 0, err)
        self.assertEqual(len(final.get("creates", [])), 1)
        self.assertEqual(len(final.get("edits", [])), 1)

    def test_publish_failure_leaves_unpublished_draft_and_retry_is_safe(self):
        self.write_assets()
        code, out, err, final = self.run_publisher(self.base_state(edit_fail=True))
        self.assertEqual(code, 1)
        # The draft exists but is not published.
        self.assertEqual(len(final.get("creates", [])), 1)
        self.assertEqual(final.get("edits", []), [])
        self.assertTrue(final["existing_release"]["draft"])

        # Retry: the managed draft is recreated (never the tag) and published.
        final["edit_fail"] = False
        code, out, err, final = self.run_publisher(final, reset=False)
        self.assertEqual(code, 0, err)
        self.assertEqual(len(final.get("deletes", [])), 1)
        self.assertFalse(final["existing_release"]["draft"])

    def test_owned_draft_delete_failure_blocks(self):
        self.write_assets()
        draft = self.release_record(draft=True)
        code, out, err, final = self.run_publisher(
            self.base_state(existing_release=draft, delete_fail=True)
        )
        self.assertEqual(code, 1)
        self.assertEqual(final.get("creates", []), [])
        self.assertEqual(final.get("edits", []), [])

    def test_duplicate_metadata_member_blocks_publication(self):
        # A set of member names would accept a duplicated metadata.json.
        with warnings.catch_warnings():
            warnings.simplefilter("ignore")  # zipfile warns about the duplicate
            self.write_assets(duplicate_metadata=True)
        code, out, err, final = self.run_publisher(self.base_state())
        self.assertEqual(code, 1)
        self.assertIn("layout", err)
        self.assertEqual(final.get("creates", []), [])

    def test_non_object_metadata_blocks_publication(self):
        self.write_assets(metadata_raw="[1, 2, 3]")
        code, out, err, final = self.run_publisher(self.base_state())
        self.assertEqual(code, 1)
        self.assertIn("not an object", err)
        self.assertEqual(final.get("creates", []), [])

    def test_checksum_path_entry_blocks_publication(self):
        self.write_assets()
        with open(os.path.join(self.assets, "checksums.txt"), "a", encoding="utf-8") as handle:
            handle.write("%s  ../evil.zip\n" % ("0" * 64))
        code, out, err, final = self.run_publisher(self.base_state())
        self.assertEqual(code, 1)
        self.assertIn("unexpected entry", err)
        self.assertEqual(final.get("creates", []), [])

    def test_duplicate_checksum_entry_blocks_publication(self):
        self.write_assets()
        name = archive_name(VERSION, "linux", "amd64")
        digest = sha256(os.path.join(self.assets, name))
        with open(os.path.join(self.assets, "checksums.txt"), "a", encoding="utf-8") as handle:
            handle.write("%s  %s\n" % (digest, name))
        code, out, err, final = self.run_publisher(self.base_state())
        self.assertEqual(code, 1)
        self.assertIn("duplicate entry", err)
        self.assertEqual(final.get("creates", []), [])

    def test_bad_tag_is_refused(self):
        self.write_assets()
        env = dict(os.environ)
        env.update({"GH_BIN": self.gh_bin, "GH_REPO": REPO})
        proc = subprocess.run(
            [sys.executable, SCRIPT, "--tag", "0.1.2", "--assets", self.assets],
            env=env,
            capture_output=True,
            text=True,
        )
        self.assertEqual(proc.returncode, 1)
        self.assertIn("stable vX.Y.Z", proc.stderr)


if __name__ == "__main__":
    unittest.main(verbosity=2)
