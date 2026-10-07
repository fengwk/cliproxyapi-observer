#!/usr/bin/env python3
"""Unattended, fail-closed dependency-only patch release controller.

This controller turns a successful full ``ci`` run on the default branch into a
new patch release **only** when every commit added since the last published
release is a routine Dependabot dependency bump. It is intentionally
conservative: any commit with unknown provenance, any code/configuration/
permission change, an empty or reverted range, a stale default branch, a
collision on the next tag, or a pending (possibly manual) release is a safe
no-op rather than a destructive action.

Trust boundary:

* The controller runs from a trusted checkout of the default branch
  (``fetch-depth: 0``). It never checks out, reads or executes pull-request
  code; per-commit provenance comes from the GitHub API and per-commit content
  is read from the immutable local history.
* ``GH_REPO`` must be supplied explicitly; there is no repository inference.
* Tag creation goes through the atomic refs API and is bound to the exact
  validated commit. A tag is never moved or overwritten.
* The tag workflow is dispatched explicitly because a tag created with
  ``GITHUB_TOKEN`` does not emit a ``push`` event.

Eligibility is delegated to the shared policy module
``scripts/dependency_policy.py`` (``evaluate_update``), which is imported
directly so the tests exercise the real security assertions.

Python standard library only.
"""

import json
import os
import re
import subprocess
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import dependency_policy  # noqa: E402  (local module)

TAG_RE = re.compile(r"^v(\d+)\.(\d+)\.(\d+)$")
WORKFLOW_PATH_RE = re.compile(r"^\.github/workflows/[^/]+\.(?:yml|yaml)$")
# GitHub returns merge timestamps as ``2024-01-01T00:00:00Z`` (optionally with
# fractional seconds or an offset).
_ISO8601_RE = re.compile(r"^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})$")
CI_PATH = ".github/workflows/ci.yml"
CI_NAME = "ci"
RELEASE_WORKFLOW = "release.yml"
DEPENDABOT_AUTHOR = "dependabot[bot]"
AUTOMATION_AUTHOR = "github-actions[bot]"
CI_EVENTS = ("push", "workflow_dispatch")

# A release workflow run in one of these states is still being processed; the
# controller waits instead of duplicating it.
ACTIVE_STATUSES = frozenset(
    {"queued", "in_progress", "waiting", "requested", "pending", "action_required"}
)


class Skip(Exception):
    """An expected, safe no-op. The controller exits successfully."""


class Failure(Exception):
    """An unexpected, reportable problem. The controller exits non-zero."""


def _tag_in_title(title, tag):
    """True when ``title`` mentions ``tag`` as a standalone version token.

    Boundaries avoid matching ``v0.1.2`` inside ``v0.1.20`` when release runs
    are deduplicated by their display title.
    """
    if not title:
        return False
    return re.search(r"(?<![0-9A-Za-z.])" + re.escape(tag) + r"(?![0-9A-Za-z.])", title) is not None


