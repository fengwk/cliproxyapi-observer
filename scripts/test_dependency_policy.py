#!/usr/bin/env python3
"""Standard-library unit tests for scripts/dependency_policy.py.

These tests exercise the shared policy directly (no network) and also run the
CLI contract: JSON on stdin, a single decision line on stdout and an exit code
of 0 (approved) or 1 (rejected or malformed).
"""

import json
import os
import subprocess
import sys
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
# Keep the working tree clean: importing the policy must not leave bytecode.
sys.dont_write_bytecode = True

import dependency_policy as policy  # noqa: E402

SCRIPT = os.path.join(os.path.dirname(os.path.abspath(__file__)), "dependency_policy.py")

BRANCH_GO = "dependabot/go_modules/github.com/router-for-me/CLIProxyAPI/v8-abc"
BRANCH_ACTIONS = "dependabot/github_actions/actions/checkout-abc"


BASE_GO_MOD = """module github.com/fengwk/cliproxyapi-observer

go 1.26.0

require (
	go.etcd.io/bbolt v1.4.3
\tgithub.com/router-for-me/CLIProxyAPI/v8 v8.0.16
\tgithub.com/tidwall/gjson v1.19.1
\tgithub.com/tidwall/sjson v1.2.5
\tgopkg.in/yaml.v3 v3.0.1
)
"""

BASE_WORKFLOW = """name: ci

on:
  push:
    branches: [main]

permissions:
  contents: read

jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod
      - name: Test
        run: go test ./...
"""


def go_payload(
    base=BASE_GO_MOD,
    head=BASE_GO_MOD,
    author="dependabot[bot]",
    branch=BRANCH_GO,
    files=("go.mod", "go.sum"),
):
    return {
        "author": author,
        "branch": branch,
        "files": list(files),
        "base_go_mod": base,
        "head_go_mod": head,
        "workflows": {},
    }


def workflow_payload(
    base=BASE_WORKFLOW,
    head=BASE_WORKFLOW,
    path=".github/workflows/ci.yml",
    files=None,
    workflows=None,
    author="dependabot[bot]",
    branch=BRANCH_ACTIONS,
):
    if files is None:
        files = [path]
    if workflows is None:
        workflows = {path: {"base": base, "head": head}}
    return {
        "author": author,
        "branch": branch,
        "files": list(files),
        "base_go_mod": "",
        "head_go_mod": "",
        "workflows": workflows,
    }


