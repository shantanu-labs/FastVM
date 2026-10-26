#!/usr/bin/env python3
"""
End-to-End Demo: OpenHands + FastVM Speculative State Branching (MCTS Rollouts).

Demonstrates an AI coding agent setting up state in a parent Firecracker MicroVM,
forking into 3 isolated copy-on-write branches via `runtime.branch()`, evaluating
3 candidate patches in parallel, and keeping the winning branch without mutating
the parent sandbox.
"""

from __future__ import annotations

import argparse
from concurrent.futures import ThreadPoolExecutor
import os
import sys

sys.path.insert(0, os.path.abspath(os.path.join(os.path.dirname(__file__), "../..")))

from openhands import (
    CmdRunAction,
    FastVMRuntime,
    FileReadAction,
    FileWriteAction,
    IPythonRunCellAction,
)


def main() -> None:
    parser = argparse.ArgumentParser(description="OpenHands FastVM MCTS Branching Demo")
    parser.add_argument(
        "--url",
        default=os.environ.get("SANDBOX_API_URL", "http://localhost:8080"),
        help="FastVM (airboxd) HTTP control plane URL",
    )
    args = parser.parse_args()

    print(f"[1/4] Connecting OpenHands FastVMRuntime to {args.url} ...")
    parent_rt = FastVMRuntime(base_url=args.url, sandbox_id="oh-parent-mcts")
    parent_rt.connect_sync()
    print(f"      Pinned parent sandbox: {parent_rt.sandbox_id}")

    # 1. Initialize repository/problem state in parent sandbox
    print("[2/4] Initializing buggy state & test harness in parent sandbox...")
    parent_rt.run_ipython(
        IPythonRunCellAction(
            code=(
                "dataset = [10, 20, 30, 40]\n"
                "expected_mean = 25.0\n"
                "def compute_metric(items):\n"
                "    return sum(items)  # BUG: returns sum instead of mean\n"
            )
        )
    )

    # Verify FileWriteAction / FileReadAction and CmdRunAction on the same sandbox
    parent_rt.write(FileWriteAction(path="/tmp/oh_task.txt", content="SWE-bench #402\n"))
    read_obs = parent_rt.read(FileReadAction(path="/tmp/oh_task.txt"))
    bash_obs = parent_rt.run(CmdRunAction(command="uname -s"))
    print(f"      Task file: {read_obs.content.strip()} | Guest OS: {bash_obs.content.strip()}")

    # 2. Fork parent sandbox into 3 isolated branches for candidate patches
    print("[3/4] Forking parent sandbox into 3 isolated CoW branches...")
    candidate_patches = [
        (
            "oh-branch-patch-1",
            "def compute_metric(items): return sum(items) / (len(items) + 1)\n"
            "assert compute_metric(dataset) == expected_mean, f'Got {compute_metric(dataset)}'\n"
            "print('PATCH_1_PASSED')",
        ),
        (
            "oh-branch-patch-2",
            "def compute_metric(items): return sum(items) / len(items)\n"
            "assert compute_metric(dataset) == expected_mean, f'Got {compute_metric(dataset)}'\n"
            "print('PATCH_2_PASSED')",
        ),
        (
            "oh-branch-patch-3",
            "dataset.clear()\n"
            "def compute_metric(items): return 0\n"
            "assert compute_metric(dataset) == expected_mean, f'Got {compute_metric(dataset)}'\n"
            "print('PATCH_3_PASSED')",
        ),
    ]

    branches = [parent_rt.branch(branch_id) for branch_id, _ in candidate_patches]

    def evaluate_branch(rt: FastVMRuntime, patch_code: str) -> tuple[str, bool, str]:
        obs = rt.run_ipython(IPythonRunCellAction(code=patch_code))
        passed = "PASSED" in obs.content and "AssertionError" not in obs.content
        return rt.sandbox_id, passed, obs.content.strip()

    with ThreadPoolExecutor(max_workers=3) as pool:
        results = list(
            pool.map(
                lambda item: evaluate_branch(item[0], item[1][1]),
                zip(branches, candidate_patches),
            )
        )

    winning_branch = None
    for sbx_id, passed, output in results:
        status = "PASS" if passed else "FAIL"
        print(f"      [{sbx_id}] -> {status}: {output.splitlines()[-1] if output else ''}")
        if passed:
            winning_branch = sbx_id

    # 3. Verify parent sandbox state was not mutated by Patch 3's `dataset.clear()`
    verify_obs = parent_rt.run_ipython(IPythonRunCellAction(code="print(len(dataset), compute_metric(dataset))"))
    print(f"[4/4] Parent sandbox state intact: len(dataset), compute_metric = {verify_obs.content.strip()}")
    print(f"      Winning MCTS branch selected: {winning_branch}")

    # Clean up
    for b in branches:
        b.close()
    parent_rt.close()


if __name__ == "__main__":
    main()