class GitHub:
    """Minimal, repository-bound ``gh`` wrapper."""

    def __init__(self, repo, bin_path):
        self.repo = repo
        self.bin = bin_path
        self._ci_workflow_id = None

    def _gh(self, args):
        proc = subprocess.run([self.bin] + list(args), capture_output=True, text=True)
        if proc.returncode != 0:
            detail = (proc.stderr or proc.stdout or "").strip().splitlines()
            raise Failure("gh %s failed: %s" % (args[0], detail[0] if detail else "unknown error"))
        return proc.stdout

    def _get(self, path, paginate=False):
        """Return the parsed JSON payload for an API path.

        With ``paginate`` the payload is the list of page responses gh emits for
        ``--paginate --slurp``. The shape depends on the endpoint: array
        endpoints yield a page per list element, while the actions-runs
        endpoints yield an object envelope ``{total_count, workflow_runs}`` per
        page. Callers unpack the endpoint-specific shape.
        """
        args = ["api", path]
        if paginate:
            args += ["--paginate", "--slurp"]
        out = self._gh(args)
        try:
            return json.loads(out) if out.strip() else None
        except ValueError:
            raise Failure("gh returned malformed JSON for %s" % path)

    def default_branch(self):
        data = self._get("repos/" + self.repo)
        branch = (data or {}).get("default_branch")
        if not isinstance(branch, str) or not branch:
            raise Failure("the default branch could not be resolved")
        return branch

    def main_sha(self, branch):
        data = self._get("repos/%s/git/ref/heads/%s" % (self.repo, branch))
        sha = ((data or {}).get("object") or {}).get("sha")
        if not isinstance(sha, str) or not sha:
            raise Failure("the default branch head could not be resolved")
        return sha

    def ci_workflow_id(self):
        if self._ci_workflow_id is None:
            data = self._get("repos/%s/actions/workflows/ci.yml" % self.repo)
            self._ci_workflow_id = (data or {}).get("id")
        return self._ci_workflow_id

    def run(self, run_id):
        return self._get("repos/%s/actions/runs/%s" % (self.repo, run_id))

    def runs(self, branch):
        pages = self._get(
            "repos/%s/actions/runs?branch=%s&per_page=100" % (self.repo, branch), paginate=True
        )
        runs = []
        for page in pages or []:
            if isinstance(page, dict):
                runs.extend(page.get("workflow_runs") or [])
        return [run for run in runs if isinstance(run, dict)]

    def releases(self):
        pages = self._get("repos/%s/releases?per_page=100" % self.repo, paginate=True)
        raw_releases = []
        for page in pages or []:
            if isinstance(page, list):
                raw_releases.extend(page)
            elif isinstance(page, dict):
                raw_releases.append(page)
        releases = []
        for raw in raw_releases:
            if not isinstance(raw, dict):
                continue
            releases.append(
                {
                    "tag": raw.get("tag_name"),
                    "draft": bool(raw.get("draft")),
                    "prerelease": bool(raw.get("prerelease")),
                    "author": (raw.get("author") or {}).get("login"),
                }
            )
        return releases

    def tag_ref(self, tag):
        distinct = "refs/tags/" + tag
        data = self._get("repos/%s/git/matching-refs/tags/%s" % (self.repo, tag))
        for ref in data or []:
            if isinstance(ref, dict) and ref.get("ref") == distinct:
                obj = ref.get("object") or {}
                return {"sha": obj.get("sha"), "type": obj.get("type")}
        return None

    def deref_tag(self, sha):
        data = self._get("repos/%s/git/tags/%s" % (self.repo, sha))
        return ((data or {}).get("object") or {}).get("sha")

    def tag_commit(self, tag):
        data = self._get("repos/%s/commits/%s" % (self.repo, tag))
        sha = (data or {}).get("sha")
        if not isinstance(sha, str) or not sha:
            raise Failure("could not resolve the commit for tag %s" % tag)
        return sha

    def commit_pulls(self, sha):
        pages = self._get("repos/%s/commits/%s/pulls" % (self.repo, sha), paginate=True)
        pulls = []
        for page in pages or []:
            if isinstance(page, list):
                pulls.extend(page)
        return [pr for pr in pulls if isinstance(pr, dict)]

    def release_runs(self, tag):
        pages = self._get(
            "repos/%s/actions/workflows/%s/runs?per_page=100" % (self.repo, RELEASE_WORKFLOW),
            paginate=True,
        )
        runs = []
        for page in pages or []:
            if isinstance(page, dict):
                runs.extend(page.get("workflow_runs") or [])
        return [
            run
            for run in runs
            if isinstance(run, dict) and _tag_in_title(run.get("display_title"), tag)
        ]

    def create_tag(self, tag, sha):
        """Create a lightweight tag bound to ``sha``; False on a lost race."""
        proc = subprocess.run(
            [
                self.bin,
                "api",
                "--method",
                "POST",
                "repos/%s/git/refs" % self.repo,
                "-f",
                "ref=refs/tags/" + tag,
                "-f",
                "sha=" + sha,
            ],
            capture_output=True,
            text=True,
        )
        if proc.returncode == 0:
            return True
        # A conflict means another writer created the tag first; re-read instead
        # of guessing from the error text. Any other error is a real failure.
        if self.tag_ref(tag) is not None:
            return False
        raise Failure("could not create tag %s" % tag)

    def dispatch_release(self, branch, tag):
        self._gh(
            [
                "workflow",
                "run",
                RELEASE_WORKFLOW,
                "--repo",
                self.repo,
                "--ref",
                branch,
                "-f",
                "tag=" + tag,
            ]
        )

    def dispatch_ci(self, branch):
        self._gh(["workflow", "run", "ci.yml", "--repo", self.repo, "--ref", branch])