class GoPolicyTest(unittest.TestCase):
    def assertApproved(self, payload):
        approved, reason = policy.evaluate_update(payload)
        self.assertTrue(approved, reason)
        self.assertEqual(reason, "approved")

    def assertRejected(self, payload):
        approved, reason = policy.evaluate_update(payload)
        self.assertFalse(approved, "expected rejection but was approved")
        self.assertTrue(reason.startswith("not auto-mergeable:"), reason)
        self.assertNotIn("\n", reason)
        self.assertEqual(len(reason.splitlines()), 1)

    # --- positives ---------------------------------------------------------

    def test_cpa_v8_bump_is_approved(self):
        head = BASE_GO_MOD.replace("v8.0.16", "v8.0.17")
        self.assertApproved(go_payload(head=head))

    def test_direct_dependency_bump_is_approved(self):
        head = BASE_GO_MOD.replace("github.com/tidwall/gjson v1.19.1", "github.com/tidwall/gjson v1.19.2")
        self.assertApproved(go_payload(head=head))

    def test_go_sum_only_update_is_approved(self):
        self.assertApproved(go_payload(files=("go.sum",)))

    def test_indirect_churn_is_approved(self):
        head = BASE_GO_MOD + "\nrequire github.com/example/transitive v1.2.3 // indirect\n"
        self.assertApproved(go_payload(head=head))

    def test_monotonic_go_directive_bump_is_approved(self):
        head = BASE_GO_MOD.replace("go 1.26.0", "go 1.26.1")
        self.assertApproved(go_payload(head=head))

    def test_go_directive_equivalence_is_approved(self):
        # 1.26 == 1.26.0; normalising widths must not look like a downgrade.
        head = BASE_GO_MOD.replace("go 1.26.0", "go 1.26")
        self.assertApproved(go_payload(head=head))

    def test_new_toolchain_directive_is_approved(self):
        head = BASE_GO_MOD.replace("go 1.26.0", "go 1.26.1\n\ntoolchain go1.26.1")
        self.assertApproved(go_payload(head=head))

    def test_unchanged_replace_directive_is_approved(self):
        base = BASE_GO_MOD + "\nreplace github.com/tidwall/gjson => github.com/tidwall/gjson v1.19.1\n"
        head = base.replace("v8.0.16", "v8.0.17")
        self.assertApproved(go_payload(base=base, head=head))

    # --- negatives ---------------------------------------------------------

    def test_non_dependabot_author_is_rejected(self):
        self.assertRejected(go_payload(author="octocat"))

    def test_non_dependabot_branch_is_rejected(self):
        self.assertRejected(go_payload(branch="feature/manual-bump"))

    def test_wrong_dependabot_ecosystem_branch_is_rejected(self):
        self.assertRejected(go_payload(branch=BRANCH_ACTIONS))

    def test_unexpected_file_is_rejected(self):
        self.assertRejected(go_payload(files=("go.mod", "go.sum", "README.md")))

    def test_empty_file_list_is_rejected(self):
        self.assertRejected(go_payload(files=()))

    def test_duplicate_files_are_rejected(self):
        self.assertRejected(go_payload(files=("go.mod", "go.mod")))

    def test_mixed_categories_are_rejected(self):
        self.assertRejected(go_payload(files=("go.mod", ".github/workflows/ci.yml")))

    def test_module_path_change_is_rejected(self):
        head = BASE_GO_MOD.replace(
            "module github.com/fengwk/cliproxyapi-observer",
            "module github.com/fengwk/other-provider",
        )
        self.assertRejected(go_payload(head=head))

    def test_new_direct_dependency_is_rejected(self):
        head = BASE_GO_MOD.replace(
            "\tgithub.com/tidwall/gjson v1.19.1",
            "\tgithub.com/example/new-dependency v1.0.0\n\tgithub.com/tidwall/gjson v1.19.1",
        )
        self.assertRejected(go_payload(head=head))

    def test_dropped_direct_dependency_is_rejected(self):
        head = BASE_GO_MOD.replace("\tgithub.com/tidwall/gjson v1.19.1\n", "")
        self.assertRejected(go_payload(head=head))

    def test_cpa_v9_module_path_is_rejected(self):
        head = BASE_GO_MOD.replace(
            "github.com/router-for-me/CLIProxyAPI/v8 v8.0.16",
            "github.com/router-for-me/CLIProxyAPI/v9 v9.0.0",
        )
        self.assertRejected(go_payload(head=head))

    def test_cpa_v9_version_on_v8_path_is_rejected(self):
        head = BASE_GO_MOD.replace("v8.0.16", "v9.0.0")
        self.assertRejected(go_payload(head=head))

    def test_non_v8_cpa_module_reference_is_rejected(self):
        head = BASE_GO_MOD + "\nrequire github.com/router-for-me/CLIProxyAPI/v9 v9.0.0 // indirect\n"
        self.assertRejected(go_payload(head=head))

    def test_added_replace_directive_is_rejected(self):
        head = BASE_GO_MOD + "\nreplace github.com/router-for-me/CLIProxyAPI/v8 => ../local-fork\n"
        self.assertRejected(go_payload(head=head))

    def test_changed_replace_directive_is_rejected(self):
        base = BASE_GO_MOD + "\nreplace github.com/tidwall/gjson => github.com/tidwall/gjson v1.19.1\n"
        head = base.replace("=> github.com/tidwall/gjson v1.19.1", "=> github.com/tidwall/gjson v1.19.2")
        self.assertRejected(go_payload(base=base, head=head))

    def test_added_exclude_directive_is_rejected(self):
        head = BASE_GO_MOD + "\nexclude github.com/tidwall/gjson v1.19.1\n"
        self.assertRejected(go_payload(head=head))

    def test_unknown_new_directive_is_rejected(self):
        head = BASE_GO_MOD + "\nsurprise github.com/example v1.0.0\n"
        self.assertRejected(go_payload(head=head))

    def test_go_directive_downgrade_is_rejected(self):
        head = BASE_GO_MOD.replace("go 1.26.0", "go 1.25.0")
        self.assertRejected(go_payload(head=head))

    def test_go_directive_removal_is_rejected(self):
        head = BASE_GO_MOD.replace("go 1.26.0\n\n", "")
        self.assertRejected(go_payload(head=head))

    def test_malformed_go_directive_is_rejected(self):
        head = BASE_GO_MOD.replace("go 1.26.0", "go latest")
        self.assertRejected(go_payload(head=head))

    def test_missing_base_go_mod_snapshot_is_rejected(self):
        self.assertRejected(go_payload(base=""))

    def test_missing_head_go_mod_snapshot_is_rejected(self):
        self.assertRejected(go_payload(head=""))

    def test_malformed_go_mod_is_rejected(self):
        self.assertRejected(go_payload(head="module\n\nrequire (\n"))

    def test_toolchain_downgrade_is_rejected(self):
        base = BASE_GO_MOD.replace("go 1.26.0", "go 1.26.0\ntoolchain go1.26.2")
        head = base.replace("toolchain go1.26.2", "toolchain go1.26.1")
        self.assertRejected(go_payload(base=base, head=head))


