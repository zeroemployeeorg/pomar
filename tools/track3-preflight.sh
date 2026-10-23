#!/usr/bin/env bash
# Track 3 (pomar-SOW-87 / zeo-rt-sched-SOW-04 / zeocore-SOW-41): a read-only
# preflight for the ENG-UK job re-running zeocore's same source revision,
# after the environment correction. This script never installs, modifies or
# executes any part of the job itself -- it only inspects the already-present
# environment and reports facts, so the scheduler's allocation can carry real
# preflight evidence instead of a plan.
#
# Council's review of 6f0f91e: this script requires bash and was not
# declaring it -- `bounded_run`'s process-group primitive (`set -m` on a
# background job) silently does NOT create a new process group under a
# plain POSIX `/bin/sh` without job-control support (dash, the real `/bin/sh`
# on most Linux hosts including the actual ENG-UK job target): verified
# directly -- `dash` without a controlling tty prints "can't access tty; job
# control turned off" and leaves the backgrounded job in the SAME process
# group as the shell itself, silently breaking every group-kill guarantee
# this script relies on. bash's `set -m` was verified to still create a
# genuinely distinct process group even with no controlling tty
# (`< /dev/null`). The shebang and invocation now require bash explicitly,
# and the script refuses to proceed under any other shell rather than run
# with a broken safety primitive and not say so.
#
# Usage: bash tools/track3-preflight.sh <zeocore-repo-path> <scratch-path> \
#          <pandoc-job-target> <expected-full-commit-sha>
#
# <zeocore-repo-path> is a local checkout of zeroemployeeorg/zeocore, read
# only (via `git show HEAD:<path>`, never the working tree) to learn its OWN
# pandoc floor (src/zeo_core/integrations/pandoc/_runner.py's
# MIN_PANDOC_VERSION), CI pin (tools/install-pandoc.sh) and Python
# requirement (pyproject.toml) -- never hardcoded here, so this script tracks
# whatever HEAD currently states, not a copy that can drift, and is not
# fooled by an uncommitted working-tree edit. <scratch-path> is the job's own
# scratch directory, checked (not created) for the store's ancestor rule: no
# ancestor directory may be writable by group or others unless it also
# carries the sticky bit (the /tmp pattern). <pandoc-job-target> is the
# council/Architect-set minimum THIS job's run requires -- it is operational
# policy, not a repo fact, so it is never read from the repo or defaulted;
# the caller must supply it explicitly
# (Architect's msg-architect-architect-pandoc-settled-2156).
# <expected-full-commit-sha> is the exact 40-hex-character commit this job
# was told to re-run; the inspected HEAD is compared against it and reported
# MATCH/MISMATCH, and MISMATCH (like anything else short of MATCH) fails the
# overall gate. This argument is REQUIRED, not optional: Council's review of
# 6f0f91e found that an omitted expected revision reported NOT-PROVIDED and
# that status silently PASSED the overall gate -- a preflight that can
# report VERIFIED without ever having been told which revision it was
# supposed to confirm is not a safe default for a job that re-runs a named,
# specific source revision. There is no longer a 3-argument form.
#
# Every external command this script runs (git, pandoc, uv, stat, hostname,
# id) is bounded: it runs in its own process group (bounded_run below) so a
# hang anywhere -- network, cache, a broken filesystem -- cannot stall this
# read-only inspection past its budget, and `uv lock --check` is additionally
# run with UV_OFFLINE=1, UV_PYTHON_DOWNLOADS=never and a private, disposable
# UV_CACHE_DIR, so this "read-only" script genuinely cannot reach the network
# or write outside its own bounds; if that cannot be guaranteed the check
# reports UNKNOWN rather than silently running unprotected. The lock check
# itself runs against a clean `git archive HEAD` snapshot, never the
# repo's own working tree -- Council's review of 6f0f91e: reporting
# source_head/floor/pin/pyproject from HEAD while validating uv.lock against
# the WORKING TREE's pyproject.toml means an uncommitted edit could make the
# lock check pass or fail for a revision this script never actually reports.
#
# The script reports an explicit overall status -- VERIFIED only if every
# gating check actually came back VERIFIED (or MATCH, for same-revision) --
# and exits 0 only in that case; any NOT-CONFIGURED, UNKNOWN or MISMATCH
# among the gating checks is a nonzero exit. This is a deliberate behavior
# change from an earlier, unconditional exit 0; any caller that wants "ran to
# completion" semantics instead of this gate must say so explicitly and the
# caller contract updated accordingly -- until then, nonzero on an unverified
# run is the only safe default for a preflight gating a real job.
#
# Output is the report followed by its own sha256 (a receipt over the report
# text itself, computed after capture, so the two can never drift).
set -eu

