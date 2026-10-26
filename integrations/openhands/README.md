# OpenHands Integration for FastVM (Airbox) ⚡

This directory contains the drop-in **`FastVMRuntime`** adapter connecting **[OpenHands](https://github.com/All-Hands-AI/OpenHands)** (formerly OpenDevin) to **FastVM (`airboxd`)** for sub-20ms hardware-isolated AWS Firecracker MicroVM code execution and millisecond state branching.

---

## Why Use FastVM Instead of `DockerRuntime` or `E2BRuntime`?

| Feature | OpenHands `DockerRuntime` (`runc`) | OpenHands `E2BRuntime` | **OpenHands `FastVMRuntime` (`airboxd`)** |
| :--- | :--- | :--- | :--- |
| **Isolation Boundary** | Shared Host Kernel (`cgroups`/`ns`) | Hardware KVM (Cloud SaaS) | **Hardware KVM (`Firecracker v1.7.0`)** |
| **Execution Latency** | ~243 ms per exec (5–15s init) | ~100–250 ms (WAN RTT) | **8.0 ms P50 Server (`11.9 ms` RTT)** |
| **Speculative Branching** | ❌ No (`git reset` mutates in-place) | ❌ No | **✅ Yes (`runtime.branch()`)** |
| **Self-Hosted / MIT** | ✅ Yes | ❌ Commercial API Key | **✅ Yes (100% Open Source MIT)** |

---

## Quickstart

### 1. Start the FastVM (`airboxd`) Control Plane
```bash
# Build and start airboxd (on Linux KVM with Firecracker, or macOS local dev mode)
make build
./bin/airboxd --port 8080 --kernel assets/vmlinux --rootfs assets/rootfs.ext4
```

### 2. Use `FastVMRuntime` with OpenHands Actions
```python
from integrations.openhands import (
    FastVMRuntime,
    CmdRunAction,
    IPythonRunCellAction,
    FileWriteAction,
    FileReadAction,
)

# 1. Initialize and pin a warm Firecracker MicroVM (<12ms)
rt = FastVMRuntime(base_url="http://localhost:8080")
rt.connect_sync()

# 2. Run stateful IPython cells and Bash commands in the same MicroVM
rt.run_ipython(IPythonRunCellAction(code="x = 40; y = 2"))
obs = rt.run_ipython(IPythonRunCellAction(code="print(x + y)"))
print(obs.content)  # -> 42

bash_obs = rt.run(CmdRunAction(command="uname -a"))
print(bash_obs.content)

# 3. Read & write files inside the MicroVM overlay
rt.write(FileWriteAction(path="/tmp/patch.py", content="print('patched')\n"))
print(rt.read(FileReadAction(path="/tmp/patch.py")).content)

# 4. Fork the live MicroVM into parallel branches for MCTS / Tree-of-Thoughts
branch_a = rt.branch("candidate-patch-a")
branch_b = rt.branch("candidate-patch-b")

branch_a.run_ipython(IPythonRunCellAction(code="x = 999"))
print(rt.run_ipython(IPythonRunCellAction(code="print(x)")).content)  # Still 40 in parent!

# 5. Release sandboxes back to the pool
branch_a.close()
branch_b.close()
rt.close()
```

### 3. Run the MCTS Speculative Branching Demo
With `airboxd` running on `http://localhost:8080`:
```bash
python3 integrations/openhands/examples/mcts_branching_demo.py --url http://localhost:8080
```

---

## Plugging into an Upstream OpenHands Checkout

To register `FastVMRuntime` inside a local clone of `All-Hands-AI/OpenHands`:

1. Copy [`fastvm_runtime.py`](./fastvm_runtime.py) to `openhands/runtime/impl/fastvm/fastvm_runtime.py`.
2. Register `'fastvm': FastVMRuntime` in `openhands/runtime/__init__.py` inside `get_runtime_cls()`.
3. Launch OpenHands pointing to your FastVM daemon:
   ```bash
   export RUNTIME=fastvm
   export SANDBOX_API_URL=http://localhost:8080
   poetry run python -m openhands.core.main -t "Fix the failing unit tests in this repo"
   ```
