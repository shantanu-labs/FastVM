"""
FastVM (Airbox) Runtime Adapter for OpenHands (formerly OpenDevin).

Provides sub-20ms hardware-isolated AWS Firecracker MicroVM execution
and millisecond speculative state branching (`runtime.branch()`) for
Tree-of-Thoughts / Monte Carlo Tree Search (MCTS) coding agents.
"""

from __future__ import annotations

import base64
from dataclasses import dataclass
import json
import os
import urllib.error
import urllib.request
import uuid
from typing import Any

try:
    from openhands.events.action import (
        CmdRunAction,
        FileReadAction,
        FileWriteAction,
        IPythonRunCellAction,
    )
    from openhands.events.observation import (
        CmdOutputObservation,
        ErrorObservation,
        FileReadObservation,
        FileWriteObservation,
        IPythonRunCellObservation,
    )
    from openhands.runtime.base import Runtime as _BaseRuntime

    OPENHANDS_AVAILABLE = True
except ImportError:
    OPENHANDS_AVAILABLE = False

    @dataclass
    class CmdRunAction:  # type: ignore[no-redef]
        command: str
        timeout: int = 30

    @dataclass
    class IPythonRunCellAction:  # type: ignore[no-redef]
        code: str
        timeout: int = 30

    @dataclass
    class FileReadAction:  # type: ignore[no-redef]
        path: str

    @dataclass
    class FileWriteAction:  # type: ignore[no-redef]
        path: str
        content: str

    @dataclass
    class CmdOutputObservation:  # type: ignore[no-redef]
        content: str
        command: str
        exit_code: int = 0

    @dataclass
    class IPythonRunCellObservation:  # type: ignore[no-redef]
        content: str
        code: str
        exit_code: int = 0

    @dataclass
    class FileReadObservation:  # type: ignore[no-redef]
        content: str
        path: str

    @dataclass
    class FileWriteObservation:  # type: ignore[no-redef]
        content: str
        path: str

    @dataclass
    class ErrorObservation:  # type: ignore[no-redef]
        content: str

    class _BaseRuntime:  # type: ignore[no-redef]
        def __init__(self, *args: Any, **kwargs: Any) -> None:
            pass


