#!/usr/bin/env python3
"""Offline tests for scripts/auto_release.py.

Each case builds a real temporary git repository (the trusted, full-history
checkout the controller reads) and drives the script against a fake ``gh``
binary. The fake speaks just enough of the GitHub API for the controller and
records every mutation, so the tests can assert both the decisions and the
side effects (tags created, release workflows dispatched, no duplicate writes).

The shared policy scripts/dependency_policy.py is imported by the controller at
runtime, so these tests exercise the real security assertions rather than a
stub. No network access is required.
"""

import json
import os
import shutil
import subprocess
import sys
import tempfile
import unittest

SCRIPT = os.path.join(os.path.dirname(os.path.abspath(__file__)), "auto_release.py")
REPO = "owner/repo"
CI_WORKFLOW_ID = 12345

FAKE_GH = r'''#!/usr/bin/env python3
"""Fake gh: routes the API calls auto_release.py makes, records mutations."""
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
    path = os.environ.get("FAKE_GH_LOG")
    if path:
        with open(path, "a", encoding="utf-8") as handle:
            handle.write(json.dumps(argv) + "\n")


def emit(obj):
    sys.stdout.write(json.dumps(obj))
    return 0


def fail(message):
    sys.stderr.write(message + "\n")
    return 1


def chunk(items, size):
    size = max(1, int(size))
    return [items[i:i + size] for i in range(0, len(items), size)] or [[]]


def envelope(runs, state):
    return [
        {"total_count": len(runs), "workflow_runs": part}
        for part in chunk(runs, state.get("page_size", 100))
    ]


def main():
    argv = sys.argv[1:]
    log(argv)
    state = load_state()

    if argv[:2] == ["workflow", "run"]:
        if state.get("dispatch_fail"):
            return fail("dispatch failed")
        state.setdefault("dispatches", []).append(argv)
        save_state(state)
        return 0

    if argv[:1] == ["release"]:
        action = argv[1] if len(argv) > 1 else ""
        tag = argv[2] if len(argv) > 2 else ""
        if action == "view":
            release = state.get("existing_release")
            if release is None or release.get("tagName") != tag:
                return fail("release not found")
            return emit(release)
        if action == "delete":
            if state.get("delete_fail"):
                return fail("delete failed")
            state["existing_release"] = None
            state.setdefault("deletes", []).append(tag)
            save_state(state)
            return 0
        if action == "create":
            if state.get("create_fail"):
                return fail("create failed")
            state["existing_release"] = {
                "tagName": tag,
                "isDraft": True,
                "isPrerelease": False,
                "author": {"login": "github-actions[bot]"},
            }
            state.setdefault("creates", []).append(tag)
            save_state(state)
            return 0
        if action == "edit":
            if state.get("edit_fail"):
                return fail("edit failed")
            release = state.get("existing_release")
            if release is not None:
                release["isDraft"] = False
            state.setdefault("edits", []).append(tag)
            save_state(state)
            return 0
        return fail("unsupported release action " + action)

    if argv[:1] != ["api"]:
        return fail("unsupported command")

    method = "GET"
    if "--method" in argv:
        method = argv[argv.index("--method") + 1]
    path = ""
    for arg in argv:
        if arg.startswith("repos/"):
            path = arg
            break
    fields = {}
    i = 0
    while i < len(argv) - 1:
        if argv[i] == "-f":
            key, _, value = argv[i + 1].partition("=")
            fields[key] = value
            i += 2
        else:
            i += 1

    repo = state["repo"]

    match = re.fullmatch(r"repos/([^/]+/[^/]+)", path)
    if match and match.group(1) == repo and method == "GET":
        return emit({"default_branch": state.get("default_branch", "main")})

    match = re.fullmatch(r"repos/([^/]+/[^/]+)/git/ref/heads/(.+)", path)
    if match and match.group(1) == repo:
        return emit({"object": {"sha": state.get("main_sha"), "type": "commit"}})

    match = re.fullmatch(r"repos/([^/]+/[^/]+)/actions/runs/(\d+)", path)
    if match and match.group(1) == repo:
        run = state.get("run_by_id", {}).get(match.group(2))
        if run is None:
            return fail("run not found")
        return emit(run)

    match = re.fullmatch(r"repos/([^/]+/[^/]+)/actions/workflows/([^/?]+)/runs\?.*", path)
    if match and match.group(1) == repo and match.group(2) == "release.yml":
        runs = state.get("release_runs", [])
        return emit(envelope(runs, state))

    match = re.fullmatch(r"repos/([^/]+/[^/]+)/actions/workflows/([^/?]+)", path)
    if match and match.group(1) == repo:
        return emit({"id": state.get("ci_workflow_id"), "path": ".github/workflows/" + match.group(2)})

    match = re.fullmatch(r"repos/([^/]+/[^/]+)/actions/runs\?.*", path)
    if match and match.group(1) == repo:
        return emit(envelope(state.get("ci_runs", []), state))

    match = re.fullmatch(r"repos/([^/]+/[^/]+)/releases\?.*", path)
    if match and match.group(1) == repo:
        return emit(chunk(state.get("releases", []), state.get("page_size", 100)))

    match = re.fullmatch(r"repos/([^/]+/[^/]+)/git/matching-refs/tags/(.+)", path)
    if match and match.group(1) == repo:
        query = match.group(2)
        refs = []
        for tag, obj in state.get("tag_refs", {}).items():
            if tag.startswith(query):
                refs.append({"ref": "refs/tags/" + tag, "object": obj})
        return emit(refs)

    match = re.fullmatch(r"repos/([^/]+/[^/]+)/git/tags/(.+)", path)
    if match and match.group(1) == repo:
        target = state.get("derefs", {}).get(match.group(2))
        if target is None:
            return fail("tag object not found")
        return emit({"object": {"sha": target, "type": "commit"}})

    match = re.fullmatch(r"repos/([^/]+/[^/]+)/commits/([^/]+)/pulls", path)
    if match and match.group(1) == repo:
        pulls = state.get("commit_pulls", {}).get(match.group(2), [])
        return emit(chunk(pulls, state.get("page_size", 100)))

    match = re.fullmatch(r"repos/([^/]+/[^/]+)/commits/(.+)", path)
    if match and match.group(1) == repo:
        sha = state.get("commit_resolve", {}).get(match.group(2))
        if sha is None:
            return fail("commit not found")
        return emit({"sha": sha})

    match = re.fullmatch(r"repos/([^/]+/[^/]+)/git/refs", path)
    if match and match.group(1) == repo and method == "POST":
        tag = fields["ref"].split("refs/tags/", 1)[-1]
        if state.get("create_conflict") or tag in state.get("tag_refs", {}):
            # Simulate a concurrent writer that won the race.
            state.setdefault("tag_refs", {})[tag] = {
                "sha": state.get("conflict_sha", fields["sha"]),
                "type": "commit",
            }
            save_state(state)
            return fail("422 Reference already exists")
        state.setdefault("tag_refs", {})[tag] = {"sha": fields["sha"], "type": "commit"}
        state.setdefault("created_tags", []).append([tag, fields["sha"]])
        save_state(state)
        return emit({"ref": fields["ref"], "object": {"sha": fields["sha"], "type": "commit"}})

    return fail("unsupported api path: " + path)


sys.exit(main())
'''