if [ -z "${BASH_VERSION:-}" ]; then
  echo "error: this script requires bash (its process-group termination guarantees depend on bash's set -m; a plain POSIX sh, e.g. dash, silently disables job control and breaks them). Run it as: bash tools/track3-preflight.sh ..." >&2
  exit 2
fi

LC_ALL=C
export LC_ALL

# A single, fixed top-level scratch directory for every temp file/dir this
# script creates (run_bounded's captured-output files, uv's private cache).
# This is deliberately NOT the per-call `cleanup_paths`-variable approach: a
# variable mutated inside preflight() -- which runs inside this script's own
# `report=$(preflight)` command substitution, i.e. a SUBSHELL -- never
# propagates back out (verified directly: a subshell's own variable writes
# are invisible to the parent once it returns). A fixed directory sidesteps
# that entirely, since filesystem state isn't subshell-scoped: every path
# created anywhere below, at any subshell depth, lives under this one
# directory, and this single top-level trap (confirmed to fire correctly on
# normal exit and on INT/TERM/HUP) removes all of it in one step.
scratch_tmp=$(mktemp -d)
trap 'rm -rf "$scratch_tmp" 2>/dev/null || true' EXIT INT TERM HUP

repo=${1:-}
scratch=${2:-}
job_target=${3:-}
expected_head=${4:-}
usage='usage: bash tools/track3-preflight.sh <zeocore-repo-path> <scratch-path> <pandoc-job-target> <expected-full-commit-sha>'
[ -n "$repo" ] && [ -d "$repo" ] || { echo "$usage" >&2; exit 2; }
[ -n "$scratch" ] || { echo "$usage" >&2; exit 2; }
[ -n "$job_target" ] || { echo "$usage" >&2; exit 2; }
[ -n "$expected_head" ] || { echo "$usage" >&2; exit 2; }
if ! expr "$expected_head" : '[0-9a-fA-F]\{40\}$' >/dev/null; then
  echo "$usage (expected-full-commit-sha must be exactly 40 hex characters, got '$expected_head')" >&2
  exit 2
fi

