#!/usr/bin/env bash
# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# migrate-region.sh is destructive, so its execution path is exercised inside a
# throwaway SPOTDEMO_ROOT: a copy of the script and lib/, recording stubs for
# the numbered steps, and gcloud/kubectl/go replaced by recording shims. Those
# three are the only external commands the script (or anything it invokes here)
# can reach, and PATH is reduced to the shim directory plus /usr/bin:/bin, so no
# real cloud API is reachable from this test.
set -euo pipefail
root="$(cd "$(dirname "$0")/../.." && pwd)"
script="${root}/infra/migrate-region.sh"
fail=0

pass() { printf 'ok   %s\n' "$1"; }
flunk() {
  printf 'FAIL %s\n' "$1"
  fail=1
}

sandbox="$(mktemp -d "${TMPDIR:-/tmp}/migrate-region-test.XXXXXX")"
trap 'rm -rf "${sandbox}"' EXIT
mkdir -p "${sandbox}/infra/lib" "${sandbox}/out" "${sandbox}/advisor" "${sandbox}/bin"
cp "${script}" "${sandbox}/infra/migrate-region.sh"
cp "${root}/infra/lib/common.sh" "${sandbox}/infra/lib/common.sh"
# The real advisor.yaml, so the profile set the script discovers is the profile
# set the repo actually ships.
cp "${root}/advisor.yaml" "${sandbox}/advisor.yaml"
sandboxed="${sandbox}/infra/migrate-region.sh"
log="${sandbox}/calls.log"
: >"${log}"

# The numbered steps are stubbed: migrate-region.sh's contract is that it runs
# them, not what they do. 08-reconciler.sh is deliberately left out so the
# "not present" branch stays exercised.
cat >"${sandbox}/step-stub" <<'STUB'
#!/usr/bin/env bash
printf 'step %s\n' "$(basename "$0")" >>"${SHIM_LOG}"
STUB
chmod +x "${sandbox}/step-stub"
for step in 01-cluster.sh 02-computeclasses.sh 05-keda.sh 07-kueue.sh; do
  cp "${sandbox}/step-stub" "${sandbox}/infra/${step}"
done

# SHIM_GCLOUD_MISSING flips `container clusters describe` to "not found" so the
# absent-cluster branch of the teardown is reachable from the test.
cat >"${sandbox}/bin/gcloud" <<'SHIM'
#!/usr/bin/env bash
printf 'gcloud %s\n' "$*" >>"${SHIM_LOG}"
if [[ "${SHIM_GCLOUD_MISSING:-0}" == 1 && "$*" == *"clusters describe"* ]]; then
  exit 1
fi
SHIM
chmod +x "${sandbox}/bin/gcloud"

cat >"${sandbox}/bin/kubectl" <<'SHIM'
#!/usr/bin/env bash
printf 'kubectl %s\n' "$*" >>"${SHIM_LOG}"
SHIM
chmod +x "${sandbox}/bin/kubectl"

# Stand-in for `go run ./cmd/capacity-advisor analyze ...`: records the argv and
# reproduces the two advisor side effects the migration consumes — a per-profile
# cluster-config env and a per-kind ComputeClass, both pinned to the requested
# region and written under --out (resolved against the advisor dir, as the real
# binary resolves it). Dropping --profile, --regions, --out or --render
# therefore breaks the migration, exactly as it would against the real advisor.
cat >"${sandbox}/bin/go" <<'SHIM'
#!/usr/bin/env bash
set -euo pipefail
printf 'go %s\n' "$*" >>"${SHIM_LOG}"
profile=""
regions=""
out=""
render=0
while (($#)); do
  case "$1" in
    --profile)
      profile="${2:-}"
      shift 2
      ;;
    --regions)
      regions="${2:-}"
      shift 2
      ;;
    --out)
      out="${2:-}"
      shift 2
      ;;
    --render)
      render=1
      shift
      ;;
    *) shift ;;
  esac