class LocalRepo:
    """Read-only helpers over the trusted, full-history checkout."""

    def __init__(self, root):
        self.root = root

    def git(self, *args, check=True):
        proc = subprocess.run(["git", "-C", self.root] + list(args), capture_output=True, text=True)
        if check and proc.returncode != 0:
            raise Failure("git %s failed" % " ".join(args))
        return proc

    def head_sha(self):
        return self.git("rev-parse", "HEAD").stdout.strip()

    def has_commit(self, sha):
        return self.git("cat-file", "-e", sha + "^{commit}", check=False).returncode == 0

    def is_ancestor(self, ancestor, descendant):
        return self.git("merge-base", "--is-ancestor", ancestor, descendant, check=False).returncode == 0

    def first_parent_range(self, base, head):
        out = self.git("rev-list", "--first-parent", "--reverse", "%s..%s" % (base, head)).stdout
        return [line for line in out.splitlines() if line]

    def parent(self, sha):
        return self.git("rev-parse", sha + "^").stdout.strip()

    def changed_files(self, base, head):
        out = self.git("diff", "--name-only", base, head).stdout
        return sorted(line for line in out.splitlines() if line)

    def file_at(self, rev, path):
        proc = self.git("show", "%s:%s" % (rev, path), check=False)
        return proc.stdout if proc.returncode == 0 else ""


# --------------------------------------------------------------------------- #
# Candidate selection
# --------------------------------------------------------------------------- #


def _trusted_ci_run(gh, run, default_branch, workflow_id):
    """The trust boundary a ci run must satisfy, shared by every selector."""
    return (
        run.get("name") == CI_NAME
        and run.get("path") == CI_PATH
        and run.get("workflow_id") == workflow_id
        and (run.get("repository") or {}).get("full_name") == gh.repo
        and (run.get("head_repository") or {}).get("full_name") == gh.repo
        and run.get("head_branch") == default_branch
        and run.get("event") in CI_EVENTS
    )


def verify_ci_run(gh, default_branch, run, expected):
    if not isinstance(run, dict):
        raise Skip("the triggering ci run could not be read")
    if not _trusted_ci_run(gh, run, default_branch, gh.ci_workflow_id()):
        raise Skip("the triggering run is not a same-repo default-branch ci run")
    if run.get("head_sha") != expected:
        raise Skip("the triggering run does not match EXPECTED_HEAD_SHA")
    if run.get("status") != "completed" or run.get("conclusion") != "success":
        raise Skip("the triggering run did not complete successfully")


def find_ci_run(gh, default_branch, sha):
    workflow_id = gh.ci_workflow_id()
    for run in gh.runs(default_branch):
        if (
            _trusted_ci_run(gh, run, default_branch, workflow_id)
            and run.get("head_sha") == sha
            and run.get("status") == "completed"
            and run.get("conclusion") == "success"
        ):
            return run
    return None


def find_active_ci_run(gh, default_branch, sha):
    workflow_id = gh.ci_workflow_id()
    for run in gh.runs(default_branch):
        if (
            _trusted_ci_run(gh, run, default_branch, workflow_id)
            and run.get("head_sha") == sha
            and run.get("status") in ACTIVE_STATUSES
        ):
            return run
    return None