# pomar's review of pr#142 (9098c0b8, then 40e0d39): the previous bounded-wait
# sent TERM then called a plain `wait`, which is itself unbounded if the
# child traps or ignores TERM -- a child that outlives TERM can run (and
# "complete") indefinitely while the script still prints "killed". Two
# further findings from Council's subsequent review of 40e0d39 (P2): (a) only
# the direct child was ever signaled, never its own descendants (a `uv`
# subprocess tree, or any other multi-process command, could survive the
# escalation entirely); (b) rc>=128 was trusted as proof WE killed it, which
# conflates a real timeout-escalation kill with an in-budget signal/crash
# that merely happens to produce the same exit-status range.
#
# This version runs the command in its OWN process group (the `set -m` /
# background-job idiom below) so the watchdog can signal the whole group via
# a negative pid, not just the one pid it happened to fork -- verified
# directly against this exact macOS /bin/sh (not assumed): a process that
# traps and ignores TERM, with its own backgrounded grandchild, survives a
# group TERM intact and is removed, together with that grandchild, by a
# single group KILL (`kill -KILL -$pid`). After reaping, it re-checks the
# process group via `ps -g` for any residual member and reports that
# honestly rather than silently trusting the escalation. The one real,
# documented limitation: a descendant that deliberately detaches into its
# own new process group (e.g. via setsid) would not be a member of this
# group and could still survive -- this is not claimed as hostile-process
# isolation, only as "everything that stayed in this command's own group is
# accounted for."
#
# `wait` is never polled via kill -0 (a zombie still answers kill -0 until
# reaped, so that would not distinguish "still running" from "exited,
# awaiting reap"); it is called exactly once per job, and is what actually
# reaps the child and reports its true exit status. A real timed-out-killed
# outcome is only ever claimed when the watchdog's own fixed timeline
# (budget elapsed, TERM sent, grace elapsed, KILL sent) is consistent with
# what was observed -- rc>=128 alone, without that provenance, is reported as
# the honest, narrower "exited with a signal-range status before our own
# escalation would have fired" case, never glossed as "killed by this check".
#
# Council's review of 6f0f91e found two further real gaps in the ORIGINAL
# version of this function (fixed below, both via the same unconditional
# post-reap sweep): (N1) if the process GROUP LEADER itself dies quickly
# from the watchdog's TERM (because, unlike the counterexample this fix was
# originally built against, the leader does not trap/ignore TERM) while a
# background descendant of its OWN does ignore TERM and would otherwise
# survive, `wait "$pid"` returns as soon as the leader exits and the
# watchdog was being cancelled immediately afterward -- before its own
# scheduled KILL step ever ran -- so that surviving descendant was never
# actually killed. (N2) a leader that simply exits normally, well within
# budget, having left an unrelated background job of its own running, had
# that leftover neither killed nor even counted, since the residual check
# only ever ran once, informationally, with no sweep action taken.
#
# The fix is the same for both: after the leader is reaped, REGARDLESS of
# why or how quickly, unconditionally check the whole process group for any
# remaining member and, if one exists, escalate TERM then KILL against the
# group right then -- not deferred to the (by then cancelled) watchdog's own
# schedule, and not skipped just because the leader itself exited cleanly.
bounded_run() {
  budget=$1
  outfile=$2
  shift 2
  start=$(date +%s)
  set -m
  "$@" >"$outfile" 2>&1 &
  pid=$!
  set +m
  # The watchdog's own stdout/stderr must be redirected here, not inherited:
  # bounded_run is itself called inside a command substitution (`x=$(...)`),
  # which only sees EOF once every process holding its pipe's write end has
  # closed it. Redirecting to /dev/null means its own children inherit that
  # instead of the substitution's pipe.
  (
    sleep "$budget"
    kill -TERM -"$pid" 2>/dev/null || true
    sleep 5
    kill -KILL -"$pid" 2>/dev/null || true
  ) >/dev/null 2>&1 &
  watchdog=$!
  # Under `set -e`, a bare failing `wait` would abort this function (and,
  # since this runs inside a command-substitution subshell, silently
  # truncate the caller's output) before the report below is ever printed --
  # the exit-status capture must stay inside an if-condition, exempt from -e.
  escalation_window_end=$((start + budget + 5))
  if wait "$pid" 2>/dev/null; then rc=0; else rc=$?; fi
  kill "$watchdog" 2>/dev/null || true
  wait "$watchdog" 2>/dev/null || true
  end=$(date +%s)
  elapsed=$((end - start))

  # Unconditional post-reap group sweep (Council's N1/N2): whatever remains
  # in this command's process group after the leader itself is gone is
  # swept here, every time, regardless of why the leader exited or how
  # quickly. The common case (nothing left) costs nothing extra; only a
  # genuine straggler pays the short grace sleeps below.
  if command -v ps >/dev/null 2>&1; then
    if ps -o pid= -g "$pid" >/dev/null 2>&1; then
      kill -TERM -"$pid" 2>/dev/null || true
      sleep 1
      if ps -o pid= -g "$pid" >/dev/null 2>&1; then
        kill -KILL -"$pid" 2>/dev/null || true
        sleep 1
      fi
    fi
  fi
  residual=none
  if command -v ps >/dev/null 2>&1; then
    if ps -o pid= -g "$pid" >/dev/null 2>&1; then
      residual=present
    fi
  else
    residual=UNKNOWN
  fi
  # A real "we killed it" claim requires both the signal-range exit status
  # AND that our own escalation timeline had actually reached at least the
  # TERM point by the time the child exited -- a process that happens to die
  # from an unrelated signal in, say, its first second is not something this
  # check terminated, even though its exit status lands in the same range.
  if [ "$rc" -ge 128 ] && [ "$end" -ge "$((start + budget))" ]; then
    echo "TIMED_OUT_KILLED $rc $residual"
  elif [ "$elapsed" -gt "$budget" ]; then
    echo "TIMED_OUT_COMPLETED_BEFORE_TERMINATION $rc $residual"
  else
    echo "COMPLETED $rc $residual"
  fi
}

