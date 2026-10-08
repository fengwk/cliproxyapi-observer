#!/usr/bin/env python3
"""Shared fail-closed policy for unattended Dependabot maintenance.

This module is the single source of truth for deciding whether a pull request is
a routine dependency update that automation may merge without human review. It
is used both as a library (``evaluate_update``) and as a stdin/stdout CLI.

Two update categories are supported:

* Go modules (``go.mod`` and/or ``go.sum`` on a ``dependabot/go_modules/``
  branch): dependency version bumps that keep the module path, the direct
  dependency path set, the CLIProxyAPI v8 pin and every non-dependency
  directive intact. A monotonic ``go``/``toolchain`` bump required by a
  dependency is allowed; arbitrary new directives are not.
* Official GitHub Actions (existing ``.github/workflows/*.yml`` or ``*.yaml``
  on a ``dependabot/github_actions/`` branch): version-ref-only changes to a
  small allowlist of first-party actions, with every other byte of every
  changed workflow unchanged.

Design constraints:

* Python standard library only.
* Pull-request content is never executed and never parsed as YAML. Workflow
  snapshots are compared as raw text so a crafted workflow can neither run code
  nor hide a change behind a parser or a YAML feature (anchors, tags, ...).
* Every unexpected or malformed input fails closed with a sanitized reason.
"""

import json
import re
import sys
from typing import Any, Iterable, Mapping, Optional, Sequence, Tuple

PLUGIN_MODULE = "github.com/fengwk/cliproxyapi-observer"
CPA_MODULE = "github.com/router-for-me/CLIProxyAPI/v8"
CPA_MODULE_PREFIX = "github.com/router-for-me/CLIProxyAPI"

# Only first-party GitHub Actions may be bumped automatically. Anything else
# (including third-party actions and reusable local workflows) stays manual.
ALLOWED_ACTIONS = frozenset(
    {
        "actions/checkout",
        "actions/setup-go",
        "actions/setup-node",
        "actions/upload-artifact",
        "actions/download-artifact",
    }
)

GO_FILES = frozenset({"go.mod", "go.sum"})
GO_BRANCH_PREFIX = "dependabot/go_modules/"
ACTIONS_BRANCH_PREFIX = "dependabot/github_actions/"
WORKFLOW_PATH_RE = re.compile(r"^\.github/workflows/[^/]+\.(?:yml|yaml)$")

# A uses line: optional list dash, "uses:", an owner/repo action, a ref and an
# optional trailing comment. Everything except the ref is compared verbatim so
# comments, indentation and quoting must be byte-identical.
_USES_RE = re.compile(
    r"^(?P<prefix>\s*(?:-\s*)?uses:\s*)"
    r"(?P<action>[A-Za-z0-9_.\-]+/[A-Za-z0-9_.\-]+)"
    r"(?P<ref>@\S+)"
    r"(?P<suffix>\s*(?:#.*)?)$"
)
_ACTION_VERSION_RE = re.compile(r"^v(\d+)(?:\.(\d+))?(?:\.(\d+))?$")
_GO_VERSION_RE = re.compile(r"^\d+(?:\.\d+)*$")
_INDIRECT_RE = re.compile(r"//\s*indirect(?:\s|$)")


def _r(message: str) -> str:
    """Return a sanitized, single-line rejection reason."""
    return "not auto-mergeable: " + message


def evaluate_update(payload: dict) -> Tuple[bool, str]:
    """Decide whether a dependency-update payload may be auto-merged.

    ``payload`` follows the fixed contract: ``author``, ``branch``,
    ``files``, ``base_go_mod``, ``head_go_mod`` and ``workflows`` (a mapping of
    workflow path to ``{"base": ..., "head": ...}`` text snapshots).

    Returns ``(True, "approved")`` when the update is a routine version bump, or
    ``(False, sanitized_reason)`` when it is not. Malformed input fails closed.
    """
    try:
        return _evaluate(payload)
    except Exception:  # pragma: no cover - defensive: any bug fails closed
        return False, _r("payload could not be evaluated")