class ActionsPolicyTest(unittest.TestCase):
    def assertApproved(self, payload):
        approved, reason = policy.evaluate_update(payload)
        self.assertTrue(approved, reason)
        self.assertEqual(reason, "approved")

    def assertRejected(self, payload):
        approved, reason = policy.evaluate_update(payload)
        self.assertFalse(approved, "expected rejection but was approved")
        self.assertTrue(reason.startswith("not auto-mergeable:"), reason)
        self.assertNotIn("\n", reason)

    def bump(self, base_line, head_line):
        return workflow_payload(
            base=BASE_WORKFLOW,
            head=BASE_WORKFLOW.replace(base_line, head_line),
        )

    # --- positives ---------------------------------------------------------

    def test_official_action_version_bump_is_approved(self):
        self.assertApproved(self.bump("actions/checkout@v4", "actions/checkout@v4.1.0"))

    def test_patch_bump_is_approved(self):
        self.assertApproved(self.bump("actions/setup-go@v5", "actions/setup-go@v5.1.0"))

    def test_major_style_bump_is_approved(self):
        self.assertApproved(self.bump("actions/setup-go@v5", "actions/setup-go@v6"))

    def test_multiple_action_bumps_are_approved(self):
        head = BASE_WORKFLOW.replace("actions/checkout@v4", "actions/checkout@v4.2.0").replace(
            "actions/setup-go@v5", "actions/setup-go@v5.1.0"
        )
        self.assertApproved(workflow_payload(head=head))

    def test_download_artifact_bump_is_approved(self):
        base = BASE_WORKFLOW + "      - uses: actions/download-artifact@v4\n"
        head = base.replace("actions/download-artifact@v4", "actions/download-artifact@v4.1.8")
        self.assertApproved(workflow_payload(base=base, head=head))

    def test_upload_artifact_bump_is_approved(self):
        base = BASE_WORKFLOW + "      - uses: actions/upload-artifact@v4\n"
        head = base.replace("actions/upload-artifact@v4", "actions/upload-artifact@v4.5.0")
        self.assertApproved(workflow_payload(base=base, head=head))

    def test_multiple_workflow_files_are_approved(self):
        other = "name: other\njobs:\n  x:\n    steps:\n      - uses: actions/checkout@v4\n"
        other_head = other.replace("actions/checkout@v4", "actions/checkout@v4.1.1")
        payload = workflow_payload(
            files=[".github/workflows/ci.yml", ".github/workflows/other.yml"],
            workflows={
                ".github/workflows/ci.yml": {
                    "base": BASE_WORKFLOW,
                    "head": BASE_WORKFLOW.replace("actions/checkout@v4", "actions/checkout@v4.1.0"),
                },
                ".github/workflows/other.yml": {"base": other, "head": other_head},
            },
        )
        self.assertApproved(payload)

    def test_unchanged_third_party_action_is_ignored(self):
        base = BASE_WORKFLOW + "      - uses: third/party@v1\n"
        head = base.replace("actions/checkout@v4", "actions/checkout@v4.1.0")
        self.assertApproved(workflow_payload(base=base, head=head))

    # --- negatives: non-version changes -----------------------------------

    def test_permission_change_is_rejected(self):
        head = BASE_WORKFLOW.replace("  contents: read", "  contents: write")
        self.assertRejected(workflow_payload(head=head))

    def test_run_command_change_is_rejected(self):
        head = BASE_WORKFLOW.replace("run: go test ./...", "run: go test -race ./...")
        self.assertRejected(workflow_payload(head=head))

    def test_action_repository_change_is_rejected(self):
        self.assertRejected(self.bump("actions/checkout@v4", "actions/setup-go@v4.1.0"))

    def test_comment_on_uses_line_change_is_rejected(self):
        base = BASE_WORKFLOW.replace("actions/checkout@v4", "actions/checkout@v4 # pinned")
        head = base.replace("actions/checkout@v4 # pinned", "actions/checkout@v4.1.0 # updated")
        self.assertRejected(workflow_payload(base=base, head=head))

    def test_comment_only_line_change_is_rejected(self):
        base = BASE_WORKFLOW + "# keep\n"
        head = base.replace("# keep", "# changed")
        self.assertRejected(workflow_payload(base=base, head=head))

    def test_added_line_is_rejected(self):
        head = BASE_WORKFLOW + "      - uses: actions/checkout@v4.1.0\n"
        self.assertRejected(workflow_payload(head=head))

    def test_removed_line_is_rejected(self):
        head = BASE_WORKFLOW.replace("      - uses: actions/checkout@v4\n", "")
        self.assertRejected(workflow_payload(head=head))

    def test_indentation_change_is_rejected(self):
        head = BASE_WORKFLOW.replace(
            "      - uses: actions/checkout@v4", "    - uses: actions/checkout@v4.1.0"
        )
        self.assertRejected(workflow_payload(head=head))

    def test_whitespace_only_change_is_rejected(self):
        head = BASE_WORKFLOW.replace("    runs-on: ubuntu-latest", "    runs-on: ubuntu-latest ")
        self.assertRejected(workflow_payload(head=head))

    # --- negatives: version references ------------------------------------

    def test_downgrade_is_rejected(self):
        self.assertRejected(self.bump("actions/checkout@v4", "actions/checkout@v3"))

    def test_equivalent_version_is_rejected(self):
        self.assertRejected(self.bump("actions/checkout@v4", "actions/checkout@v4.0.0"))

    def test_third_party_action_bump_is_rejected(self):
        base = BASE_WORKFLOW + "      - uses: third/party@v1\n"
        head = base.replace("third/party@v1", "third/party@v2")
        self.assertRejected(workflow_payload(base=base, head=head))

    def test_sha_reference_is_rejected(self):
        self.assertRejected(
            self.bump(
                "actions/checkout@v4",
                "actions/checkout@8f4b7f84864484a7bf31766abe9204da3cbe65b3",
            )
        )

    def test_branch_reference_is_rejected(self):
        self.assertRejected(self.bump("actions/checkout@v4", "actions/checkout@main"))

    def test_malformed_four_part_version_is_rejected(self):
        self.assertRejected(self.bump("actions/checkout@v4", "actions/checkout@v4.1.0.1"))

    def test_prerelease_version_is_rejected(self):
        self.assertRejected(self.bump("actions/checkout@v4", "actions/checkout@v4.1.0-beta"))

    def test_unquoted_ref_with_trailing_junk_is_rejected(self):
        self.assertRejected(self.bump("actions/checkout@v4", "actions/checkout@v5 extra"))

    # --- negatives: snapshots ---------------------------------------------

    def test_missing_snapshot_is_rejected(self):
        payload = workflow_payload(workflows={})
        self.assertRejected(payload)

    def test_extra_snapshot_is_rejected(self):
        payload = workflow_payload(
            workflows={
                ".github/workflows/ci.yml": {
                    "base": BASE_WORKFLOW,
                    "head": BASE_WORKFLOW.replace("actions/checkout@v4", "actions/checkout@v4.1.0"),
                },
                ".github/workflows/extra.yml": {"base": "a\n", "head": "b\n"},
            }
        )
        self.assertRejected(payload)

    def test_empty_snapshot_is_rejected(self):
        payload = workflow_payload(
            workflows={".github/workflows/ci.yml": {"base": "", "head": BASE_WORKFLOW}}
        )
        self.assertRejected(payload)

    def test_malformed_snapshot_is_rejected(self):
        payload = workflow_payload(workflows={".github/workflows/ci.yml": {"base": BASE_WORKFLOW}})
        self.assertRejected(payload)

    def test_identical_snapshot_is_rejected(self):
        self.assertRejected(workflow_payload())

    def test_added_trailing_blank_line_is_rejected(self):
        # A hidden trailing newline must not hide behind a version bump.
        head = BASE_WORKFLOW.replace("actions/checkout@v4", "actions/checkout@v4.1.0") + "\n"
        self.assertRejected(workflow_payload(head=head))

    def test_removed_trailing_newline_is_rejected(self):
        head = BASE_WORKFLOW.replace("actions/checkout@v4", "actions/checkout@v4.1.0").rstrip("\n")
        self.assertRejected(workflow_payload(head=head))

    def test_crlf_change_is_rejected(self):
        head = BASE_WORKFLOW.replace("actions/checkout@v4", "actions/checkout@v4.1.0").replace("\n", "\r\n")
        self.assertRejected(workflow_payload(head=head))

    def test_workflow_file_on_go_branch_is_rejected(self):
        payload = workflow_payload(branch=BRANCH_GO)
        self.assertRejected(payload)

    def test_go_file_on_actions_branch_is_rejected(self):
        payload = go_payload(branch=BRANCH_ACTIONS)
        self.assertRejected(payload)