# run_bounded: the ergonomic wrapper every external command below actually
# calls. Sets BOUNDED_STATUS (OK, FAILED, TIMED_OUT_KILLED,
# TIMED_OUT_COMPLETED, or UNKNOWN), BOUNDED_RC and BOUNDED_OUTPUT (the
# command's captured combined output), and never leaves a temp file behind.
run_bounded() {
  budget=$1
  shift
  out=$(mktemp "$scratch_tmp/run.XXXXXX")
  report=$(bounded_run "$budget" "$out" "$@")
  set -- $report
  kind=${1:-UNKNOWN}
  code=${2:-}
  residual=${3:-none}
  BOUNDED_OUTPUT=$(cat "$out" 2>/dev/null || echo "")
  rm -f "$out"
  BOUNDED_RESIDUAL=$residual
  case "$kind" in
    COMPLETED)
      BOUNDED_RC=$code
      if [ "$code" = 0 ]; then BOUNDED_STATUS=OK; else BOUNDED_STATUS=FAILED; fi
      ;;
    TIMED_OUT_KILLED)
      BOUNDED_STATUS=TIMED_OUT_KILLED
      BOUNDED_RC=$code
      ;;
    TIMED_OUT_COMPLETED_BEFORE_TERMINATION)
      BOUNDED_STATUS=TIMED_OUT_COMPLETED
      BOUNDED_RC=$code
      ;;
    *)
      BOUNDED_STATUS=UNKNOWN
      BOUNDED_RC=""
      ;;
  esac
}

# is_numeric_dotted: strict ^[0-9]+(\.[0-9]+)*$ -- no leading/trailing dot,
# no empty segment, no non-digit character. Council's P2: a pandoc version
# string must be validated as this shape before it is ever handed to
# `sort -V`, or a non-numeric string can silently win or lose a comparison.
is_numeric_dotted() {
  v=$1
  case "$v" in
    '' | *[!0-9.]* | .* | *. | *..*) return 1 ;;
    *) return 0 ;;
  esac
}

overall=VERIFIED
note_not_verified() { overall=NOT-VERIFIED; }