def _evaluate(payload: Any) -> Tuple[bool, str]:
    if not isinstance(payload, Mapping):
        return False, _r("payload is not a JSON object")

    author = payload.get("author")
    branch = payload.get("branch")
    files = payload.get("files")
    base_go_mod = payload.get("base_go_mod", "")
    head_go_mod = payload.get("head_go_mod", "")
    workflows = payload.get("workflows", {})

    if not isinstance(author, str) or not isinstance(branch, str):
        return False, _r("author or branch is malformed")
    if not isinstance(files, list) or not all(isinstance(f, str) for f in files):
        return False, _r("changed file list is malformed")
    if not isinstance(base_go_mod, str) or not isinstance(head_go_mod, str):
        return False, _r("go.mod snapshot is malformed")
    if not isinstance(workflows, Mapping):
        return False, _r("workflow snapshots are malformed")

    if author != "dependabot[bot]":
        return False, _r("author is not dependabot[bot]")
    if not files:
        return False, _r("no changed files reported")
    if any(not f for f in files):
        return False, _r("changed file list contains an empty path")
    if len(files) != len(set(files)):
        return False, _r("changed file list contains duplicates")

    file_set = set(files)
    is_go = file_set <= GO_FILES
    is_actions = all(WORKFLOW_PATH_RE.match(f) for f in files)

    if is_go and not is_actions:
        if not branch.startswith(GO_BRANCH_PREFIX):
            return False, _r("branch is not a dependabot go_modules branch")
        return _evaluate_go(base_go_mod, head_go_mod)
    if is_actions and not is_go:
        if not branch.startswith(ACTIONS_BRANCH_PREFIX):
            return False, _r("branch is not a dependabot github_actions branch")
        return _evaluate_workflows(file_set, workflows)
    return False, _r("changed files mix unsupported categories")


# --------------------------------------------------------------------------- #
# Go module updates
# --------------------------------------------------------------------------- #


def _parse_go_mod(text: str) -> Optional[dict]:
    """Parse the directives we care about from a go.mod, or None if malformed.

    Only the subset needed for the policy is extracted. Lines carrying any
    other directive (replace, exclude, retract, unknown) are collected verbatim
    so that any change to them is rejected.
    """
    module = ""
    go_version: Optional[str] = None
    toolchain: Optional[str] = None
    requires: list = []
    others: list = []
    in_require = False

    for raw in text.splitlines():
        line = raw.split("//", 1)[0].strip()
        if not line:
            continue
        if in_require:
            if line == ")":
                in_require = False
                continue
            if line.startswith("("):
                return None
            entry = line.split()
            if len(entry) < 2:
                return None
            requires.append((entry[0], entry[1], bool(_INDIRECT_RE.search(raw))))
            continue

        parts = line.split(None, 1)
        name = parts[0]
        value = parts[1].strip() if len(parts) > 1 else ""
        if name == "module":
            if not value:
                return None
            module = value.split()[0]
        elif name == "go":
            if not value:
                return None
            go_version = value.split()[0]
        elif name == "toolchain":
            if not value:
                return None
            toolchain = value.split()[0]
        elif name == "require":
            if value.startswith("("):
                if value[1:].strip():
                    return None
                in_require = True
            else:
                entry = value.split()
                if len(entry) < 2:
                    return None
                requires.append((entry[0], entry[1], bool(_INDIRECT_RE.search(raw))))
        else:
            others.append(" ".join(line.split()))

    if in_require:
        return None
    return {
        "module": module,
        "go": go_version,
        "toolchain": toolchain,
        "requires": requires,
        "others": others,
    }


def _parse_go_version(value: Optional[str]) -> Optional[Tuple[int, ...]]:
    if value is None:
        return None
    text = value[2:] if value.startswith("go") else value
    if not _GO_VERSION_RE.match(text):
        return None
    return tuple(int(part) for part in text.split("."))


def _compare_go_versions(a: Tuple[int, ...], b: Tuple[int, ...]) -> int:
    width = max(len(a), len(b))
    a = a + (0,) * (width - len(a))
    b = b + (0,) * (width - len(b))
    return (a > b) - (a < b)


def _check_monotonic(name: str, base_value: Optional[str], head_value: Optional[str]) -> Tuple[bool, str]:
    if base_value == head_value:
        return True, ""
    if base_value is None:
        if head_value is not None and _parse_go_version(head_value) is not None:
            return True, ""
        return False, _r("go.mod %s directive is malformed" % name)
    if head_value is None:
        return False, _r("go.mod %s directive was removed" % name)
    base_parsed = _parse_go_version(base_value)
    head_parsed = _parse_go_version(head_value)
    if base_parsed is None or head_parsed is None:
        return False, _r("go.mod %s directive is malformed" % name)
    if _compare_go_versions(head_parsed, base_parsed) < 0:
        return False, _r("go.mod %s directive was downgraded" % name)
    return True, ""


def _is_v8_version(version: str) -> bool:
    return version.startswith("v8.") or version == "v8"