GO_MOD_BASE = (
    "module github.com/fengwk/cliproxyapi-observer\n"
    "\n"
    "go 1.26.0\n"
    "\n"
    "require (\n"
    "\tgo.etcd.io/bbolt v1.4.3\n"
    "\tgithub.com/router-for-me/CLIProxyAPI/v8 v8.0.16\n"
    "\tgopkg.in/yaml.v3 v3.0.1\n"
    ")\n"
)
GO_MOD_BUMP = GO_MOD_BASE.replace("v8.0.16", "v8.0.17")
GO_MOD_BUMP2 = GO_MOD_BASE.replace("v8.0.16", "v8.0.18")
GO_MOD_V9 = GO_MOD_BASE.replace("CLIProxyAPI/v8 v8.0.16", "CLIProxyAPI/v9 v9.0.0")
GO_MOD_INDIRECT = GO_MOD_BASE + "\nrequire github.com/example/transitive v1.2.3 // indirect\n"
CI_BASE = (
    "name: ci\n"
    "on:\n"
    "  push:\n"
    "    branches: [main]\n"
    "permissions:\n"
    "  contents: read\n"
    "jobs:\n"
    "  test:\n"
    "    runs-on: ubuntu-latest\n"
    "    steps:\n"
    "      - uses: actions/checkout@v4\n"
)
CI_BUMP = CI_BASE.replace("actions/checkout@v4", "actions/checkout@v4.1.0")

WORKFLOW_BRANCH = "dependabot/go_modules/github.com/router-for-me/CLIProxyAPI/v8-abc"
ACTIONS_BRANCH = "dependabot/github_actions/actions/checkout-abc"


def write_executable(path, content):
    with open(path, "w", encoding="utf-8") as handle:
        handle.write(content)
    os.chmod(path, 0o755)