class FastVMRuntime(_BaseRuntime):
    """
    OpenHands Runtime backed by FastVM (Airbox) Firecracker MicroVMs.

    Features:
    - Sub-20ms warm Firecracker MicroVM execution over AF_VSOCK
    - Stateful Python & Bash execution bound to a pinned `sandbox_id`
    - Speculative state branching (`runtime.branch()`) via Firecracker VM snapshots
    - Zero-overhead background jobs (`runtime.start_job()`) via `/v1/jobs`
    """

    def __init__(
        self,
        base_url: str | None = None,
        sandbox_id: str | None = None,
        default_timeout_ms: int = 30000,
        *args: Any,
        **kwargs: Any,
    ) -> None:
        if OPENHANDS_AVAILABLE:
            super().__init__(*args, **kwargs)
        self.base_url = (
            base_url or os.environ.get("SANDBOX_API_URL", "http://localhost:8080")
        ).rstrip("/")
        self.sandbox_id = sandbox_id or f"oh-{uuid.uuid4().hex[:8]}"
        self.default_timeout_ms = default_timeout_ms

    def _request(
        self,
        method: str,
        path: str,
        payload: dict[str, Any] | None = None,
        timeout_sec: float = 35.0,
    ) -> dict[str, Any]:
        url = f"{self.base_url}{path}"
        data = json.dumps(payload).encode("utf-8") if payload is not None else None
        headers = {"Content-Type": "application/json"} if data is not None else {}
        req = urllib.request.Request(url, data=data, headers=headers, method=method)
        try:
            with urllib.request.urlopen(req, timeout=timeout_sec) as resp:
                body = resp.read().decode("utf-8")
                return json.loads(body) if body else {}
        except urllib.error.HTTPError as e:
            err_body = e.read().decode("utf-8", errors="replace")
            raise RuntimeError(f"FastVM API error ({e.code}) on {path}: {err_body}") from e

    async def connect(self) -> None:
        """Pin a pre-warmed Firecracker MicroVM from the FastVM pool (<12ms)."""
        self.connect_sync()

    def connect_sync(self) -> dict[str, Any]:
        """Synchronous helper to initialize and pin the MicroVM sandbox."""
        return self._exec("python", "pass", timeout_ms=5000)

    def _exec(
        self,
        language: str,
        code: str,
        timeout_ms: int | None = None,
        env: dict[str, str] | None = None,
        stdin: str | None = None,
    ) -> dict[str, Any]:
        t_ms = timeout_ms or self.default_timeout_ms
        payload: dict[str, Any] = {
            "sandbox_id": self.sandbox_id,
            "language": language,
            "code": code,
            "timeout_ms": t_ms,
        }
        if env:
            payload["env"] = env
        if stdin:
            payload["stdin"] = stdin
        return self._request(
            "POST", "/v1/execute", payload=payload, timeout_sec=(t_ms / 1000.0) + 5.0
        )

    def run(self, action: CmdRunAction) -> CmdOutputObservation:
        """Execute a shell command inside the pinned Firecracker MicroVM."""
        timeout_ms = getattr(action, "timeout", 30) * 1000
        res = self._exec("bash", action.command, timeout_ms=timeout_ms)
        stdout = res.get("stdout") or ""
        stderr = res.get("stderr") or ""
        output = stdout + stderr
        return CmdOutputObservation(
            content=output,
            command=action.command,
            exit_code=int(res.get("exit_code", 0)),
        )

    def run_ipython(self, action: IPythonRunCellAction) -> IPythonRunCellObservation:
        """Execute a stateful Python cell inside the warm in-guest interpreter."""
        timeout_ms = getattr(action, "timeout", 30) * 1000
        res = self._exec("python", action.code, timeout_ms=timeout_ms)
        stdout = res.get("stdout") or ""
        stderr = res.get("stderr") or ""
        output = stdout + stderr
        if OPENHANDS_AVAILABLE:
            return IPythonRunCellObservation(content=output, code=action.code)
        return IPythonRunCellObservation(
            content=output,
            code=action.code,
            exit_code=int(res.get("exit_code", 0)),
        )

    def read(self, action: FileReadAction) -> FileReadObservation | ErrorObservation:
        """Read a file from the MicroVM rootfs overlay."""
        code = (
            "import pathlib, sys\n"
            f"p = pathlib.Path({action.path!r})\n"
            "if not p.exists():\n"
            "    sys.stderr.write(f'File not found: {p}\\n')\n"
            "    sys.exit(1)\n"
            "sys.stdout.write(p.read_text())\n"
        )
        res = self._exec("python", code)
        if res.get("exit_code", 0) != 0:
            return ErrorObservation(content=res.get("stderr") or "Failed to read file")
        return FileReadObservation(content=res.get("stdout", ""), path=action.path)

    def write(self, action: FileWriteAction) -> FileWriteObservation | ErrorObservation:
        """Write a file into the MicroVM rootfs overlay."""
        b64 = base64.b64encode(action.content.encode("utf-8")).decode("ascii")
        code = (
            "import base64, pathlib\n"
            f"p = pathlib.Path({action.path!r})\n"
            "p.parent.mkdir(parents=True, exist_ok=True)\n"
            f"p.write_bytes(base64.b64decode({b64!r}))\n"
        )
        res = self._exec("python", code)
        if res.get("exit_code", 0) != 0:
            return ErrorObservation(content=res.get("stderr") or "Failed to write file")
        return FileWriteObservation(content="", path=action.path)

    def branch(self, new_sandbox_id: str | None = None) -> "FastVMRuntime":
        """
        Snapshot and fork the running Firecracker MicroVM into an isolated CoW branch.

        Enables Tree-of-Thoughts / MCTS coding agents to explore multiple candidate
        code patches in parallel without mutating the parent sandbox state.
        """
        resp = self._request(
            "POST",
            f"/v1/sandboxes/{self.sandbox_id}/branch",
            payload={"new_sandbox_id": new_sandbox_id or ""},
        )
        child_id = resp["sandbox_id"]
        return FastVMRuntime(
            base_url=self.base_url,
            sandbox_id=child_id,
            default_timeout_ms=self.default_timeout_ms,
        )

    def start_job(
        self, code: str, language: str = "python", timeout_ms: int = 60000
    ) -> dict[str, Any]:
        """Dispatch an asynchronous background job (<1ms HTTP 202 Accepted)."""
        return self._request(
            "POST",
            "/v1/jobs",
            payload={
                "sandbox_id": self.sandbox_id,
                "language": language,
                "code": code,
                "timeout_ms": timeout_ms,
            },
        )

    def get_job(self, job_id: str) -> dict[str, Any]:
        """Poll status and output of a background job."""
        return self._request("GET", f"/v1/jobs/{job_id}")

    def cancel_job(self, job_id: str) -> dict[str, Any]:
        """Cancel a running background job."""
        return self._request("DELETE", f"/v1/jobs/{job_id}")

    def close(self) -> None:
        """Terminate the pinned sandbox and return resources to the FastVM pool."""
        try:
            self._request("DELETE", f"/v1/sandboxes/{self.sandbox_id}", timeout_sec=5.0)
        except Exception:
            pass
