#!/usr/bin/env bash
# Decides whether a completed CI run for a Dependabot pull request may be
# merged, and performs the merge when it is safe.
#
# This script is intentionally run from the trusted default branch by a
# `workflow_run` job. It never checks out or executes pull-request code; it only
# reads pull-request metadata and file contents through the GitHub API and then
# merges the exact commit that CI validated.
#
# The repository does not enable GitHub auto-merge, so the script merges
# immediately with `--match-head-commit` (GitHub still refuses the merge if the
# head moved). Merging with GITHUB_TOKEN does not emit a push event, so the
# main-branch CI is explicitly dispatched afterwards.
#
# The caller must pass EXPECTED_HEAD_SHA = the immutable
# github.event.workflow_run.head_sha of the completed CI run. If the pull request
# head has moved since that run, the script leaves the pull request open instead
# of merging an untested commit. If the workflow_run payload carried no pull
# request number, the script resolves the single open, same-repository, default-
# base pull request whose head is that commit, and otherwise skips safely.
#
# Eligibility decisions live in the shared, dependency-free policy at
# scripts/dependency_policy.py (importable as evaluate_update). It accepts two
# narrow classes of routine maintenance:
#   - Go module updates: only go.mod/go.sum changed on a dependabot/go_modules/
#     branch, with the module path, direct dependency set, CLIProxyAPI v8 pin
#     and all non-dependency directives unchanged. Dependabot may add or remove
#     `// indirect` entries (tidy churn), bump versions and raise the go/
#     toolchain directive; it may not add arbitrary directives.
#   - Official GitHub Actions updates: only existing
#     .github/workflows/*.yml|yaml changed on a dependabot/github_actions/
#     branch, and each changed workflow differs solely in version refs on uses
#     lines for the allowlisted first-party actions.
#
# The caller requires a successful full CI run (unit tests plus the minimum and
# latest real-host integration jobs) before this script runs, so ABI/schema
# compatibility is already verified for the exact head commit.
set -euo pipefail

DEPENDABOT_AUTOMERGE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DEPENDENCY_POLICY_SCRIPT="${DEPENDABOT_AUTOMERGE_DIR}/dependency_policy.py"

# guard_decision <author> <branch> <changed-files> <base-go.mod> <head-go.mod>
#                [workflows-json]
# Prints "approved" or a sanitized rejection reason and returns 0/1
# respectively. The optional sixth argument is a JSON object mapping each
# changed workflow path to {"base": ..., "head": ...} snapshots; it defaults to
# {} so the historical five-argument callers keep working.
guard_decision() {
	local author="$1" branch="$2" files="$3" base="$4" head="$5" workflows="${6:-{\}}"
	local payload
	if ! payload="$(DEP_AUTHOR="$author" DEP_BRANCH="$branch" DEP_FILES="$files" \
		DEP_BASE="$base" DEP_HEAD="$head" DEP_WORKFLOWS="$workflows" \
		python3 -c '
import json, os, sys
files = [line for line in os.environ["DEP_FILES"].split("\n") if line != ""]
try:
    workflows = json.loads(os.environ["DEP_WORKFLOWS"] or "{}")
except ValueError:
    sys.exit(2)
sys.stdout.write(json.dumps({
    "author": os.environ["DEP_AUTHOR"],
    "branch": os.environ["DEP_BRANCH"],
    "files": files,
    "base_go_mod": os.environ["DEP_BASE"],
    "head_go_mod": os.environ["DEP_HEAD"],
    "workflows": workflows,
}))
')"; then
		echo "not auto-mergeable: workflow snapshots are malformed"
		return 1
	fi
	printf '%s' "$payload" | python3 "$DEPENDENCY_POLICY_SCRIPT"
}

# raw_file_at <gh> <repo> <ref> <path>
# Prints the file content at an immutable ref, or nothing when it is missing.
# Trailing newlines are trimmed by command substitution, which is irrelevant for
# the line-oriented go.mod policy.
raw_file_at() {
	{ "$1" api "repos/$2/contents/$4?ref=$3" \
		-H 'Accept: application/vnd.github.raw' 2>/dev/null || true; }
}

# raw_file_base64 <gh> <repo> <ref> <path>
# Prints the base64 of the exact file bytes so that trailing blank lines survive
# command substitution; workflow comparison must see them.
raw_file_base64() {
	{ "$1" api "repos/$2/contents/$4?ref=$3" \
		-H 'Accept: application/vnd.github.raw' 2>/dev/null || true; } | base64 | tr -d '\n'
}

# build_workflows_json reads "path<TAB>base64-base<TAB>base64-head" records and
# prints the workflows object expected by the policy.
build_workflows_json() {
	python3 -c '
import base64, json, sys
snapshots = {}
for line in sys.stdin:
    line = line.rstrip("\n")
    if not line:
        continue
    path, base, head = line.split("\t")
    snapshots[path] = {
        "base": base64.b64decode(base).decode("utf-8", "surrogateescape"),
        "head": base64.b64decode(head).decode("utf-8", "surrogateescape"),
    }
sys.stdout.write(json.dumps(snapshots))
'
}