def attempt_ci_recovery(gh, local, default_branch, candidate):
    """Dispatch a fresh main ci run for an eligible, unpublished dependency range.

    Merges performed with ``GITHUB_TOKEN`` do not emit a ``push`` event, so the
    automerge workflow dispatches main ci explicitly. When that dispatch failed
    the default branch has no successful run and a schedule would otherwise wait
    forever. A new run may only be requested for a range that every real
    Dependabot/policy check accepts and that has a non-empty net change; an
    ineligible or already-released commit is never re-run, and a tag is never
    created here.
    """
    if not local.has_commit(candidate):
        raise Skip("candidate commit %s is not available in the trusted checkout" % candidate[:12])
    latest = latest_release(gh)
    if latest is None:
        raise Skip("no published stable release to base the next patch version on")
    last_tag, last_commit = latest
    if not local.has_commit(last_commit):
        raise Skip("latest release %s is not available in the trusted checkout" % last_tag)
    if not local.is_ancestor(last_commit, candidate):
        raise Skip("latest release %s is not an ancestor of the candidate" % last_tag)
    ensure_release_range(gh, local, default_branch, last_commit, candidate)
    if find_active_ci_run(gh, default_branch, candidate) is not None:
        raise Skip("a ci run for %s is already active; waiting" % candidate[:12])
    gh.dispatch_ci(default_branch)
    raise Skip("dispatched the main ci run for %s; waiting for it to succeed" % candidate[:12])


# --------------------------------------------------------------------------- #
# Release range eligibility
# --------------------------------------------------------------------------- #


def latest_release(gh):
    """Return ``(tag, commit)`` for the latest public stable ``vX.Y.Z`` release."""
    best_key = None
    best_tag = None
    for release in gh.releases():
        if release["draft"] or release["prerelease"]:
            continue
        tag = release["tag"]
        if not isinstance(tag, str):
            continue
        match = TAG_RE.match(tag)
        if match is None:
            continue
        key = tuple(int(part) for part in match.groups())
        if best_key is None or key > best_key:
            best_key = key
            best_tag = tag
    if best_tag is None:
        return None
    return best_tag, gh.tag_commit(best_tag)


def bump_patch(tag):
    match = TAG_RE.match(tag)
    if match is None:
        raise Failure("latest release tag %r is not a stable vX.Y.Z version" % tag)
    return "v%d.%d.%d" % (int(match.group(1)), int(match.group(2)), int(match.group(3)) + 1)


def _pull_request_merged(pr):
    """REST ``commits/{sha}/pulls`` has no boolean ``merged`` field.

    A merged pull request is ``state == "closed"`` with a valid ISO-8601
    ``merged_at``; an unmerged closed pull request has ``merged_at: null``.
    """
    if pr.get("state") != "closed":
        return False
    merged_at = pr.get("merged_at")
    return isinstance(merged_at, str) and _ISO8601_RE.match(merged_at) is not None


def eligible_pull_request(gh, default_branch, sha):
    """Pick the unique merged same-repo Dependabot PR whose merge is ``sha``."""
    matches = []
    for pr in gh.commit_pulls(sha):
        head = pr.get("head") or {}
        base = pr.get("base") or {}
        head_ref = head.get("ref") or ""
        if not _pull_request_merged(pr):
            continue
        if pr.get("merge_commit_sha") != sha:
            continue
        if base.get("ref") != default_branch:
            continue
        if (head.get("repo") or {}).get("full_name") != gh.repo:
            continue
        if not head_ref.startswith("dependabot/"):
            continue
        if (pr.get("user") or {}).get("login") != DEPENDABOT_AUTHOR:
            continue
        matches.append(pr)
    if len(matches) != 1:
        return None, "no unique merged same-repo Dependabot pull request"
    return matches[0], ""


