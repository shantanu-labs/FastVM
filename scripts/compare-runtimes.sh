#!/usr/bin/env bash
# ==============================================================================
# Airbox (FastVM) Head-to-Head Runtime Benchmark Suite
# ==============================================================================
# Compares Python code execution latency (`print(2 + 2)`) across:
#   1. FastVM (Airbox)       - Hardware KVM (Firecracker Warm Pool + AF_VSOCK)
#   2. Cold-Boot Firecracker - Hardware KVM (Unpooled Firecracker Cold Boot)
#   3. Docker (runc)         - Shared Host Kernel (Linux namespaces + cgroups)
#   4. gVisor (runsc)        - User-Space Application Kernel (Sentry)
#
# Usage:
#   # Option A: Run directly on a Linux KVM host
#   ./scripts/compare-runtimes.sh --runs 30
#
#   # Option B: Auto-provision an ephemeral GCP Spot KVM VM using your own gcloud auth
#   gcloud auth login && gcloud config set project YOUR_GCP_PROJECT_ID
#   ./scripts/compare-runtimes.sh --provision-gcp --runs 30
# ==============================================================================

set -euo pipefail

RUNS=30
PROVISION_GCP=false
GCP_ZONE="${GCP_ZONE:-us-central1-a}"
GCP_MACHINE_TYPE="${GCP_MACHINE_TYPE:-n2-standard-2}"
GCP_INSTANCE_NAME="${GCP_INSTANCE_NAME:-airbox-bench-$(date +%s)}"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --runs)
      RUNS="$2"
      shift 2
      ;;
    --provision-gcp)
      PROVISION_GCP=true
      shift
      ;;
    --zone)
      GCP_ZONE="$2"
      shift 2
      ;;
    -h|--help)
      sed -n '2,18p' "$0"
      exit 0
      ;;
    *)
      echo "Unknown option: $1"
      exit 1
      ;;
  esac
done

