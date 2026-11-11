"""Minimal stdio MCP server used to verify Yuanpu's managed Python path."""

import asyncio
import json
import os
import shutil
import subprocess
import sys

from typing import Annotated, TypedDict

from mcp.server.fastmcp import FastMCP
from mcp.types import ToolAnnotations
from pydantic import Field


class EchoResult(TypedDict):
    text: str
    length: int


class DialogResult(TypedDict):
    shown: bool
    platform: str
    process_id: int


mcp = FastMCP("yuanpu_echo_mcp", log_level="ERROR")
_dialog_tasks: set[asyncio.Task[None]] = set()


def _configured_text(text: str) -> str:
    config_file = os.environ.get("YUANPU_CAPABILITY_CONFIG_FILE")
    if not config_file:
        return text
    try:
        with open(config_file, encoding="utf-8") as handle:
            value = json.load(handle)
        prefix = value.get("responsePrefix", "")
        return f"{prefix}{text}" if isinstance(prefix, str) else text
    except (OSError, ValueError, TypeError):
        return text


@mcp.tool(
    name="yuanpu_echo_text",
    annotations=ToolAnnotations(
        title="Echo text",
        readOnlyHint=True,
        destructiveHint=False,
        idempotentHint=True,
        openWorldHint=False,
    ),
    structured_output=True,
)
async def yuanpu_echo_text(
    text: Annotated[
        str,
        Field(description="Text to return unchanged.", min_length=1, max_length=4096),
    ],
) -> EchoResult:
    """Return text unchanged with its Unicode code-point length."""

    configured = _configured_text(text)
    return {"text": configured, "length": len(configured)}


@mcp.tool(
    name="yuanpu_approved_echo",
    annotations=ToolAnnotations(
        title="Echo text after one-time approval",
        readOnlyHint=False,
        destructiveHint=False,
        idempotentHint=True,
        openWorldHint=False,
    ),
    structured_output=True,
)
async def yuanpu_approved_echo(
    text: Annotated[
        str,
        Field(description="Text to return after explicit host approval.", min_length=1, max_length=4096),
    ],
) -> EchoResult:
    """Exercise Yuanpu's trusted one-time approval flow before returning text."""

    configured = _configured_text(text)
    return {"text": configured, "length": len(configured)}


def _dialog_command(text: str, title: str) -> list[str]:
    """Build an injection-safe native dialog command for the current platform."""

    if sys.platform == "darwin":
        return [
            "/usr/bin/osascript",
            "-e",
            "on run argv",
            "-e",
            "display dialog (item 1 of argv) with title (item 2 of argv) "
            'buttons {"OK"} default button "OK"',
            "-e",
            "end run",
            "--",
            text,
            title,
        ]
    if sys.platform == "win32":
        system_root = os.environ.get("SYSTEMROOT", r"C:\Windows")
        powershell = os.path.join(
            system_root,
            "System32",
            "WindowsPowerShell",
            "v1.0",
            "powershell.exe",
        )
        return [
            powershell,
            "-NoProfile",
            "-NonInteractive",
            "-Command",
            "Add-Type -AssemblyName PresentationFramework; "
            "[System.Windows.MessageBox]::Show($args[0], $args[1]) | Out-Null",
            text,
            title,
        ]
    dialog = shutil.which("zenity") or shutil.which("xmessage")
    if not dialog:
        raise RuntimeError(
            "No supported Linux dialog program is installed; install zenity or xmessage."
        )
    if os.path.basename(dialog) == "zenity":
        return [dialog, "--info", f"--text={text}", f"--title={title}"]
    return [dialog, "-title", title, text]


async def _reap_dialog(process: asyncio.subprocess.Process) -> None:
    await process.wait()