done
[[ -n "${profile}" && -n "${regions}" && -n "${out}" && "${render}" == 1 ]] || exit 1
[[ -d "${out}" ]] || exit 1
printf 'PROJECT=test-project\nREGION=%s\nZONES=%s-a,%s-b\n' \
  "${regions}" "${regions}" "${regions}" >"${out}/cluster-config-${profile}.env"
# advisor.yaml's profiles are named <kind>-batch and the advisor names the
# ComputeClass file after the kind, so the leading segment is the kind here.
printf 'nodePoolAutoCreation:\n  zones: [%s-a]\n' "${regions}" \
  >"${out}/computeclass-${profile%%-*}.yaml"
SHIM
chmod +x "${sandbox}/bin/go"

printf 'PROJECT=test-project\nREGION=us-south1\nZONES=us-south1-b\n' \
  >"${sandbox}/out/cluster-config.env"
# A GPU ComputeClass left over from the pre-migration region. Nothing in a
# cpu-batch migration would regenerate it if the advisor only ran once, and
# 02-computeclasses.sh applies it verbatim to the new cluster.
printf 'nodePoolAutoCreation:\n  zones: [us-south1-a]\n' \
  >"${sandbox}/out/computeclass-gpu.yaml"

printf 'y\n' >"${sandbox}/yes.in"

# Every invocation below runs with the shims first on a trimmed PATH. stdin is
# /dev/null unless a case opts in: a destructive script that regresses into
# prompting must fail this suite, never block it.
run_stdin=/dev/null
shim_gcloud_missing=0
run() {
  env SHIM_LOG="${log}" SHIM_GCLOUD_MISSING="${shim_gcloud_missing}" \
    PATH="${sandbox}/bin:/usr/bin:/bin" "$@" <"${run_stdin}"
}

expect_fail() {
  local desc="$1" want="$2"
  shift 2
  local status=0
  run "$@" >/dev/null 2>&1 || status=$?
  if ((status == want)); then
    pass "${desc}"
  else
    flunk "${desc} (exit ${status}, want ${want})"
  fi
}

expect_fail 'rejects a missing region argument' 2 "${sandboxed}"
expect_fail 'rejects a non-US region' 2 "${sandboxed}" europe-west4 --dry-run
expect_fail 'rejects an unknown flag' 2 "${sandboxed}" us-east4 --verbose --dry-run

# --dry-run is honoured wherever it appears, not only in slot 2.
out=$(run "${sandboxed}" --dry-run us-east4 2>&1) ||
  flunk 'dry-run before the region exited non-zero'
for needle in 'us-east4' 'delete' 'cpu-batch' 'gpu-batch' '01-cluster.sh' 'DRY RUN'; do
  if grep -q -- "${needle}" <<<"${out}"; then
    pass "dry-run plan mentions ${needle}"
  else
    flunk "dry-run plan missing ${needle}"
  fi
done

if [[ -s "${log}" ]]; then
  flunk "dry-run reached an external command: $(tr '\n' ';' <"${log}")"
else
  pass 'dry-run issues no gcloud/kubectl/go calls'
fi

# Full non-interactive migration against the shims.
: >"${log}"
run_stdin="${sandbox}/yes.in"
if run "${sandboxed}" us-east4 >"${sandbox}/run.out" 2>&1; then
  pass 'migration run exits 0'
else
  flunk "migration run failed: $(tail -5 "${sandbox}/run.out" | tr '\n' ';')"
fi
run_stdin=/dev/null

recorded() {
  local desc="$1" pattern="$2"
  if grep -qF -- "${pattern}" "${log}"; then
    pass "${desc}"
  else
    flunk "${desc} (no '${pattern}' in recorded calls)"
  fi
}

refute_recorded() {
  local desc="$1" pattern="$2"
  if grep -qF -- "${pattern}" "${log}"; then
    flunk "${desc} ('${pattern}' was recorded)"
  else
    pass "${desc}"
  fi
}