def eligible_commit(gh, local, default_branch, sha):
    """Return ``(ok, reason)`` for one first-parent commit on the default branch."""
    pr, reason = eligible_pull_request(gh, default_branch, sha)
    if pr is None:
        return False, reason
    head_ref = (pr.get("head") or {}).get("ref") or ""
    parent = local.parent(sha)
    files = local.changed_files(parent, sha)
    base_go_mod = local.file_at(parent, "go.mod")
    head_go_mod = local.file_at(sha, "go.mod")
    workflows = {}
    for path in files:
        if WORKFLOW_PATH_RE.match(path):
            workflows[path] = {
                "base": local.file_at(parent, path),
                "head": local.file_at(sha, path),
            }
    payload = {
        "author": DEPENDABOT_AUTHOR,
        "branch": head_ref,
        "files": files,
        "base_go_mod": base_go_mod,
        "head_go_mod": head_go_mod,
        "workflows": workflows,
    }
    # The author is asserted again by the policy; a malformed payload fails
    # closed there too.
    return dependency_policy.evaluate_update(payload)


def ensure_release_range(gh, local, default_branch, base, head):
    """Raise Skip unless every first-parent commit in ``base..head`` is a bump."""
    commits = local.first_parent_range(base, head)
    if not commits:
        raise Skip("no new commits since the last release")
    for commit in commits:
        ok, reason = eligible_commit(gh, local, default_branch, commit)
        if not ok:
            raise Skip("commit %s is not an eligible dependency update: %s" % (commit[:12], reason))
    if not local.changed_files(base, head):
        raise Skip("the dependency range has no net change against the last release")


def release_run_state(runs):
    if any(run.get("status") in ACTIVE_STATUSES for run in runs):
        return "active"
    if any(run.get("conclusion") == "success" for run in runs):
        return "done"
    if runs:
        return "failed"
    return "none"


# --------------------------------------------------------------------------- #
# Reconciliation
# --------------------------------------------------------------------------- #


def reconcile(gh, local):
    default_branch = gh.default_branch()
    run_id = os.environ.get("CI_RUN_ID", "").strip()
    expected = os.environ.get("EXPECTED_HEAD_SHA", "").strip()
    if run_id or expected:
        if not (run_id and expected):
            raise Failure("CI_RUN_ID and EXPECTED_HEAD_SHA must be provided together")
        verify_ci_run(gh, default_branch, gh.run(run_id), expected)
        candidate = expected
    else:
        # Schedule / manual dispatch: derive the current default branch head.
        # A successful full ci run is required before any release work; when it
        # is missing (e.g. the automerge main-ci dispatch failed) only an
        # eligible, unpublished dependency range may trigger a fresh run.
        candidate = gh.main_sha(default_branch)
        if find_ci_run(gh, default_branch, candidate) is None:
            attempt_ci_recovery(gh, local, default_branch, candidate)
            return

    # The candidate must still be the current default branch head, otherwise a
    # newer push (with its own ci run) owns the release.
    main_sha = gh.main_sha(default_branch)
    if main_sha != candidate:
        raise Skip("the default branch advanced to %s; waiting for its ci run" % main_sha[:12])
    if not local.has_commit(candidate):
        raise Skip("candidate commit %s is not available in the trusted checkout" % candidate[:12])

    latest = latest_release(gh)
    if latest is None:
        raise Skip("no published stable release to base the next patch version on")
    last_tag, last_commit = latest
    if not local.has_commit(last_commit):
        raise Skip("latest release %s is not available in the trusted checkout" % last_tag)
    if not local.is_ancestor(last_commit, candidate):
        raise Skip("latest release %s is not an ancestor of the candidate" % last_tag)

    next_tag = bump_patch(last_tag)
    releases = gh.releases()
    existing_release = next((r for r in releases if r["tag"] == next_tag), None)
    if existing_release is not None and not existing_release["draft"]:
        raise Skip("release %s is already published" % next_tag)
    if existing_release is not None and existing_release["author"] != AUTOMATION_AUTHOR:
        raise Skip("draft release %s is not owned by this automation" % next_tag)

    ref = gh.tag_ref(next_tag)
    if ref is not None:
        reconcile_existing_tag(
            gh, local, default_branch, candidate, last_commit, next_tag, ref, existing_release
        )
        return
    create_and_dispatch(
        gh, local, default_branch, candidate, last_commit, next_tag, existing_release
    )