@mcp.tool(
    name="yuanpu_show_message",
    annotations=ToolAnnotations(
        title="Show a desktop message",
        readOnlyHint=False,
        destructiveHint=False,
        idempotentHint=False,
        openWorldHint=False,
    ),
    structured_output=True,
)
async def yuanpu_show_message(
    text: Annotated[
        str,
        Field(description="Message displayed in the desktop dialog.", min_length=1, max_length=500),
    ],
    title: Annotated[
        str,
        Field(description="Short title displayed above the message.", min_length=1, max_length=80),
    ] = "MindLink",
) -> DialogResult:
    """Show one native desktop dialog after the host grants one-time approval."""

    command = _dialog_command(text, title)
    process = await asyncio.create_subprocess_exec(
        *command,
        stdin=subprocess.DEVNULL,
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
    )
    task = asyncio.create_task(_reap_dialog(process))
    _dialog_tasks.add(task)
    task.add_done_callback(_dialog_tasks.discard)
    return {"shown": True, "platform": sys.platform, "process_id": process.pid}


@mcp.tool(
    name="yuanpu_diagnostic_error",
    annotations=ToolAnnotations(
        title="Return a diagnostic error",
        readOnlyHint=True,
        destructiveHint=False,
        idempotentHint=True,
        openWorldHint=False,
    ),
)
async def yuanpu_diagnostic_error() -> str:
    """Return a controlled tool error for transport and UI diagnostics."""

    raise ValueError("Yuanpu managed Python diagnostic error")


@mcp.tool(
    name="yuanpu_wait",
    annotations=ToolAnnotations(
        title="Wait for cancellation diagnostics",
        readOnlyHint=True,
        destructiveHint=False,
        idempotentHint=True,
        openWorldHint=False,
    ),
)
async def yuanpu_wait(
    seconds: Annotated[
        float,
        Field(description="Seconds to wait before returning.", ge=0.0, le=10.0),
    ] = 1.0,
) -> str:
    """Wait for a bounded interval so clients can verify cancellation."""

    await asyncio.sleep(seconds)
    return f"waited {seconds:g} seconds"


async def yuanpu_spawn_child() -> dict[str, int]:
    """Spawn a sleeping descendant used only to verify process-tree cleanup."""

    child = subprocess.Popen(  # noqa: S603 - fixed interpreter and fixed test program
        [sys.executable, "-c", "import time; time.sleep(60)"],
        stdin=subprocess.DEVNULL,
    )
    return {"pid": child.pid}


async def yuanpu_spawn_child_and_exit() -> dict[str, int]:
    """Spawn a descendant, then crash the MCP root for lifecycle testing."""

    result = await yuanpu_spawn_child()
    asyncio.get_running_loop().call_later(0.1, os._exit, 17)
    return result


async def yuanpu_write_marker_and_exit(marker: str) -> None:
    """Persist one test marker, then exit before an MCP result can be sent."""

    with open(marker, "a", encoding="utf-8") as marker_file:
        marker_file.write("executed\n")
        marker_file.flush()
        os.fsync(marker_file.fileno())
    os._exit(23)


if os.environ.get("YUANPU_MCP_TEST_FIXTURES") == "1":
    mcp.tool(
        name="yuanpu_spawn_child",
        annotations=ToolAnnotations(
            title="Spawn a lifecycle-test child",
            readOnlyHint=True,
            destructiveHint=False,
            idempotentHint=True,
            openWorldHint=False,
        ),
        structured_output=True,
    )(yuanpu_spawn_child)
    mcp.tool(
        name="yuanpu_spawn_child_and_exit",
        annotations=ToolAnnotations(
            title="Spawn a child and exit the test server",
            readOnlyHint=True,
            destructiveHint=False,
            idempotentHint=True,
            openWorldHint=False,
        ),
        structured_output=True,
    )(yuanpu_spawn_child_and_exit)
    mcp.tool(
        name="yuanpu_write_marker_and_exit",
        annotations=ToolAnnotations(
            title="Write a lifecycle-test marker and exit",
            readOnlyHint=False,
            destructiveHint=True,
            idempotentHint=False,
            openWorldHint=False,
        ),
    )(yuanpu_write_marker_and_exit)


def main() -> None:
    """Run the local MCP server over stdio; stdout is reserved for MCP frames."""

    mcp.run(transport="stdio")


if __name__ == "__main__":
    main()