# ------------------------------------------------------------------------------
# Mode B: Ephemeral GCP Spot VM Orchestration (Uses caller's own gcloud auth)
# ------------------------------------------------------------------------------
if [[ "${PROVISION_GCP}" == "true" ]]; then
  if ! command -v gcloud >/dev/null 2>&1; then
    echo "ERROR: 'gcloud' CLI is not installed or not in PATH."
    echo "Install Google Cloud SDK and run: gcloud auth login && gcloud config set project <YOUR_PROJECT_ID>"
    exit 1
  fi

  ACTIVE_PROJECT="${GCP_PROJECT:-$(gcloud config get-value project 2>/dev/null || true)}"
  if [[ -z "${ACTIVE_PROJECT}" || "${ACTIVE_PROJECT}" == "(unset)" ]]; then
    echo "ERROR: No active GCP project configured."
    echo "Please configure your own GCP project first:"
    echo "  gcloud auth login"
    echo "  gcloud config set project <YOUR_GCP_PROJECT_ID>"
    exit 1
  fi

  echo "==> Using active GCP project from local gcloud config (Zone: ${GCP_ZONE})"
  echo "==> Provisioning ephemeral Spot KVM instance '${GCP_INSTANCE_NAME}' (${GCP_MACHINE_TYPE})..."

  cleanup_gcp() {
    echo ""
    echo "==> Cleaning up ephemeral GCP instance '${GCP_INSTANCE_NAME}'..."
    gcloud compute instances delete "${GCP_INSTANCE_NAME}" \
      --zone="${GCP_ZONE}" \
      --quiet >/dev/null 2>&1 || true
    echo "✓ Ephemeral GCP instance deleted."
  }
  trap cleanup_gcp EXIT INT TERM

  gcloud compute instances create "${GCP_INSTANCE_NAME}" \
    --zone="${GCP_ZONE}" \
    --machine-type="${GCP_MACHINE_TYPE}" \
    --provisioning-model=SPOT \
    --instance-termination-action=DELETE \
    --enable-nested-virtualization \
    --image-family=ubuntu-2204-lts \
    --image-project=ubuntu-os-cloud \
    --boot-disk-size=30GB \
    --boot-disk-type=pd-ssd \
    --quiet

  echo "==> Waiting for SSH readiness on '${GCP_INSTANCE_NAME}'..."
  for _ in {1..30}; do
    if gcloud compute ssh "${GCP_INSTANCE_NAME}" --zone="${GCP_ZONE}" --command="echo ready" --quiet >/dev/null 2>&1; then
      break
    fi
    sleep 3
  done

  REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
  TARBALL="$(mktemp /tmp/airbox-src-XXXXXX.tar.gz)"
  COPYFILE_DISABLE=1 tar --exclude='.git' --exclude='assets' --exclude='bin' --exclude='hidden' -czf "${TARBALL}" -C "${REPO_ROOT}" .

  gcloud compute scp "${TARBALL}" "${GCP_INSTANCE_NAME}:/tmp/airbox-src.tar.gz" --zone="${GCP_ZONE}" --quiet
  rm -f "${TARBALL}"

  gcloud compute ssh "${GCP_INSTANCE_NAME}" --zone="${GCP_ZONE}" --quiet --command="
    set -euo pipefail
    export DEBIAN_FRONTEND=noninteractive LC_ALL=C.UTF-8 LANG=C.UTF-8
    mkdir -p ~/airbox && tar -xzf /tmp/airbox-src.tar.gz -C ~/airbox 2>/dev/null
    cd ~/airbox
    chmod +x scripts/*.sh
    ./scripts/compare-runtimes.sh --runs ${RUNS}
  "
  exit 0
fi

# ------------------------------------------------------------------------------
# Mode A: Native Linux KVM Host Execution
# ------------------------------------------------------------------------------
if [[ "$(uname -s)" != "Linux" ]]; then
  echo "ERROR: Local runtime comparison requires a Linux host with /dev/kvm."
  echo "To run automatically on an ephemeral GCP KVM node using your own gcloud credentials, pass --provision-gcp:"
  echo "  ./scripts/compare-runtimes.sh --provision-gcp --runs ${RUNS}"
  exit 1
fi

export DEBIAN_FRONTEND=noninteractive LC_ALL=C.UTF-8 LANG=C.UTF-8
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${REPO_ROOT}"

echo "==> [1/4] Ensuring dependencies (Go, Firecracker, Docker, gVisor runsc, Python3)..."
if ! command -v docker >/dev/null 2>&1 || ! command -v make >/dev/null 2>&1 || ! command -v killall >/dev/null 2>&1; then
  sudo -E apt-get update -qq
  sudo -E apt-get install -y -qq apt-transport-https ca-certificates curl gnupg make psmisc docker.io wget jq e2fsprogs python3 >/dev/null
fi

if ! command -v go >/dev/null 2>&1; then
  echo "==> Installing Go 1.24.0..."
  wget -q https://go.dev/dl/go1.24.0.linux-amd64.tar.gz -O /tmp/go.tar.gz
  sudo rm -rf /usr/local/go && sudo tar -C /usr/local -xzf /tmp/go.tar.gz
fi
export PATH="/usr/local/go/bin:${PATH}"

if ! command -v runsc >/dev/null 2>&1; then
  echo "==> Installing gVisor (runsc)..."
  curl -fsSL https://gvisor.dev/archive.key | sudo gpg --dearmor -o /usr/share/keyrings/gvisor-archive-keyring.gpg
  echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/gvisor-archive-keyring.gpg] https://storage.googleapis.com/gvisor/releases release main" | sudo tee /etc/apt/sources.list.d/gvisor.list >/dev/null
  sudo -E apt-get update -qq
  sudo -E apt-get install -y -qq runsc >/dev/null
  sudo runsc install >/dev/null 2>&1 || true
  sudo systemctl restart docker
fi

if [[ ! -f /usr/local/bin/firecracker ]]; then
  ./scripts/setup-host.sh
fi

if [[ ! -f assets/vmlinux || ! -f assets/rootfs.ext4 ]]; then
  ./scripts/download-assets.sh
fi

echo "==> Pre-pulling python:3.10-slim image for Docker & gVisor..."
sudo docker pull -q python:3.10-slim >/dev/null

echo "==> Building Airbox (airboxd + agent-init) and injecting into rootfs..."
make build >/dev/null

cleanup_daemons() {
  sudo killall -9 -q airboxd firecracker 2>/dev/null || true
}
trap cleanup_daemons EXIT

# Helper: Python statistical timer that runs a shell command N times and prints JSON stats
run_timed_suite() {
  local label="$1"
  local isolation="$2"
  local cmd="$3"
  local runs="$4"

  python3 - "$label" "$isolation" "$cmd" "$runs" <<'PYEOF'
import json, math, subprocess, sys, time

label, isolation, cmd, runs = sys.argv[1], sys.argv[2], sys.argv[3], int(sys.argv[4])

# 2 warmup iterations
for _ in range(2):
    subprocess.run(cmd, shell=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)

samples_ms = []
t_suite_start = time.perf_counter()
for _ in range(runs):
    t0 = time.perf_counter()
    res = subprocess.run(cmd, shell=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    dt = (time.perf_counter() - t0) * 1000.0
    if res.returncode == 0:
        samples_ms.append(dt)
t_suite_total = time.perf_counter() - t_suite_start

samples_ms.sort()
n = len(samples_ms)
def pct(p):
    if n == 0:
        return 0.0
    idx = min(n - 1, max(0, math.ceil((p / 100.0) * n) - 1))
    return samples_ms[idx]

out = {
    "label": label,
    "isolation": isolation,
    "runs": n,
    "min_ms": round(samples_ms[0], 2) if n else 0,
    "p50_ms": round(pct(50), 2),
    "p95_ms": round(pct(95), 2),
    "p99_ms": round(pct(99), 2),
    "max_ms": round(samples_ms[-1], 2) if n else 0,
    "rps": round(n / t_suite_total, 2) if t_suite_total > 0 else 0,
}
print(json.dumps(out))
PYEOF
}

# 1. Benchmark FastVM (Airbox Warm Pool)
echo "==> [2/4] Benchmarking FastVM (Airbox Pre-Warmed Firecracker Pool) (${RUNS} runs)..."
cleanup_daemons
sudo ./bin/airboxd --port 8080 --kernel assets/vmlinux --rootfs assets/rootfs.ext4 --warm-pool-size 10 >/tmp/airbox-warm.log 2>&1 &
for _ in {1..30}; do
  if curl -sf http://localhost:8080/healthz >/dev/null 2>&1; then break; fi
  sleep 1
done
sleep 2

AIRBOX_WARM_JSON=$(run_timed_suite \
  "FastVM (Airbox Warm Pool)" \
  "Hardware KVM (Firecracker)" \
  "curl -sf -X POST http://localhost:8080/v1/execute -H 'Content-Type: application/json' -d '{\"language\":\"python\",\"code\":\"print(2+2)\"}'" \
  "${RUNS}")

# 2. Benchmark Cold-Boot Firecracker (--warm-pool-size 0)
echo "==> [3/4] Benchmarking Cold-Boot Firecracker MicroVM (--warm-pool-size=0) (${RUNS} runs)..."
cleanup_daemons
sleep 1
sudo ./bin/airboxd --port 8080 --kernel assets/vmlinux --rootfs assets/rootfs.ext4 --warm-pool-size 0 >/tmp/airbox-cold.log 2>&1 &
for _ in {1..20}; do
  if curl -sf http://localhost:8080/healthz >/dev/null 2>&1; then break; fi
  sleep 1
done

FIRECRACKER_COLD_JSON=$(run_timed_suite \
  "Standard Firecracker (Cold Boot)" \
  "Hardware KVM (Firecracker)" \
  "curl -sf -X POST http://localhost:8080/v1/execute -H 'Content-Type: application/json' -d '{\"language\":\"python\",\"code\":\"print(2+2)\"}'" \
  "${RUNS}")

cleanup_daemons

# 3. Benchmark Docker (runc) & gVisor (runsc)
echo "==> [4/4] Benchmarking Docker (runc) and gVisor (runsc) (${RUNS} runs each)..."
DOCKER_RUNC_JSON=$(run_timed_suite \
  "Docker (runc container)" \
  "Shared Host Kernel (cgroups/ns)" \
  "sudo docker run --rm --network none python:3.10-slim python3 -c 'print(2+2)'" \
  "${RUNS}")

GVISOR_RUNSC_JSON=$(run_timed_suite \
  "gVisor (runsc sandbox)" \
  "User-Space Kernel (Sentry)" \
  "sudo docker run --rm --runtime=runsc --network none python:3.10-slim python3 -c 'print(2+2)'" \
  "${RUNS}")

# Render Markdown comparison table
python3 - "${AIRBOX_WARM_JSON}" "${FIRECRACKER_COLD_JSON}" "${DOCKER_RUNC_JSON}" "${GVISOR_RUNSC_JSON}" <<'PYEOF'
import json, sys

rows = [json.loads(arg) for arg in sys.argv[1:]]
base_p50 = rows[0]["p50_ms"]

print("\n================================================================================")
print("⚡ HEAD-TO-HEAD SANDBOX RUNTIME BENCHMARK RESULTS (Sequential Single-Worker RTT)")
print("================================================================================\n")
print("| Runtime / Architecture | Isolation Boundary | Min (ms) | P50 (ms) | P95 (ms) | P99 (ms) | Relative Latency |")
print("| :--- | :--- | :---: | :---: | :---: | :---: | :---: |")
for idx, r in enumerate(rows):
    ratio = r["p50_ms"] / base_p50 if base_p50 > 0 else 1.0
    rel = "**1.0x (Baseline)**" if idx == 0 else f"`{ratio:.1f}x slower`"
    p50_str = f"**`{r['p50_ms']:.2f} ms`**" if idx == 0 else f"`{r['p50_ms']:.2f} ms`"
    p95_str = f"**`{r['p95_ms']:.2f} ms`**" if idx == 0 else f"`{r['p95_ms']:.2f} ms`"
    print(f"| **{r['label']}** | {r['isolation']} | `{r['min_ms']:.2f} ms` | {p50_str} | {p95_str} | `{r['p99_ms']:.2f} ms` | {rel} |")
print("")
PYEOF