class ControllerTest(unittest.TestCase):
    def setUp(self):
        self.work = tempfile.mkdtemp(prefix="auto-release-")
        self.addCleanup(shutil.rmtree, self.work, ignore_errors=True)
        self.gh_bin = os.path.join(self.work, "gh")
        write_executable(self.gh_bin, FAKE_GH)
        self.repo_dir = os.path.join(self.work, "repo")
        os.makedirs(self.repo_dir)
        self.git("init", "-q")
        self.git("checkout", "-q", "-b", "main")
        self.git("config", "user.email", "test@example.com")
        self.git("config", "user.name", "Test")

    # -- git fixture helpers ------------------------------------------------ #

    def git(self, *args):
        proc = subprocess.run(
            ["git", "-C", self.repo_dir] + list(args), capture_output=True, text=True
        )
        if proc.returncode != 0:
            raise AssertionError("git %s failed: %s" % (" ".join(args), proc.stderr))
        return proc.stdout.strip()

    def commit(self, files, message):
        for rel, content in files.items():
            path = os.path.join(self.repo_dir, rel)
            os.makedirs(os.path.dirname(path), exist_ok=True)
            with open(path, "w", encoding="utf-8") as handle:
                handle.write(content)
        self.git("add", "-A")
        self.git("commit", "-q", "-m", message)
        return self.git("rev-parse", "HEAD")

    # -- fixture state helpers --------------------------------------------- #

    @staticmethod
    def run_dict(sha, run_id=555, **overrides):
        run = {
            "id": run_id,
            "name": "ci",
            "path": ".github/workflows/ci.yml",
            "head_branch": "main",
            "head_sha": sha,
            "event": "push",
            "status": "completed",
            "conclusion": "success",
            "workflow_id": CI_WORKFLOW_ID,
            "repository": {"full_name": REPO},
            "head_repository": {"full_name": REPO},
        }
        run.update(overrides)
        return run

    @staticmethod
    def pr_dict(sha, branch=WORKFLOW_BRANCH, author="dependabot[bot]", **overrides):
        # Real shape of GET repos/{owner}/{repo}/commits/{sha}/pulls: there is no
        # boolean "merged" field; a merge is state=closed plus merged_at.
        pr = {
            "state": "closed",
            "merged_at": "2024-01-01T00:00:00Z",
            "merge_commit_sha": sha,
            "base": {"ref": "main"},
            "head": {"ref": branch, "repo": {"full_name": REPO}},
            "user": {"login": author},
        }
        pr.update(overrides)
        return pr

    def standard_repo(self):
        """Base commit tagged v0.1.0 plus one eligible CPA bump on main."""
        base = self.commit(
            {
                "go.mod": GO_MOD_BASE,
                "go.sum": "base\n",
                ".github/workflows/ci.yml": CI_BASE,
                "README.md": "# plugin\n",
            },
            "base",
        )
        self.git("tag", "v0.1.0")
        head = self.commit({"go.mod": GO_MOD_BUMP, "go.sum": "bump\n"}, "bump cpa")
        return base, head

    def base_state(self, *, main_sha, releases, commit_pulls, tag_refs=None, ci_runs=None,
                   release_runs=None, commit_resolve=None, run_by_id=None, **extra):
        state = {
            "repo": REPO,
            "default_branch": "main",
            "main_sha": main_sha,
            "ci_workflow_id": CI_WORKFLOW_ID,
            "releases": releases,
            "tag_refs": tag_refs or {},
            "commit_pulls": commit_pulls,
            "commit_resolve": commit_resolve or {},
            "ci_runs": ci_runs or [],
            "release_runs": release_runs or [],
            "run_by_id": run_by_id or {},
        }
        state.update(extra)
        return state

    @staticmethod
    def published_release(tag="v0.1.0"):
        return {
            "tag_name": tag,
            "draft": False,
            "prerelease": False,
            "author": {"login": "github-actions[bot]"},
        }

    # -- controller runner -------------------------------------------------- #

    def run_controller(self, state, env=None, reset_state=True):
        state_path = os.path.join(self.work, "state.json")
        log_path = os.path.join(self.work, "gh.log")
        # The state is always persisted so a follow-up reconciliation observes
        # the mutations the previous run made (tags, dispatches, ...).
        with open(state_path, "w", encoding="utf-8") as handle:
            json.dump(state, handle)
        if reset_state:
            open(log_path, "w", encoding="utf-8").close()
        full_env = dict(os.environ)
        full_env.update(
            {
                "GH_BIN": self.gh_bin,
                "GH_REPO": state.get("repo", REPO),
                "REPO_ROOT": self.repo_dir,
                "FAKE_GH_STATE": state_path,
                "FAKE_GH_LOG": log_path,
                "CI_RUN_ID": "",
                "EXPECTED_HEAD_SHA": "",
            }
        )
        if env:
            full_env.update(env)
        proc = subprocess.run(
            [sys.executable, SCRIPT], env=full_env, capture_output=True, text=True, cwd=self.work
        )
        with open(state_path, "r", encoding="utf-8") as handle:
            final_state = json.load(handle)
        return proc.returncode, proc.stdout, proc.stderr, final_state

    # -- assertions --------------------------------------------------------- #

    def assert_skip(self, result, needle=None):
        code, out, err, state = result
        self.assertEqual(code, 0, "expected a safe no-op, got rc=%d stderr=%s" % (code, err))
        self.assertIn("skip:", out)
        if needle:
            self.assertIn(needle, out)
        self.assertEqual(state.get("created_tags", []), [])
        self.assertEqual(state.get("dispatches", []), [])

    @staticmethod
    def ci_dispatches(final):
        return [args for args in final.get("dispatches", []) if any("ci.yml" in t for t in args)]

    @staticmethod
    def release_dispatches(final):
        return [
            args for args in final.get("dispatches", []) if any("release.yml" in t for t in args)
        ]

    def test_single_dependency_bump_creates_tag_and_dispatches(self):
        base, head = self.standard_repo()
        state = self.base_state(
            main_sha=head,
            releases=[self.published_release()],
            commit_resolve={"v0.1.0": base, base: base, head: head},
            commit_pulls={head: [self.pr_dict(head)]},
            ci_runs=[self.run_dict(head)],
        )
        code, out, err, final = self.run_controller(state)
        self.assertEqual(code, 0, err)
        self.assertEqual(final.get("created_tags"), [["v0.1.1", head]])
        self.assertEqual(len(final.get("dispatches", [])), 1)
        self.assertIn("--ref", final["dispatches"][0])
        self.assertIn("main", final["dispatches"][0])
        self.assertIn("tag=v0.1.1", final["dispatches"][0])

    def test_official_actions_bump_is_released(self):
        base = self.commit(
            {"go.mod": GO_MOD_BASE, ".github/workflows/ci.yml": CI_BASE, "LICENSE": "x\n"},
            "base",
        )
        self.git("tag", "v0.1.0")
        head = self.commit({".github/workflows/ci.yml": CI_BUMP}, "bump checkout")
        state = self.base_state(
            main_sha=head,
            releases=[self.published_release()],
            commit_resolve={"v0.1.0": base, base: base, head: head},
            commit_pulls={head: [self.pr_dict(head, branch=ACTIONS_BRANCH)]},
            ci_runs=[self.run_dict(head)],
        )
        code, out, err, final = self.run_controller(state)
        self.assertEqual(code, 0, err)
        self.assertEqual(final.get("created_tags"), [["v0.1.1", head]])

    def test_cumulative_dependency_commits_release_once(self):
        base = self.commit({"go.mod": GO_MOD_BASE, "go.sum": "a\n"}, "base")
        self.git("tag", "v0.1.0")
        first = self.commit({"go.mod": GO_MOD_BUMP, "go.sum": "b\n"}, "bump 1")
        second = self.commit({"go.mod": GO_MOD_BUMP2, "go.sum": "c\n"}, "bump 2")
        state = self.base_state(
            main_sha=second,
            releases=[self.published_release()],
            commit_resolve={"v0.1.0": base, base: base, first: first, second: second},
            commit_pulls={first: [self.pr_dict(first)], second: [self.pr_dict(second)]},
            ci_runs=[self.run_dict(second)],
        )
        code, out, err, final = self.run_controller(state)
        self.assertEqual(code, 0, err)
        self.assertEqual(final.get("created_tags"), [["v0.1.1", second]])

    def test_manual_commit_is_skipped(self):
        base = self.commit({"go.mod": GO_MOD_BASE}, "base")
        self.git("tag", "v0.1.0")
        head = self.commit({"README.md": "# changed by a human\n"}, "manual change")
        state = self.base_state(
            main_sha=head,
            releases=[self.published_release()],
            commit_resolve={"v0.1.0": base, base: base, head: head},
            commit_pulls={head: [self.pr_dict(head, branch="feature/x", author="octocat")]},
            ci_runs=[self.run_dict(head)],
        )
        self.assert_skip(self.run_controller(state), "not an eligible dependency update")

    def test_dependabot_source_change_is_skipped(self):
        base = self.commit({"go.mod": GO_MOD_BASE}, "base")
        self.git("tag", "v0.1.0")
        head = self.commit({"README.md": "# dependabot touched docs\n"}, "doc change")
        state = self.base_state(
            main_sha=head,
            releases=[self.published_release()],
            commit_resolve={"v0.1.0": base, base: base, head: head},
            commit_pulls={head: [self.pr_dict(head)]},
            ci_runs=[self.run_dict(head)],
        )
        self.assert_skip(self.run_controller(state), "not an eligible dependency update")

    def test_dependabot_pr_from_fork_is_skipped(self):
        base = self.commit({"go.mod": GO_MOD_BASE}, "base")
        self.git("tag", "v0.1.0")
        head = self.commit({"go.mod": GO_MOD_BUMP}, "fork bump")
        pr = self.pr_dict(head)
        pr["head"]["repo"] = {"full_name": "fork/repo"}
        state = self.base_state(
            main_sha=head,
            releases=[self.published_release()],
            commit_resolve={"v0.1.0": base, base: base, head: head},
            commit_pulls={head: [pr]},
            ci_runs=[self.run_dict(head)],
        )
        self.assert_skip(self.run_controller(state), "no unique merged same-repo Dependabot")

    def test_malformed_dependabot_branch_is_skipped(self):
        base = self.commit({"go.mod": GO_MOD_BASE}, "base")
        self.git("tag", "v0.1.0")
        head = self.commit({"go.mod": GO_MOD_BUMP}, "odd branch bump")
        state = self.base_state(
            main_sha=head,
            releases=[self.published_release()],
            commit_resolve={"v0.1.0": base, base: base, head: head},
            commit_pulls={head: [self.pr_dict(head, branch="maintenance/bump")]},
            ci_runs=[self.run_dict(head)],
        )
        self.assert_skip(self.run_controller(state), "not an eligible dependency update")

    def test_v9_bump_is_skipped(self):
        base = self.commit({"go.mod": GO_MOD_BASE}, "base")
        self.git("tag", "v0.1.0")
        head = self.commit({"go.mod": GO_MOD_V9}, "v9 bump")
        state = self.base_state(
            main_sha=head,
            releases=[self.published_release()],
            commit_resolve={"v0.1.0": base, base: base, head: head},
            commit_pulls={head: [self.pr_dict(head)]},
            ci_runs=[self.run_dict(head)],
        )
        self.assert_skip(self.run_controller(state), "not an eligible dependency update")

    def test_reverted_range_is_skipped(self):
        base = self.commit({"go.mod": GO_MOD_BASE}, "base")
        self.git("tag", "v0.1.0")
        added = self.commit({"go.mod": GO_MOD_INDIRECT}, "add indirect")
        reverted = self.commit({"go.mod": GO_MOD_BASE}, "remove indirect")
        state = self.base_state(
            main_sha=reverted,
            releases=[self.published_release()],
            commit_resolve={
                "v0.1.0": base,
                base: base,
                added: added,
                reverted: reverted,
            },
            commit_pulls={added: [self.pr_dict(added)], reverted: [self.pr_dict(reverted)]},
            ci_runs=[self.run_dict(reverted)],
        )
        self.assert_skip(self.run_controller(state), "no net change")

    # -- event-driven selection -------------------------------------------- #

    def test_event_run_success_releases(self):
        base, head = self.standard_repo()
        state = self.base_state(
            main_sha=head,
            releases=[self.published_release()],
            commit_resolve={"v0.1.0": base, base: base, head: head},
            commit_pulls={head: [self.pr_dict(head)]},
            run_by_id={"555": self.run_dict(head)},
        )
        code, out, err, final = self.run_controller(
            state, env={"CI_RUN_ID": "555", "EXPECTED_HEAD_SHA": head}
        )
        self.assertEqual(code, 0, err)
        self.assertEqual(final.get("created_tags"), [["v0.1.1", head]])

    def test_event_run_from_fork_is_skipped(self):
        base, head = self.standard_repo()
        run = self.run_dict(head)
        run["repository"] = {"full_name": "fork/repo"}
        state = self.base_state(
            main_sha=head,
            releases=[self.published_release()],
            commit_resolve={"v0.1.0": base, base: base, head: head},
            commit_pulls={head: [self.pr_dict(head)]},
            run_by_id={"555": run},
        )
        self.assert_skip(
            self.run_controller(state, env={"CI_RUN_ID": "555", "EXPECTED_HEAD_SHA": head}),
            "same-repo default-branch ci run",
        )

    def test_event_run_pr_event_is_skipped(self):
        base, head = self.standard_repo()
        state = self.base_state(
            main_sha=head,
            releases=[self.published_release()],
            commit_resolve={"v0.1.0": base, base: base, head: head},
            commit_pulls={head: [self.pr_dict(head)]},
            run_by_id={"555": self.run_dict(head, event="pull_request")},
        )
        self.assert_skip(
            self.run_controller(state, env={"CI_RUN_ID": "555", "EXPECTED_HEAD_SHA": head}),
            "same-repo default-branch ci run",
        )

    def test_event_run_failed_conclusion_is_skipped(self):
        base, head = self.standard_repo()
        state = self.base_state(
            main_sha=head,
            releases=[self.published_release()],
            commit_resolve={"v0.1.0": base, base: base, head: head},
            commit_pulls={head: [self.pr_dict(head)]},
            run_by_id={"555": self.run_dict(head, conclusion="failure")},
        )
        self.assert_skip(
            self.run_controller(state, env={"CI_RUN_ID": "555", "EXPECTED_HEAD_SHA": head}),
            "did not complete successfully",
        )

    def test_event_expected_sha_mismatch_is_skipped(self):
        base, head = self.standard_repo()
        state = self.base_state(
            main_sha=head,
            releases=[self.published_release()],
            commit_resolve={"v0.1.0": base, base: base, head: head},
            commit_pulls={head: [self.pr_dict(head)]},
            run_by_id={"555": self.run_dict(head)},
        )
        self.assert_skip(
            self.run_controller(state, env={"CI_RUN_ID": "555", "EXPECTED_HEAD_SHA": "other"}),
            "EXPECTED_HEAD_SHA",
        )

    def test_event_run_stale_main_is_skipped(self):
        base, head = self.standard_repo()
        newer = self.commit({"README.md": "# newer\n"}, "advance main")
        state = self.base_state(
            main_sha=newer,
            releases=[self.published_release()],
            commit_resolve={"v0.1.0": base, base: base, head: head, newer: newer},
            commit_pulls={head: [self.pr_dict(head)]},
            run_by_id={"555": self.run_dict(head)},
        )
        self.assert_skip(
            self.run_controller(state, env={"CI_RUN_ID": "555", "EXPECTED_HEAD_SHA": head}),
            "default branch advanced",
        )

    # -- discovery selection ----------------------------------------------- #

    def test_schedule_dispatches_missing_main_ci_for_eligible_range(self):
        # The automerge main-ci dispatch can fail after a GITHUB_TOKEN merge; a
        # schedule must be able to re-request ci for an eligible range.
        base, head = self.standard_repo()
        state = self.base_state(
            main_sha=head,
            releases=[self.published_release()],
            commit_resolve={"v0.1.0": base, base: base, head: head},
            commit_pulls={head: [self.pr_dict(head)]},
            ci_runs=[self.run_dict(head, conclusion="failure")],
        )
        code, out, err, final = self.run_controller(state)
        self.assertEqual(code, 0, err)
        self.assertIn("dispatched the main ci run", out)
        self.assertEqual(final.get("created_tags", []), [])
        self.assertEqual(self.release_dispatches(final), [])
        ci = self.ci_dispatches(final)
        self.assertEqual(len(ci), 1)
        self.assertIn("--ref", ci[0])
        self.assertIn("main", ci[0])

    def test_schedule_waits_for_active_main_ci(self):
        base, head = self.standard_repo()
        state = self.base_state(
            main_sha=head,
            releases=[self.published_release()],
            commit_resolve={"v0.1.0": base, base: base, head: head},
            commit_pulls={head: [self.pr_dict(head)]},
            ci_runs=[self.run_dict(head, status="in_progress", conclusion=None)],
        )
        self.assert_skip(self.run_controller(state), "already active")

    def test_schedule_does_not_dispatch_ci_for_ineligible_range(self):
        base = self.commit({"go.mod": GO_MOD_BASE}, "base")
        self.git("tag", "v0.1.0")
        head = self.commit({"README.md": "# manual\n"}, "manual")
        state = self.base_state(
            main_sha=head,
            releases=[self.published_release()],
            commit_resolve={"v0.1.0": base, base: base, head: head},
            commit_pulls={head: [self.pr_dict(head, branch="feature/x", author="octocat")]},
            ci_runs=[],
        )
        self.assert_skip(self.run_controller(state), "not an eligible dependency update")

    def test_schedule_does_not_dispatch_ci_for_released_sha(self):
        base, head = self.standard_repo()
        state = self.base_state(
            main_sha=head,
            releases=[self.published_release("v0.1.1")],
            commit_resolve={"v0.1.1": head, base: base, head: head},
            commit_pulls={},
            ci_runs=[],
        )
        self.assert_skip(self.run_controller(state), "no new commits")

    def test_discovery_finds_main_ci_run_across_pages(self):
        base = self.commit({"go.mod": GO_MOD_BASE, "go.sum": "a\n"}, "base")
        self.git("tag", "v0.1.0")
        first = self.commit({"go.mod": GO_MOD_BUMP, "go.sum": "b\n"}, "bump1")
        head = self.commit({"go.mod": GO_MOD_BUMP2, "go.sum": "c\n"}, "bump2")
        state = self.base_state(
            main_sha=head,
            page_size=1,
            releases=[self.published_release()],
            commit_resolve={"v0.1.0": base, base: base, first: first, head: head},
            commit_pulls={first: [self.pr_dict(first)], head: [self.pr_dict(head)]},
            ci_runs=[self.run_dict(first, run_id=1), self.run_dict(head, run_id=2)],
        )
        code, out, err, final = self.run_controller(state)
        self.assertEqual(code, 0, err)
        self.assertEqual(final.get("created_tags"), [["v0.1.1", head]])

    def test_active_release_run_detected_across_pages(self):
        base, head = self.standard_repo()
        state = self.base_state(
            main_sha=head,
            page_size=1,
            releases=[self.published_release()],
            tag_refs={"v0.1.1": {"sha": head, "type": "commit"}},
            commit_resolve={"v0.1.0": base, base: base, head: head},
            commit_pulls={head: [self.pr_dict(head)]},
            ci_runs=[self.run_dict(head)],
            release_runs=[
                {"display_title": "release v0.0.9", "status": "completed", "conclusion": "failure"},
                {"display_title": "release v0.1.1", "status": "in_progress", "conclusion": None},
            ],
        )
        self.assert_skip(self.run_controller(state), "still running")

    def test_open_dependabot_pr_is_skipped(self):
        base, head = self.standard_repo()
        state = self.base_state(
            main_sha=head,
            releases=[self.published_release()],
            commit_resolve={"v0.1.0": base, base: base, head: head},
            commit_pulls={head: [self.pr_dict(head, state="open", merged_at=None)]},
            ci_runs=[self.run_dict(head)],
        )
        self.assert_skip(self.run_controller(state), "no unique merged same-repo Dependabot")

    def test_closed_unmerged_dependabot_pr_is_skipped(self):
        base, head = self.standard_repo()
        state = self.base_state(
            main_sha=head,
            releases=[self.published_release()],
            commit_resolve={"v0.1.0": base, base: base, head: head},
            commit_pulls={head: [self.pr_dict(head, merged_at=None)]},
            ci_runs=[self.run_dict(head)],
        )
        self.assert_skip(self.run_controller(state), "no unique merged same-repo Dependabot")

    def test_malformed_merged_at_is_skipped(self):
        base, head = self.standard_repo()
        state = self.base_state(
            main_sha=head,
            releases=[self.published_release()],
            commit_resolve={"v0.1.0": base, base: base, head: head},
            commit_pulls={head: [self.pr_dict(head, merged_at="yesterday")]},
            ci_runs=[self.run_dict(head)],
        )
        self.assert_skip(self.run_controller(state), "no unique merged same-repo Dependabot")

    def test_discovery_ignores_stale_run_and_picks_matching(self):
        base = self.commit({"go.mod": GO_MOD_BASE}, "base")
        self.git("tag", "v0.1.0")
        older = self.commit({"go.mod": GO_MOD_BUMP, "go.sum": "x\n"}, "bump1")
        head = self.commit({"go.mod": GO_MOD_BUMP2, "go.sum": "y\n"}, "bump2")
        state = self.base_state(
            main_sha=head,
            releases=[self.published_release()],
            commit_resolve={"v0.1.0": base, base: base, older: older, head: head},
            commit_pulls={older: [self.pr_dict(older)], head: [self.pr_dict(head)]},
            ci_runs=[self.run_dict(older, run_id=1), self.run_dict(head, run_id=2)],
        )
        code, out, err, final = self.run_controller(state)
        self.assertEqual(code, 0, err)
        self.assertEqual(final.get("created_tags"), [["v0.1.1", head]])

    def test_discovery_ignores_untrusted_ci_runs(self):
        base, head = self.standard_repo()
        mutations = {
            "foreign repository": {"repository": {"full_name": "fork/repo"}},
            "foreign head repository": {"head_repository": {"full_name": "fork/repo"}},
            "wrong workflow id": {"workflow_id": 999},
            "wrong path": {"path": ".github/workflows/other.yml"},
            "wrong name": {"name": "other"},
            "wrong branch": {"head_branch": "feature/x"},
        }
        for label, override in mutations.items():
            with self.subTest(label):
                state = self.base_state(
                    main_sha=head,
                    releases=[self.published_release()],
                    commit_resolve={"v0.1.0": base, base: base, head: head},
                    commit_pulls={head: [self.pr_dict(head)]},
                    ci_runs=[self.run_dict(head, **override)],
                )
                code, out, err, final = self.run_controller(state)
                self.assertEqual(code, 0, err)
                # The untrusted run never authorizes a release; the eligible,
                # unpublished range falls back to a fresh ci dispatch instead.
                self.assertEqual(final.get("created_tags", []), [])
                self.assertEqual(len(self.ci_dispatches(final)), 1)
                self.assertEqual(self.release_dispatches(final), [])

    # -- release base problems --------------------------------------------- #

    def test_non_ancestor_release_is_skipped(self):
        base, head = self.standard_repo()
        # A commit that exists locally but is not an ancestor of main.
        self.git("checkout", "-q", "-b", "side", base)
        side = self.commit({"README.md": "# side\n"}, "side branch")
        self.git("checkout", "-q", "main")
        state = self.base_state(
            main_sha=head,
            releases=[self.published_release()],
            commit_resolve={"v0.1.0": side, side: side, base: base, head: head},
            commit_pulls={head: [self.pr_dict(head)]},
            ci_runs=[self.run_dict(head)],
        )
        self.assert_skip(self.run_controller(state), "not an ancestor of the candidate")

    def test_malformed_release_tag_is_ignored(self):
        base, head = self.standard_repo()
        state = self.base_state(
            main_sha=head,
            releases=[
                {"tag_name": "release-1", "draft": False, "prerelease": False,
                 "author": {"login": "github-actions[bot]"}},
            ],
            commit_resolve={"v0.1.0": base, base: base, head: head},
            commit_pulls={head: [self.pr_dict(head)]},
            ci_runs=[self.run_dict(head)],
        )
        self.assert_skip(self.run_controller(state), "no published stable release")

    def test_unchanged_source_is_skipped(self):
        base, head = self.standard_repo()
        # The published release already points at the current head.
        state = self.base_state(
            main_sha=head,
            releases=[self.published_release("v0.1.1")],
            commit_resolve={"v0.1.1": head, base: base, head: head},
            commit_pulls={},
            ci_runs=[self.run_dict(head)],
        )
        self.assert_skip(self.run_controller(state), "no new commits")

    # -- pending / existing tags ------------------------------------------- #

    def test_pending_tag_same_sha_redispatches(self):
        base, head = self.standard_repo()
        state = self.base_state(
            main_sha=head,
            releases=[self.published_release()],
            tag_refs={"v0.1.1": {"sha": head, "type": "commit"}},
            commit_resolve={"v0.1.0": base, base: base, head: head, "v0.1.1": head},
            commit_pulls={head: [self.pr_dict(head)]},
            ci_runs=[self.run_dict(head)],
        )
        code, out, err, final = self.run_controller(state)
        self.assertEqual(code, 0, err)
        self.assertEqual(final.get("created_tags", []), [])
        self.assertEqual(len(final.get("dispatches", [])), 1)
        self.assertIn("tag=v0.1.1", final["dispatches"][0])

    def test_pending_tag_older_sha_redispatches_without_new_tag(self):
        base = self.commit({"go.mod": GO_MOD_BASE, "go.sum": "a\n"}, "base")
        self.git("tag", "v0.1.0")
        tagged = self.commit({"go.mod": GO_MOD_BUMP, "go.sum": "b\n"}, "bump1")
        head = self.commit({"go.mod": GO_MOD_BUMP2, "go.sum": "c\n"}, "bump2")
        state = self.base_state(
            main_sha=head,
            releases=[self.published_release()],
            tag_refs={"v0.1.1": {"sha": tagged, "type": "commit"}},
            commit_resolve={"v0.1.0": base, base: base, tagged: tagged, head: head},
            commit_pulls={tagged: [self.pr_dict(tagged)], head: [self.pr_dict(head)]},
            ci_runs=[self.run_dict(head), self.run_dict(tagged)],
        )
        code, out, err, final = self.run_controller(state)
        self.assertEqual(code, 0, err)
        self.assertEqual(final.get("created_tags", []), [])
        self.assertEqual(len(final.get("dispatches", [])), 1)
        self.assertIn("tag=v0.1.1", final["dispatches"][0])

    def test_collision_tag_is_skipped(self):
        base = self.commit({"go.mod": GO_MOD_BASE}, "base")
        self.git("tag", "v0.1.0")
        head = self.commit({"README.md": "# manual\n"}, "manual")
        state = self.base_state(
            main_sha=head,
            releases=[self.published_release()],
            tag_refs={"v0.1.1": {"sha": head, "type": "commit"}},
            commit_resolve={"v0.1.0": base, base: base, head: head},
            commit_pulls={head: [self.pr_dict(head, branch="feature/x", author="octocat")]},
            ci_runs=[self.run_dict(head)],
        )
        self.assert_skip(self.run_controller(state), "not an eligible dependency update")

    def test_create_ref_conflict_reconciles_without_duplicate(self):
        base, head = self.standard_repo()
        state = self.base_state(
            main_sha=head,
            releases=[self.published_release()],
            commit_resolve={"v0.1.0": base, base: base, head: head},
            commit_pulls={head: [self.pr_dict(head)]},
            ci_runs=[self.run_dict(head)],
            create_conflict=True,
        )
        code, out, err, final = self.run_controller(state)
        self.assertEqual(code, 0, err)
        # The concurrent writer's tag is the only one; we reconcile, not recreate.
        self.assertEqual(final.get("created_tags", []), [])
        self.assertEqual(len(final.get("dispatches", [])), 1)
        self.assertIn("tag=v0.1.1", final["dispatches"][0])

    def test_dispatch_failure_is_retried_with_same_tag(self):
        base, head = self.standard_repo()
        state = self.base_state(
            main_sha=head,
            releases=[self.published_release()],
            commit_resolve={"v0.1.0": base, base: base, head: head},
            commit_pulls={head: [self.pr_dict(head)]},
            ci_runs=[self.run_dict(head)],
            dispatch_fail=True,
        )
        code, out, err, final = self.run_controller(state)
        self.assertEqual(code, 1)
        self.assertEqual(final.get("created_tags"), [["v0.1.1", head]])
        self.assertEqual(final.get("dispatches", []), [])

        # Next reconciliation: the tag already exists and no release run was
        # ever dispatched, so the exact same tag is retried.
        final["dispatch_fail"] = False
        code, out, err, final = self.run_controller(final, reset_state=False)
        self.assertEqual(code, 0, err)
        self.assertEqual(len(final.get("dispatches", [])), 1)
        self.assertIn("tag=v0.1.1", final["dispatches"][0])
        self.assertEqual(final.get("created_tags"), [["v0.1.1", head]])

    # -- release workflow run reconciliation -------------------------------- #

    def test_active_release_run_waits(self):
        base, head = self.standard_repo()
        state = self.base_state(
            main_sha=head,
            releases=[self.published_release()],
            tag_refs={"v0.1.1": {"sha": head, "type": "commit"}},
            commit_resolve={"v0.1.0": base, base: base, head: head},
            commit_pulls={head: [self.pr_dict(head)]},
            ci_runs=[self.run_dict(head)],
            release_runs=[
                {"display_title": "release v0.1.1", "status": "in_progress", "conclusion": None}
            ],
        )
        self.assert_skip(self.run_controller(state), "still running")

    def test_failed_release_run_is_retried(self):
        base, head = self.standard_repo()
        state = self.base_state(
            main_sha=head,
            releases=[self.published_release()],
            tag_refs={"v0.1.1": {"sha": head, "type": "commit"}},
            commit_resolve={"v0.1.0": base, base: base, head: head},
            commit_pulls={head: [self.pr_dict(head)]},
            ci_runs=[self.run_dict(head)],
            release_runs=[
                {"display_title": "release v0.1.1", "status": "completed", "conclusion": "failure"}
            ],
        )
        code, out, err, final = self.run_controller(state)
        self.assertEqual(code, 0, err)
        self.assertEqual(len(final.get("dispatches", [])), 1)

    def test_second_run_does_not_duplicate_tag(self):
        base, head = self.standard_repo()
        state = self.base_state(
            main_sha=head,
            releases=[self.published_release()],
            commit_resolve={"v0.1.0": base, base: base, head: head},
            commit_pulls={head: [self.pr_dict(head)]},
            ci_runs=[self.run_dict(head)],
        )
        _, _, _, final = self.run_controller(state)
        self.assertEqual(final.get("created_tags"), [["v0.1.1", head]])
        _, _, _, final = self.run_controller(final, reset_state=False)
        self.assertEqual(final.get("created_tags"), [["v0.1.1", head]])

    def test_published_next_release_is_noop(self):
        base, head = self.standard_repo()
        state = self.base_state(
            main_sha=head,
            releases=[self.published_release(), self.published_release("v0.1.1")],
            commit_resolve={"v0.1.0": base, base: base, head: head, "v0.1.1": head},
            commit_pulls={head: [self.pr_dict(head)]},
            ci_runs=[self.run_dict(head)],
        )
        # v0.1.1 is published, so the next version becomes v0.1.2 with no new
        # commits beyond it -> safe no-op.
        self.assert_skip(self.run_controller(state), "skip:")

    def test_foreign_run_repository_is_rejected(self):
        base, head = self.standard_repo()
        run = self.run_dict(head)
        run["repository"] = {"full_name": "attacker/repo"}
        state = self.base_state(
            main_sha=head,
            releases=[self.published_release()],
            commit_resolve={"v0.1.0": base, base: base, head: head},
            commit_pulls={head: [self.pr_dict(head)]},
            run_by_id={"555": run},
        )
        self.assert_skip(
            self.run_controller(state, env={"CI_RUN_ID": "555", "EXPECTED_HEAD_SHA": head}),
            "same-repo default-branch ci run",
        )

    def test_missing_gh_repo_fails(self):
        code, out, err, _ = self.run_controller(
            self.base_state(main_sha="x", releases=[], commit_pulls={}), env={"GH_REPO": ""}
        )
        self.assertEqual(code, 2)
        self.assertIn("GH_REPO is required", err)


if __name__ == "__main__":
    unittest.main(verbosity=2)