recorded 'tears the old region cluster down' \
  'gcloud container clusters delete spot-demo --region us-south1'
# One advisor run per advisor.yaml profile, each pinned to the target and
# writing where the pipeline reads. Both names come from the copied
# advisor.yaml; adding a profile there means adding it here.
for profile in cpu-batch gpu-batch; do
  recorded "re-renders the ${profile} profile for the target region" \
    "go run ./cmd/capacity-advisor analyze --profile ${profile} --regions us-east4 --config ../advisor.yaml --out ../out --render"
done
recorded 'runs the numbered steps' 'step 01-cluster.sh'
recorded 'runs the real KEDA step (not a phantom 05-deploy.sh)' 'step 05-keda.sh'
recorded 'runs the last numbered step' 'step 07-kueue.sh'
refute_recorded 'migration rebuilds no images' '04-build.sh'

# Finding 2 (round 1): the rebuilt config must carry ZONES as well as REGION, or
# 01-cluster.sh refuses to run.
env_file="${sandbox}/out/cluster-config.env"
if grep -q '^REGION=us-east4$' "${env_file}" && grep -q '^ZONES=us-east4-' "${env_file}"; then
  pass 'cluster-config.env is regenerated with REGION and ZONES'
else
  flunk "cluster-config.env not regenerated: $(tr '\n' ';' <"${env_file}")"
fi

# The stale-ComputeClass hazard: 02-computeclasses.sh applies every
# out/computeclass-*.yaml it finds, so none may survive a migration still
# naming the old region's zones.
gpu_class="${sandbox}/out/computeclass-gpu.yaml"
if grep -q 'us-east4' "${gpu_class}" && ! grep -q 'us-south1' "${gpu_class}"; then
  pass 'no ComputeClass is left pinned to the old region'
else
  flunk "stale GPU ComputeClass after migration: $(tr '\n' ';' <"${gpu_class}")"
fi

# Migrating to the region the cluster is already in is a clean no-op, not a
# teardown. out/cluster-config.env now says us-east4.
: >"${log}"
noop=$(run "${sandboxed}" us-east4 2>&1) && status=0 || status=$?
if ((status == 0)) && grep -q 'already in us-east4' <<<"${noop}"; then
  pass 're-migrating to the current region is a no-op'
else
  flunk "re-migration to the current region: exit ${status}, output $(tr '\n' ';' <<<"${noop}")"
fi
if [[ -s "${log}" ]]; then
  flunk "the no-op reached an external command: $(tr '\n' ';' <"${log}")"
else
  pass 'the no-op issues no gcloud/kubectl/go calls'
fi

noop=$(run "${sandboxed}" us-east4 --dry-run 2>&1) && status=0 || status=$?
if ((status == 0)) && grep -q 'already in us-east4' <<<"${noop}" &&
  ! grep -q 'DRY RUN' <<<"${noop}"; then
  pass 'dry-run reports the no-op instead of a plan it would not perform'
else
  flunk "dry-run no-op: exit ${status}, output $(tr '\n' ';' <<<"${noop}")"
fi

# A cluster that is already gone (half-finished earlier migration) must not
# turn into a delete, and must not stop the rebuild.
: >"${log}"
run_stdin="${sandbox}/yes.in"
shim_gcloud_missing=1
if run "${sandboxed}" us-west1 >"${sandbox}/rerun.out" 2>&1; then
  pass 'migration re-runs when the old cluster is already gone'
else
  flunk "re-run with an absent cluster failed: $(tail -5 "${sandbox}/rerun.out" | tr '\n' ';')"
fi
shim_gcloud_missing=0
run_stdin=/dev/null
refute_recorded 'issues no delete for a cluster that is already gone' \
  'container clusters delete'
recorded 'still rebuilds after an absent-cluster teardown' 'step 01-cluster.sh'

exit "${fail}"