# resolve_pr_number <gh> <repo> <expected-head>
# Prints the number of the single open, same-repository pull request whose base
# is the default branch and whose head is the CI-tested commit. Returns non-zero
# when no unique match exists so the caller can skip safely.
resolve_pr_number() {
	local gh="$1" repo="$2" expected="$3"
	local default_branch rows count=0 found=""
	default_branch="$("$gh" api "repos/${repo}" --jq '.default_branch')" || return 1
	rows="$("$gh" api "repos/${repo}/pulls?state=open&base=${default_branch}&per_page=100" \
		--paginate --jq '.[] | [(.number|tostring), .head.sha, .head.repo.full_name, .base.ref] | @tsv')" || return 1
	local number head_sha head_repo base_ref
	while IFS=$'\t' read -r number head_sha head_repo base_ref; do
		[[ -n "$number" ]] || continue
		if [[ "$head_sha" == "$expected" && "$head_repo" == "$repo" && "$base_ref" == "$default_branch" ]]; then
			count=$((count + 1))
			found="$number"
		fi
	done <<<"$rows"
	if [[ "$count" -eq 1 ]]; then
		printf '%s' "$found"
		return 0
	fi
	return 1
}

main() {
	local repo="${GH_REPO:-}" pr="${PR_NUMBER:-}"
	if [[ -z "$repo" ]]; then
		echo "dependabot-automerge: GH_REPO is required" >&2
		return 2
	fi

	# The tested commit must be supplied by the workflow; never fall back to the
	# current pull request head, which may have advanced past the successful run.
	local expected_head="${EXPECTED_HEAD_SHA:-}"
	if [[ -z "$expected_head" ]]; then
		echo "dependabot-automerge: EXPECTED_HEAD_SHA (workflow_run.head_sha) is required" >&2
		return 2
	fi

	local gh="${GH_BIN:-gh}"

	# workflow_run payloads can carry an empty pull_requests list; resolve the
	# single open same-repository pull request at the tested head, or skip.
	if [[ -z "$pr" ]]; then
		local resolved
		if ! resolved="$(resolve_pr_number "$gh" "$repo" "$expected_head")"; then
			echo "dependabot-automerge: no unique open pull request matches the CI-tested commit; skipping"
			return 0
		fi
		pr="$resolved"
	fi

	# Reruns after the pull request was closed or merged are no-ops.
	local state
	state="$("$gh" pr view "$pr" --repo "$repo" --json state --jq '.state')"
	if [[ "$state" != "OPEN" ]]; then
		echo "dependabot-automerge: pull request ${pr} is ${state}; skipping"
		return 0
	fi

	local author branch base_sha current_head
	author="$("$gh" api "repos/${repo}/pulls/${pr}" --jq '.user.login')"
	branch="$("$gh" api "repos/${repo}/pulls/${pr}" --jq '.head.ref')"
	base_sha="$("$gh" api "repos/${repo}/pulls/${pr}" --jq '.base.sha')"
	current_head="$("$gh" api "repos/${repo}/pulls/${pr}" --jq '.head.sha')"

	# Refuse to merge an untested commit if the head advanced after the CI run.
	if [[ "$current_head" != "$expected_head" ]]; then
		echo "dependabot-automerge: pull request head moved from the CI-tested commit ${expected_head} to ${current_head}; leaving the pull request open"
		return 0
	fi

	local files
	files="$("$gh" api "repos/${repo}/pulls/${pr}/files" --paginate --jq '.[].filename')"

	# Snapshots are read from the immutable base and the CI-tested head only.
	local base head
	base="$(raw_file_at "$gh" "$repo" "$base_sha" "go.mod")"
	head="$(raw_file_at "$gh" "$repo" "$current_head" "go.mod")"

	local workflows="{}" wf_records="" wf
	while IFS= read -r wf; do
		[[ "$wf" =~ ^\.github/workflows/[^/]+\.(yml|yaml)$ ]] || continue
		wf_records+="${wf}"$'\t'"$(raw_file_base64 "$gh" "$repo" "$base_sha" "$wf")"$'\t'"$(raw_file_base64 "$gh" "$repo" "$current_head" "$wf")"$'\n'
	done <<<"$files"
	if [[ -n "$wf_records" ]]; then
		workflows="$(printf '%s' "$wf_records" | build_workflows_json)"
	fi

	local decision
	if ! decision="$(guard_decision "$author" "$branch" "$files" "$base" "$head" "$workflows")"; then
		echo "dependabot-automerge: ${decision}; leaving the pull request open for a human"
		return 0
	fi

	echo "dependabot-automerge: ${decision}; merging tested commit ${expected_head}"

	# Immediate squash merge of the exact commit CI validated. --match-head-commit
	# makes GitHub reject the merge if the head moved since the check above, so a
	# newer untested commit can never be merged.
	if ! "$gh" pr merge "$pr" --repo "$repo" --squash --match-head-commit "$expected_head"; then
		echo "dependabot-automerge: merge was refused for tested commit ${expected_head}; not dispatching CI" >&2
		return 1
	fi

	# A merge performed with GITHUB_TOKEN does not trigger the push event, so the
	# main-branch CI would never run for the merge commit. Dispatch it explicitly
	# for the fixed main branch only.
	"$gh" workflow run ci.yml --repo "$repo" --ref main
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
	main "$@"
fi