def create_and_dispatch(gh, local, default_branch, candidate, last_commit, next_tag, existing_release):
    if existing_release is not None:
        raise Skip("release %s exists without a tag ref; refusing to touch it" % next_tag)
    ensure_release_range(gh, local, default_branch, last_commit, candidate)

    # Re-check the remote default branch immediately before writing; a race can
    # only skip, never bind the tag to a stale commit.
    if gh.main_sha(default_branch) != candidate:
        raise Skip("the default branch advanced before tag creation; retrying on the next run")

    if not gh.create_tag(next_tag, candidate):
        # Another writer won the race. Reconcile the tag that now exists rather
        # than creating a second one.
        ref = gh.tag_ref(next_tag)
        if ref is None:
            raise Failure("tag %s creation conflicted but no tag is present" % next_tag)
        reconcile_existing_tag(gh, local, default_branch, candidate, last_commit, next_tag, ref, None)
        return

    try:
        gh.dispatch_release(default_branch, next_tag)
    except Failure:
        raise Failure(
            "created tag %s but could not dispatch the release workflow; the same tag is retried"
            % next_tag
        )


def reconcile_existing_tag(
    gh, local, default_branch, candidate, last_commit, tag, ref, existing_release
):
    if existing_release is not None and not existing_release["draft"]:
        raise Skip("release %s is already published" % tag)
    if existing_release is not None and existing_release["author"] != AUTOMATION_AUTHOR:
        raise Skip("draft release %s is not owned by this automation" % tag)

    target = ref.get("sha")
    if ref.get("type") == "tag":
        target = gh.deref_tag(target)
    if not isinstance(target, str) or not target:
        raise Failure("could not resolve tag %s" % tag)
    if not local.has_commit(target):
        raise Skip("existing tag %s points to an unknown commit; refusing to move it" % tag)
    if not local.is_ancestor(target, candidate):
        raise Skip("existing tag %s does not point to a default-branch ancestor" % tag)

    # The tag must describe the same eligible dependency range as a fresh
    # release; a manual, mixed or reverted range is never published.
    ensure_release_range(gh, local, default_branch, last_commit, target)
    if find_ci_run(gh, default_branch, target) is None:
        raise Skip("tag commit %s has no successful main ci run yet" % target[:12])

    state = release_run_state(gh.release_runs(tag))
    if state == "active":
        raise Skip("the release workflow for %s is still running" % tag)
    if state == "done":
        raise Skip("the release workflow for %s already succeeded" % tag)
    try:
        gh.dispatch_release(default_branch, tag)
    except Failure:
        raise Failure("could not dispatch the release workflow for existing tag %s; it is retried" % tag)


# --------------------------------------------------------------------------- #
# Entry point
# --------------------------------------------------------------------------- #


def main(argv=None):
    try:
        repo = os.environ.get("GH_REPO", "").strip()
        if not repo:
            print("auto-release: GH_REPO is required", file=sys.stderr)
            return 2
        bin_path = os.environ.get("GH_BIN", "gh").strip() or "gh"
        root = os.environ.get("REPO_ROOT", os.getcwd())
        try:
            reconcile(GitHub(repo, bin_path), LocalRepo(root))
        except Skip as skip:
            print("auto-release: skip: %s" % skip)
            return 0
    except Failure as failure:
        print("auto-release: failure: %s" % failure, file=sys.stderr)
        return 1
    except Exception as exc:  # defensive: an unexpected bug fails closed
        print("auto-release: failure: %s" % exc, file=sys.stderr)
        return 1
    print("auto-release: reconciliation complete")
    return 0


if __name__ == "__main__":
    sys.exit(main())
