# FastVM (Airbox) Architecture & Design Documentation

Welcome to the technical documentation for **FastVM (Airbox)**—a sub-100ms, hardware-isolated AWS Firecracker microVM execution engine built for autonomous coding agents.

## 📚 Documentation Index

1. **[System Architecture Overview](./architecture.md)**
   - End-to-end architecture connecting the HTTP Control Plane (`airboxd`), Hypervisor Driver (`FirecrackerHypervisor`), and In-Guest PID 1 Agent (`agent-init`).
   - Latency budget breakdown (how FastVM achieves an ~8ms server-side P50 vs ~620ms cold Firecracker).
   - Cross-platform development and fallback model (`firecracker` vs `mock`).

2. **[VSOCK & In-Guest Agent (`agent-init`)](./vsock-and-guest-agent.md)**
   - How `agent-init` boots as Linux `PID 1` inside the microVM.
   - How `AF_VSOCK` (`virtio-vsock`) bridges the host and guest without SSH or TCP/IP overhead.
   - The Firecracker UNIX domain socket handshake (`CONNECT 5000\n` $\rightarrow$ `OK <port>\n`).
   - The pre-warmed in-guest Python REPL worker (`pyWorker`) and state serialization.

3. **[Sandbox Lifecycle, Warm Pooling & State Branching](./sandbox-lifecycle-and-branching.md)**
   - Pre-warmed Firecracker microVM pool (`warmPool`) and automatic slot replenishment.
   - Ephemeral copy-on-write rootfs overlays in `/dev/shm` (`tmpfs`).
   - Ephemeral vs. stateful/pinned sandboxes.
   - Speculative agent state branching (`PauseVM` $\rightarrow$ `CreateSnapshot` $\rightarrow$ `ResumeVM`).
   - Asynchronous background jobs (`/v1/jobs`).

4. **[Networking & Security Isolation](./networking-and-security.md)**
   - Zero-CLI Linux Netlink TAP device pool (`tap0`..`tap99`) and `/30` subnet IPAM.
   - Host `iptables` firewall, NAT masquerading, and strict SSRF / Cloud Metadata (`169.254.169.254/32`) lockdown.
   - Multi-layer defense-in-depth: KVM hardware virtualization, OOM-killer protection (`oom_score_adj`), and fork-bomb containment (`threads-max` & `ulimit`).
