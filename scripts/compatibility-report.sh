#!/usr/bin/env bash
# Deduplicated failure reporting and recovery for the daily real-host
# compatibility workflow (.github/workflows/compatibility.yml).
#
# The workflow runs a reusable real-host check and then invokes this script from
# a separate `if: always()` job, passing the reusable job result through
# REPORT_RESULT so a failed check is observed instead of being swallowed. The
# reusable job itself still fails, which keeps the whole run red.
#
# On any non-success result the script opens exactly one issue (or comments on
# the existing one) owned by github-actions[bot]; on success it closes that same
# issue. Only the run URL is ever published: logs, tokens, payloads and host
# output are never read or copied into the issue.
set -euo pipefail

COMPATIBILITY_REPORT_TITLE="compatibility real-host check failed"
COMPATIBILITY_REPORT_AUTHOR="github-actions[bot]"

# find_owned_issue <gh> <repo>
# Prints the number of the open, bot-owned issue carrying the dedup title, or
# nothing when no such issue exists. Filtering by author keeps the automation
# from commenting on or closing a human-filed issue with the same title.
find_owned_issue() {
	local gh="$1" repo="$2"
	"$gh" issue list --repo "$repo" --state open \
		--author "$COMPATIBILITY_REPORT_AUTHOR" \
		--search "\"$COMPATIBILITY_REPORT_TITLE\" in:title" \
		--json number --jq '.[0].number // empty'
}

main() {
	local repo="${GH_REPO:-}" result="${REPORT_RESULT:-}" run_url="${RUN_URL:-}"
	if [[ -z "$repo" ]]; then
		echo "compatibility-report: GH_REPO is required" >&2
		return 2
	fi
	if [[ "$result" != "success" && "$result" != "failure" && "$result" != "cancelled" ]]; then
		echo "compatibility-report: REPORT_RESULT must be success, failure or cancelled" >&2
		return 2
	fi
	# A failure must always carry the run link; a recovery needs no URL.
	if [[ "$result" != "success" && -z "$run_url" ]]; then
		echo "compatibility-report: RUN_URL is required to report a failure" >&2
		return 2
	fi

	local gh="${GH_BIN:-gh}" number
	number="$(find_owned_issue "$gh" "$repo")"

	if [[ "$result" == "success" ]]; then
		# Recovery: close only our own still-open issue; a missing one is a no-op.
		if [[ -z "$number" ]]; then
			return 0
		fi
		# Propagate the status explicitly so a broken close surfaces a non-zero
		# exit even when the caller suppresses `set -e`.
		"$gh" issue close "$number" --repo "$repo" \
			--comment "compatibility real-host check recovered."
		return $?
	fi

	if [[ -z "$number" ]]; then
		"$gh" issue create --repo "$repo" \
			--title "$COMPATIBILITY_REPORT_TITLE" --body "$run_url"
	else
		"$gh" issue comment "$number" --repo "$repo" --body "$run_url"
	fi
	return $?
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
	main "$@"
fi
