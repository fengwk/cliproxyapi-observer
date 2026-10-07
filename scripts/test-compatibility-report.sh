#!/usr/bin/env bash
# Unit tests for scripts/compatibility-report.sh.
#
# A fake `gh` records every call so we can prove the unattended reporting
# contract: one deduplicated bot-owned issue is opened on failure, later
# failures comment on it instead of duplicating, a recovered run closes that
# issue, human-owned issues are never touched, only the run URL is published
# (never logs, tokens or payloads), and a broken `gh` call surfaces a non-zero
# exit instead of being swallowed. No network or GitHub access is required.
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck disable=SC1091 # sourced from the same directory at runtime
source "${script_dir}/compatibility-report.sh"

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

cat >"${work}/gh" <<'FAKE_GH'
#!/usr/bin/env bash
set -euo pipefail
args="$*"
printf '%s\n' "$args" >>"$FAKE_GH_LOG"
case "$args" in
issue\ list*)
	printf '%s\n' "${FAKE_ISSUE_NUMBER:-}"
	;;
issue\ create*)
	exit "${FAKE_CREATE_RC:-0}"
	;;
issue\ comment*)
	exit "${FAKE_COMMENT_RC:-0}"
	;;
issue\ close*)
	exit "${FAKE_CLOSE_RC:-0}"
	;;
*)
	echo "fake gh: unexpected args: $args" >&2
	exit 3
	;;
esac
FAKE_GH
chmod +x "${work}/gh"

failures=0
case_number=0

# run_case <description> <result> <run-url> <issue-number> <expect-rc>
#          <expect-create> <expect-comment> <expect-close>
run_case() {
	local description="$1" result="$2" run_url="$3" issue_number="$4" expect_rc="$5"
	local expect_create="$6" expect_comment="$7" expect_close="$8"
	case_number=$((case_number + 1))
	local log="${work}/gh-${case_number}.log"
	: >"$log"

	local rc=0
	(
		export GH_BIN="${work}/gh" GH_REPO="owner/repo"
		export REPORT_RESULT="$result" RUN_URL="$run_url"
		export FAKE_GH_LOG="$log" FAKE_ISSUE_NUMBER="$issue_number"
		export FAKE_CREATE_RC="${FAKE_CREATE_RC:-0}"
		export FAKE_COMMENT_RC="${FAKE_COMMENT_RC:-0}"
		export FAKE_CLOSE_RC="${FAKE_CLOSE_RC:-0}"
		main
	) >/dev/null 2>&1 || rc=$?

	local creates comments closes
	creates="$(grep -c '^issue create' "$log" || true)"
	comments="$(grep -c '^issue comment' "$log" || true)"
	closes="$(grep -c '^issue close' "$log" || true)"

	# GitHub is never touched when inputs are invalid (empty log). Otherwise the
	# dedup lookup must be open and bot-owned.
	local lookup_ok="yes"
	if [[ -s "$log" ]] && ! grep -q '^issue list .*--state open .*--author github-actions\[bot\] ' "$log"; then
		lookup_ok="no"
	fi

	# Every mutation must carry exactly the run URL as its body and nothing else.
	local body_ok="yes" line
	while IFS= read -r line; do
		[[ "$line" == issue\ create* || "$line" == issue\ comment* ]] || continue
		if [[ "$line" != *"--body ${run_url}" || "$line" == *"--body ${run_url} "* ]]; then
			body_ok="no"
		fi
	done <"$log"

	if [[ "$rc" -eq "$expect_rc" && "$creates" -eq "$expect_create" && \
		"$comments" -eq "$expect_comment" && "$closes" -eq "$expect_close" && \
		"$lookup_ok" == "yes" && "$body_ok" == "yes" ]]; then
		printf 'ok   %d - %s\n' "$case_number" "$description"
	else
		printf 'FAIL %d - %s (rc=%d want=%d create=%s/%s comment=%s/%s close=%s/%s lookup=%s body=%s)\n' \
			"$case_number" "$description" "$rc" "$expect_rc" "$creates" "$expect_create" \
			"$comments" "$expect_comment" "$closes" "$expect_close" "$lookup_ok" "$body_ok"
		failures=$((failures + 1))
	fi
}

url="https://github.com/owner/repo/actions/runs/12345"

# First failure opens exactly one issue carrying only the run URL.
run_case "failure opens one deduplicated issue with only the run URL" \
	failure "$url" '' 0 1 0 0

# A repeated failure comments on the existing issue instead of duplicating it.
run_case "repeated failure comments on the existing bot issue" \
	failure "$url" '42' 0 0 1 0

# A cancelled check is reported like a failure.
run_case "cancelled check is reported as a failure" \
	cancelled "$url" '42' 0 0 1 0

# A successful run closes the previously opened issue.
run_case "recovered run closes the bot-owned issue" \
	success '' '42' 0 0 0 1

# A successful run with no open issue is a no-op.
run_case "recovered run without an open issue is a no-op" \
	success '' '' 0 0 0 0

# Missing inputs fail closed before touching GitHub.
run_case "missing result fails closed without touching GitHub" \
	'' "$url" '42' 2 0 0 0
run_case "failure without a run URL fails closed" \
	failure '' '42' 2 0 0 0

# A broken gh call must not be swallowed: the job has to fail so the problem is
# visible instead of leaving a stale green result.
FAKE_CREATE_RC=1 run_case "a failed issue creation surfaces a non-zero exit" \
	failure "$url" '' 1 1 0 0
FAKE_CLOSE_RC=1 run_case "a failed recovery close surfaces a non-zero exit" \
	success '' '42' 1 0 0 1

# The script must never read CI logs, copy credentials or invent extra content.
src="$(cat "${script_dir}/compatibility-report.sh")"
for forbidden in 'run view' '--log' 'GH_TOKEN' 'secrets.'; do
	case_number=$((case_number + 1))
	if [[ "$src" == *"$forbidden"* ]]; then
		printf 'FAIL %d - reporting script must not reference %q\n' "$case_number" "$forbidden"
		failures=$((failures + 1))
	else
		printf 'ok   %d - reporting script does not reference %q\n' "$case_number" "$forbidden"
	fi
done

if [[ "$failures" -ne 0 ]]; then
	printf '\n%d case(s) failed\n' "$failures" >&2
	exit 1
fi

printf '\nall %d cases passed\n' "$case_number"