def _evaluate_go(base_go_mod: str, head_go_mod: str) -> Tuple[bool, str]:
    if not base_go_mod or not head_go_mod:
        return False, _r("go.mod snapshot is missing")
    base = _parse_go_mod(base_go_mod)
    head = _parse_go_mod(head_go_mod)
    if base is None or head is None:
        return False, _r("go.mod could not be parsed")

    if base["module"] != head["module"]:
        return False, _r("go.mod module path changed")
    if head["module"] != PLUGIN_MODULE:
        return False, _r("go.mod module path is not the plugin module")

    base_direct = {path for path, _version, indirect in base["requires"] if not indirect}
    head_direct = {path for path, _version, indirect in head["requires"] if not indirect}
    if base_direct != head_direct:
        return False, _r("go.mod direct dependency set changed")

    cpa_versions = [version for path, version, _i in head["requires"] if path == CPA_MODULE]
    if not cpa_versions:
        return False, _r("go.mod is missing the CLIProxyAPI v8 dependency")
    for version in cpa_versions:
        if not _is_v8_version(version):
            return False, _r("CLIProxyAPI is not pinned to a v8 version")
    for path, _version, _i in head["requires"]:
        if path.startswith(CPA_MODULE_PREFIX) and path != CPA_MODULE:
            return False, _r("go.mod references a non-v8 CLIProxyAPI module")

    if base["others"] != head["others"]:
        return False, _r("go.mod replace/exclude or other directives changed")

    for name in ("go", "toolchain"):
        ok, reason = _check_monotonic(name, base[name], head[name])
        if not ok:
            return False, reason

    return True, "approved"


# --------------------------------------------------------------------------- #
# Official GitHub Actions updates
# --------------------------------------------------------------------------- #


def _parse_action_version(ref: str) -> Optional[Tuple[int, int, int]]:
    if ref.startswith("@"):
        ref = ref[1:]
    match = _ACTION_VERSION_RE.match(ref)
    if match is None:
        return None
    return tuple(int(part) for part in match.groups(default="0"))  # type: ignore[return-value]


def _compare_workflow_text(base: str, head: str) -> Tuple[bool, str, bool]:
    """Compare two workflow snapshots line by line.

    Returns ``(ok, reason, changed)``. A change is acceptable only when a line
    differs solely in the version ref of an allowlisted action and that version
    strictly increases. Every other byte, line count, comment, indentation and
    action repository must be identical.
    """
    base_lines = base.split("\n")
    head_lines = head.split("\n")
    if len(base_lines) != len(head_lines):
        return False, _r("workflow structure changed"), False

    changed = False
    for base_line, head_line in zip(base_lines, head_lines):
        if base_line == head_line:
            continue
        base_match = _USES_RE.match(base_line)
        head_match = _USES_RE.match(head_line)
        if base_match is None or head_match is None:
            return False, _r("workflow changed outside an action version reference"), False
        if base_match.group("action") != head_match.group("action"):
            return False, _r("workflow action repository changed"), False
        if base_match.group("prefix") != head_match.group("prefix") or base_match.group(
            "suffix"
        ) != head_match.group("suffix"):
            return False, _r("workflow content changed outside the action version reference"), False
        action = base_match.group("action")
        if action not in ALLOWED_ACTIONS:
            return False, _r("workflow uses an action outside the allowlist"), False
        base_version = _parse_action_version(base_match.group("ref"))
        head_version = _parse_action_version(head_match.group("ref"))
        if base_version is None or head_version is None:
            return False, _r("workflow action reference is not a numeric version tag"), False
        if head_version <= base_version:
            return False, _r("workflow action version did not increase"), False
        changed = True
    return True, "approved", changed


def _evaluate_workflows(files: Iterable[str], workflows: Mapping) -> Tuple[bool, str]:
    file_set = set(files)
    snapshot_paths = set(workflows.keys())
    if snapshot_paths != file_set:
        return False, _r("workflow snapshots do not match the changed files")

    changed = False
    for path in sorted(files):
        snapshot = workflows[path]
        if not isinstance(snapshot, Mapping) or set(snapshot.keys()) != {"base", "head"}:
            return False, _r("workflow snapshot for a changed file is malformed")
        base = snapshot["base"]
        head = snapshot["head"]
        if not isinstance(base, str) or not isinstance(head, str) or not base or not head:
            return False, _r("workflow snapshot for a changed file is missing")
        if base == head:
            return False, _r("changed workflow is identical to its base snapshot")
        ok, reason, file_changed = _compare_workflow_text(base, head)
        if not ok:
            return False, reason
        changed = changed or file_changed

    if not changed:
        return False, _r("no workflow action version change was detected")
    return True, "approved"


# --------------------------------------------------------------------------- #
# CLI
# --------------------------------------------------------------------------- #


def main(argv: Optional[Sequence[str]] = None) -> int:
    """Read a JSON payload from stdin, print the decision and return 0/1."""
    try:
        raw = sys.stdin.read()
    except Exception:
        print(_r("payload could not be read"))
        return 1
    try:
        payload = json.loads(raw)
    except Exception:
        print(_r("payload is not valid JSON"))
        return 1
    approved, reason = evaluate_update(payload)
    print(reason)
    return 0 if approved else 1


if __name__ == "__main__":
    sys.exit(main())