class MalformedPayloadTest(unittest.TestCase):
    def assertRejected(self, payload):
        approved, reason = policy.evaluate_update(payload)
        self.assertFalse(approved, "expected rejection but was approved")
        self.assertTrue(reason.startswith("not auto-mergeable:"), reason)
        self.assertNotIn("\n", reason)

    def test_non_object_payload_is_rejected(self):
        self.assertRejected(["not", "an", "object"])

    def test_non_string_author_is_rejected(self):
        payload = go_payload()
        payload["author"] = 7
        self.assertRejected(payload)

    def test_non_list_files_is_rejected(self):
        payload = go_payload()
        payload["files"] = "go.mod"
        self.assertRejected(payload)

    def test_non_string_file_is_rejected(self):
        payload = go_payload()
        payload["files"] = ["go.mod", 3]
        self.assertRejected(payload)

    def test_non_string_go_mod_is_rejected(self):
        payload = go_payload()
        payload["base_go_mod"] = ["not", "a", "string"]
        self.assertRejected(payload)

    def test_non_mapping_workflows_is_rejected(self):
        payload = workflow_payload()
        payload["workflows"] = ["not", "a", "mapping"]
        self.assertRejected(payload)

    def test_missing_keys_fail_closed(self):
        self.assertRejected({"author": "dependabot[bot]"})


class CliContractTest(unittest.TestCase):
    def run_cli(self, stdin_text):
        return subprocess.run(
            [sys.executable, SCRIPT],
            input=stdin_text,
            capture_output=True,
            text=True,
        )

    def test_approved_payload_prints_approved_and_exits_zero(self):
        result = self.run_cli(json.dumps(go_payload()))
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout.strip(), "approved")

    def test_rejected_payload_exits_one_with_reason(self):
        result = self.run_cli(json.dumps(go_payload(author="octocat")))
        self.assertEqual(result.returncode, 1)
        self.assertTrue(result.stdout.strip().startswith("not auto-mergeable:"))

    def test_invalid_json_fails_closed(self):
        result = self.run_cli("{not json")
        self.assertEqual(result.returncode, 1)
        self.assertTrue(result.stdout.strip().startswith("not auto-mergeable:"))

    def test_non_object_json_fails_closed(self):
        result = self.run_cli(json.dumps([1, 2, 3]))
        self.assertEqual(result.returncode, 1)
        self.assertTrue(result.stdout.strip().startswith("not auto-mergeable:"))


if __name__ == "__main__":
    unittest.main(verbosity=2)