preflight() {
  run_bounded 5 date -u +%Y-%m-%dT%H:%M:%SZ
  ts=$([ "$BOUNDED_STATUS" = OK ] && printf '%s' "$BOUNDED_OUTPUT" | tr -d '\n' || echo UNKNOWN)
  run_bounded 5 hostname
  host=$([ "$BOUNDED_STATUS" = OK ] && printf '%s' "$BOUNDED_OUTPUT" | tr -d '\n' || echo UNKNOWN)
  run_bounded 5 id -u
  uid=$([ "$BOUNDED_STATUS" = OK ] && printf '%s' "$BOUNDED_OUTPUT" | tr -d '\n' || echo UNKNOWN)
  run_bounded 5 id -un
  user=$([ "$BOUNDED_STATUS" = OK ] && printf '%s' "$BOUNDED_OUTPUT" | tr -d '\n' || echo UNKNOWN)

  echo "=== Track3 preflight (read-only) ==="
  echo "timestamp: $ts"
  echo "host: $host"
  echo "uid: $uid ($user)"
  echo "argv: $0 $repo $scratch $job_target${expected_head:+ $expected_head}"

  # --- repository root identity: refuse upward-search misattribution ---
  # Council: comparing only `git -C "$repo" rev-parse HEAD`'s success
  # tolerates $repo being an arbitrary SUBDIRECTORY of some unrelated repo
  # (git searches upward for .git) -- every fact this script reports must be
  # about the repo whose TOP LEVEL is genuinely the requested path, not
  # whatever repo git happened to find by walking up from it.
  repo_root_status=UNKNOWN
  repo_root=""
  requested_canonical=$(cd -P "$repo" 2>/dev/null && pwd -P) || requested_canonical=""
  run_bounded 5 git -C "$repo" rev-parse --show-toplevel
  if [ "$BOUNDED_STATUS" = OK ]; then
    repo_root=$(printf '%s' "$BOUNDED_OUTPUT" | tr -d '\n')
    if [ -n "$requested_canonical" ] && [ "$repo_root" = "$requested_canonical" ]; then
      repo_root_status=VERIFIED
    else
      repo_root_status=NOT-CONFIGURED
    fi
  fi
  echo "repo_root_status: $repo_root_status (requested $requested_canonical; git toplevel ${repo_root:-NOT-INSPECTED})"
  [ "$repo_root_status" = VERIFIED ] || note_not_verified

  # --- source identity of the inspected zeocore checkout ---
  source_head=NOT-INSPECTED
  source_remote=NOT-INSPECTED
  working_tree_status=UNKNOWN
  if [ "$repo_root_status" = VERIFIED ]; then
    run_bounded 5 git -C "$repo_root" rev-parse HEAD
    if [ "$BOUNDED_STATUS" = OK ]; then
      source_head=$(printf '%s' "$BOUNDED_OUTPUT" | tr -d '\n')
    fi
    run_bounded 5 git -C "$repo_root" remote get-url origin
    if [ "$BOUNDED_STATUS" = OK ]; then
      source_remote=$(printf '%s' "$BOUNDED_OUTPUT" | tr -d '\n')
    else
      source_remote=NOT-CONFIGURED
    fi
    run_bounded 5 git -C "$repo_root" status --porcelain
    if [ "$BOUNDED_STATUS" = OK ]; then
      if [ -z "$BOUNDED_OUTPUT" ]; then working_tree_status=CLEAN; else working_tree_status=DIRTY; fi
    fi
  fi
  echo "zeocore source_head: $source_head"
  echo "zeocore source_remote: $source_remote"
  echo "working_tree_status: $working_tree_status (informational -- facts below are read from HEAD, not the working tree, regardless)"

  if [ "$source_head" = NOT-INSPECTED ]; then
    same_revision_status=UNKNOWN
  elif [ "$source_head" = "$expected_head" ]; then
    same_revision_status=MATCH
  else
    same_revision_status=MISMATCH
  fi
  echo "same_revision_status: $same_revision_status (expected $expected_head)"
  [ "$same_revision_status" = MATCH ] || note_not_verified

  # --- 1. pandoc: three distinct, separately-sourced values (council's
  # settlement, msg-architect-architect-pandoc-settled-2156): the repo's
  # floor (the oldest pandoc the code itself requires), the repo's CI pin
  # (what CI/release installs), and the job target (council/Architect
  # operational policy, supplied by the caller -- never read from the repo).
  # The installed pandoc is checked against the JOB TARGET, never the CI pin.
  # All three repo-side facts are read from HEAD (`git show`), never the
  # working tree, so an uncommitted local edit cannot change what this
  # script reports.
  floor_version=UNKNOWN
  ci_pin_version=UNKNOWN
  required_python=UNKNOWN
  floor_path="src/zeo_core/integrations/pandoc/_runner.py"
  pin_path="tools/install-pandoc.sh"
  pyproject_path="pyproject.toml"
  if [ "$repo_root_status" = VERIFIED ]; then
    run_bounded 5 git -C "$repo_root" show "HEAD:$floor_path"
    if [ "$BOUNDED_STATUS" = OK ]; then
      floor_version=$(printf '%s' "$BOUNDED_OUTPUT" | sed -n 's/.*MIN_PANDOC_VERSION[^(]*(\([0-9][0-9]*\)[^0-9]*\([0-9][0-9]*\)).*/\1.\2/p' | head -n 1)
      [ -n "$floor_version" ] || floor_version=DOES-NOT-EXIST
    else
      floor_version=DOES-NOT-EXIST
    fi
    run_bounded 5 git -C "$repo_root" show "HEAD:$pin_path"
    if [ "$BOUNDED_STATUS" = OK ]; then
      ci_pin_version=$(printf '%s' "$BOUNDED_OUTPUT" | awk -F'"' '/^VERSION=/{print $2; exit}')
      [ -n "$ci_pin_version" ] || ci_pin_version=DOES-NOT-EXIST
    else
      ci_pin_version=DOES-NOT-EXIST
    fi
    run_bounded 5 git -C "$repo_root" show "HEAD:$pyproject_path"
    if [ "$BOUNDED_STATUS" = OK ]; then
      required_python=$(printf '%s' "$BOUNDED_OUTPUT" | awk -F'"' '/^requires-python/{print $2; exit}')
      [ -n "$required_python" ] || required_python=DOES-NOT-EXIST
    else
      required_python=DOES-NOT-EXIST
    fi
  fi
  echo "pandoc floor (code minimum, from HEAD:$floor_path): $floor_version"
  echo "pandoc CI pin (from HEAD:$pin_path): $ci_pin_version"
  echo "pandoc job target (council/Architect operational policy, not a repo fact): $job_target"
  echo "zeocore requires-python (from HEAD:$pyproject_path): $required_python"

  pandoc_path=$(command -v pandoc 2>/dev/null || true)
  if [ -z "$pandoc_path" ]; then
    echo "pandoc on PATH: DOES-NOT-EXIST"
    pandoc_status=DOES-NOT-EXIST
  else
    run_bounded 5 pandoc --version
    if [ "$BOUNDED_STATUS" != OK ]; then
      echo "pandoc on PATH: $pandoc_path (version check: $BOUNDED_STATUS, bounded to 5s)"
      pandoc_status=UNKNOWN
    else
      installed_version=$(printf '%s' "$BOUNDED_OUTPUT" | head -n 1 | awk '{print $2}')
      echo "pandoc on PATH: $pandoc_path"
      echo "pandoc installed_version: $installed_version"
      if ! is_numeric_dotted "$installed_version" || ! is_numeric_dotted "$job_target"; then
        pandoc_status=UNKNOWN
      else
        newest=$(printf '%s\n%s\n' "$job_target" "$installed_version" | sort -V | tail -n 1)
        if [ "$newest" = "$installed_version" ]; then
          pandoc_status=VERIFIED
        else
          pandoc_status=NOT-CONFIGURED
        fi
      fi
    fi
  fi
  satisfies=does-not-satisfy
  [ "$pandoc_status" = VERIFIED ] && satisfies=satisfies
  echo "pandoc_status: $pandoc_status (job target >= required: installed $satisfies)"
  [ "$pandoc_status" = VERIFIED ] || note_not_verified

  # --- 2. lock/project consistency (never `uv sync`, which installs).
  # `uv lock --check` proves ONLY that uv.lock is consistent with
  # pyproject.toml -- it does NOT prove the environment this job will
  # actually run in has those packages, extras or the required Python
  # version installed. It is run with UV_OFFLINE=1, UV_PYTHON_DOWNLOADS=never
  # and a private, disposable UV_CACHE_DIR, so this "read-only" check
  # genuinely cannot reach the network, trigger a Python download, or write
  # into any shared cache; if a private cache directory cannot be created,
  # the check reports UNKNOWN rather than running unprotected.
  uv_path=$(command -v uv 2>/dev/null || true)
  uv_timeout_ok=false
  lock_consistency=UNKNOWN
  lock_reason=" (uv not on PATH)"
  if [ -z "$uv_path" ]; then
    echo "uv on PATH: DOES-NOT-EXIST"
  else
    # pomar's review: TRACK3_UV_TIMEOUT must be a positive, bounded integer.
    uv_timeout=${TRACK3_UV_TIMEOUT:-30}
    case "$uv_timeout" in
      '' | *[!0-9]*)
        echo "uv on PATH: $uv_path (TRACK3_UV_TIMEOUT invalid: must be a positive integer, got '$uv_timeout')"
        lock_reason=" (TRACK3_UV_TIMEOUT invalid)"
        ;;
      *)
        if [ "$uv_timeout" -ge 1 ] && [ "$uv_timeout" -le 3600 ]; then
          uv_timeout_ok=true
        else
          echo "uv on PATH: $uv_path (TRACK3_UV_TIMEOUT invalid: must be between 1 and 3600 seconds, got $uv_timeout)"
          lock_reason=" (TRACK3_UV_TIMEOUT invalid)"
        fi
        ;;
    esac
  fi
  if [ "$uv_timeout_ok" = true ]; then
    run_bounded 10 "$uv_path" --version
    case "$BOUNDED_STATUS" in
      OK) echo "uv on PATH: $uv_path ($BOUNDED_OUTPUT)" ;;
      *) echo "uv on PATH: $uv_path (version check: $BOUNDED_STATUS, bounded to 10s)" ;;
    esac

    uv_cache_dir=$(mktemp -d "$scratch_tmp/uvcache.XXXXXX" 2>/dev/null || echo "")
    head_snapshot=$(mktemp -d "$scratch_tmp/headsnap.XXXXXX" 2>/dev/null || echo "")
    if [ -z "$uv_cache_dir" ]; then
      lock_consistency=UNKNOWN
      lock_reason=" (could not allocate a private UV_CACHE_DIR; refusing to run unprotected)"
    elif [ -z "$head_snapshot" ]; then
      lock_consistency=UNKNOWN
      lock_reason=" (could not allocate a HEAD snapshot directory; refusing to check the working tree instead)"
    else
      # Council's review of 6f0f91e: `uv lock --check` reads real files on
      # disk, so running it directly against $repo_root checks the WORKING
      # TREE's pyproject.toml/uv.lock, not the HEAD this script reports --
      # an uncommitted edit could make this pass or fail for a revision
      # that isn't the one named in source_head/same_revision_status above.
      # `git archive HEAD` extracts exactly HEAD's own tree into a throwaway
      # directory, so the lock check genuinely validates the inspected
      # revision regardless of working-tree state.
      run_bounded 10 sh -c "git -C \"\$1\" archive HEAD | tar -x -C \"\$2\"" sh "$repo_root" "$head_snapshot"
      if [ "$BOUNDED_STATUS" != OK ]; then
        lock_consistency=UNKNOWN
        lock_reason=" (could not extract a HEAD snapshot: $BOUNDED_STATUS)"
      else
        run_bounded "$uv_timeout" env \
          UV_OFFLINE=1 \
          UV_PYTHON_DOWNLOADS=never \
          "UV_CACHE_DIR=$uv_cache_dir" \
          "$uv_path" lock --check --directory "$head_snapshot"
        case "$BOUNDED_STATUS" in
          OK)
            lock_consistency=VERIFIED
            lock_reason=""
            ;;
          FAILED)
            lock_consistency=NOT-CONFIGURED
            lock_reason=""
            ;;
          TIMED_OUT_KILLED)
            lock_consistency=NOT-CONFIGURED
            lock_reason=" (timed out after ${uv_timeout}s, killed; residual process-group members: $BOUNDED_RESIDUAL)"
            ;;
          TIMED_OUT_COMPLETED)
            lock_consistency=NOT-CONFIGURED
            lock_reason=" (timed out after ${uv_timeout}s, completed on its own before termination was confirmed -- not killed by this check)"
            ;;
          *)
            lock_consistency=UNKNOWN
            lock_reason=" (bounded execution unavailable or unrecognized)"
            ;;
        esac
      fi
    fi
  fi
  if [ "$repo_root_status" != VERIFIED ]; then
    lock_consistency=UNKNOWN
    lock_reason=" (repository root not verified; refusing to resolve --directory)"
  fi
  echo "uv_lock_consistency_with_pyproject (uv lock --check against a HEAD snapshot, not the working tree; offline, no Python downloads, private cache, bounded to ${uv_timeout:-N/A}s, does not prove an installed environment): $lock_consistency$lock_reason"
  [ "$lock_consistency" = VERIFIED ] || note_not_verified

  # --- 3. scratch path ancestry: no existing ancestor writable by others
  # unless sticky. The rule is about the already-existing directory chain,
  # not the scratch leaf itself (which the job creates later, and which this
  # preflight never creates). Report the leaf's own existence separately,
  # informationally, and always check every REAL (symlink-resolved) existing
  # ancestor regardless of whether the leaf exists yet.
  leaf_exists=NO
  [ -d "$scratch" ] && leaf_exists=YES

  # Canonicalize `..` components lexically before walking up to the nearest
  # existing ancestor -- Council: a nonexistent leaf suffix containing `..`
  # (e.g. .../future/../leaf) must not be walked literally, which could
  # mis-locate the "nearest existing ancestor" relative to the path's real,
  # lexically-resolved meaning.
  lexically_canonical() {
    p=$1
    case "$p" in /*) ;; *) p="$PWD/$p" ;; esac
    oldIFS=$IFS
    IFS='/'
    set -- $p
    IFS=$oldIFS
    result=""
    for part in "$@"; do
      case "$part" in
        '' | '.') ;;
        '..') result=$(dirname "$result") ; [ "$result" = "." ] && result="" ;;
        *) result="$result/$part" ;;
      esac
    done
    [ -n "$result" ] || result="/"
    printf '%s' "$result"
  }
  scratch_lexical=$(lexically_canonical "$scratch")

  nearest="$scratch_lexical"
  while [ ! -d "$nearest" ] && [ "$nearest" != "/" ] && [ -n "$nearest" ]; do
    nearest=$(dirname "$nearest")
  done
  # Canonicalize the nearest existing ancestor (cd -P/pwd -P is the portable
  # idiom) so a symlinked ancestor, e.g. /tmp -> /private/tmp, is walked by
  # its real path, reaching /private's own permissions too, not just /tmp's.
  walk=$(cd -P "$nearest" 2>/dev/null && pwd -P) || walk="$nearest"

  scratch_status=VERIFIED
  scratch_reason=""
  while [ -n "$walk" ]; do
    run_bounded 5 stat -L -f '%Sp' "$walk"
    if [ "$BOUNDED_STATUS" = OK ]; then
      perm=$BOUNDED_OUTPUT
    else
      run_bounded 5 stat -L -c '%A' "$walk"
      if [ "$BOUNDED_STATUS" = OK ]; then perm=$BOUNDED_OUTPUT; else perm=""; fi
    fi
    # pomar's re-review of pr#142 round 2: length alone is not parse
    # validation -- match the real ls-style grammar before trusting any
    # character position. A stat that cannot run, or whose output doesn't
    # match this grammar, is UNKNOWN (we genuinely don't know), which is
    # distinct from NOT-CONFIGURED (we DO know, and it's unsafe) -- both
    # fail the ancestor closed (neither is VERIFIED), but the label now says
    # which one actually happened.
    case "$perm" in
      [-dlpscbD][-r][-w][-xsS][-r][-w][-xsS][-r][-w][-xtT]) ;;
      *)
        scratch_status=UNKNOWN
        scratch_reason="$walk: could not read permission metadata (stat unavailable or unparseable: '$perm')"
        break
        ;;
    esac
    group_write=$(printf '%s' "$perm" | cut -c6)
    other_write=$(printf '%s' "$perm" | cut -c9)
    other_exec=$(printf '%s' "$perm" | cut -c10)
    sticky=false
    case "$other_exec" in t | T) sticky=true ;; esac
    if { [ "$group_write" = w ] || [ "$other_write" = w ]; } && [ "$sticky" = false ]; then
      scratch_status=NOT-CONFIGURED
      scratch_reason="$walk is group- or others-writable without the sticky bit ($perm)"
      break
    fi
    [ "$walk" = "/" ] && break
    walk=$(dirname "$walk")
  done
  echo "scratch_path: $scratch (leaf exists: $leaf_exists)"
  echo "scratch_ancestry_status: $scratch_status${scratch_reason:+ ($scratch_reason)}"
  [ "$scratch_status" = VERIFIED ] || note_not_verified

  # --- 4. keychain-dependent test capability: explicit, never silent ---
  echo "keychain_dependent_capability: NOT-INSPECTED (the specific keychain item/skip-reason contract is zeocore's to supply per the accepted gate; this preflight does not invent one)"

  echo "overall_status: $overall"
  echo "=== end Track3 preflight ==="
}

report=$(preflight)
printf '%s\n' "$report"
hash=$(printf '%s\n' "$report" | shasum -a 256 2>/dev/null | awk '{print $1}')
[ -n "$hash" ] || hash=$(printf '%s\n' "$report" | sha256sum 2>/dev/null | awk '{print $1}')
echo "receipt_sha256: $hash"

# preflight() runs inside this command substitution's own subshell, so its
# $overall mutations never reach here directly -- the only reliable source
# of truth for the gate is the overall_status line it actually printed.
final_overall=$(printf '%s\n' "$report" | sed -n 's/^overall_status: //p')
[ "$final_overall" = VERIFIED ] || exit 1
exit 0
