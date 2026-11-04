"""FastMCP protocol surface for WordBridge.

The Word/COM implementation stays in :class:`WordBridgeService`.  This module
only describes the public tools and delegates each call to that service, so
stdio and Streamable HTTP always expose the same schemas and behavior.
"""

from __future__ import annotations

from typing import Any, Literal, Protocol

from fastmcp import FastMCP
from fastmcp.exceptions import ToolError
from mcp.types import ToolAnnotations
from pydantic import BaseModel, Field


class WordBridgeHandlers(Protocol):
    async def handle_capture(self, timeout_ms: int = 30000) -> dict: ...
    async def handle_modify(self, region_id: str, new_text: str) -> dict: ...
    async def handle_force(self, region_id: str, new_text: str) -> dict: ...
    async def handle_diff(self, region_id: str) -> dict: ...
    async def handle_reject(self, region_id: str) -> dict: ...
    async def handle_patch(self, region_id: str, patches: list) -> dict: ...
    async def handle_get_selection(self) -> dict: ...
    async def handle_list(self) -> dict: ...
    async def handle_status(self, region_id: str) -> dict: ...
    async def handle_list_documents(self) -> dict: ...
    async def handle_find(self, text: str, max_matches: int = 20) -> dict: ...
    async def handle_create_document(
        self,
        title: str = "",
        content: str = "",
        filename: str = "",
    ) -> dict: ...
    async def handle_translate(
        self,
        text: str = "",
        domain: str = "legal",
        provider: str | None = None,
        model: str | None = None,
        task_id: str | None = None,
        glossary: dict | None = None,
    ) -> dict: ...
    async def handle_translate_status(self, task_id: str) -> dict: ...
    async def handle_translate_glossary(
        self,
        action: str,
        source_term: str = "",
        target_term: str = "",
        domain: str = "general",
    ) -> dict: ...


class TextPatch(BaseModel):
    """One exact replacement inside a captured Word region."""

    find: str = Field(description="要定位的原文片段；必须在 Region 中唯一匹配")
    replace: str = Field(description="替换后的文本")


READ_ONLY = ToolAnnotations(
    readOnlyHint=True,
    destructiveHint=False,
    idempotentHint=True,
    openWorldHint=False,
)
LOCAL_WRITE = ToolAnnotations(
    readOnlyHint=False,
    destructiveHint=False,
    idempotentHint=False,
    openWorldHint=False,
)
FORCE_WRITE = ToolAnnotations(
    readOnlyHint=False,
    destructiveHint=True,
    idempotentHint=False,
    openWorldHint=False,
)
NETWORK_TASK = ToolAnnotations(
    readOnlyHint=False,
    destructiveHint=False,
    idempotentHint=False,
    openWorldHint=True,
)


def _checked(result: dict[str, Any]) -> dict[str, Any]:
    """Convert business failures into an MCP tool error with a useful message."""
    if result.get("status") == "error":
        raise ToolError(str(result.get("message") or result.get("error") or "WordBridge 调用失败"))
    return result


def create_mcp_server(service: WordBridgeHandlers, version: str = "1.0.0") -> FastMCP:
    """Build the FastMCP server around an initialized service instance."""
    mcp = FastMCP(
        name="WordBridge",
        instructions=(
            "Call word_get_active_selection before editing. Prefer word_patch for minimal "
            "tracked changes; use word_force only after an explicit overwrite decision."
        ),
        version=version,
    )

    @mcp.tool(annotations=LOCAL_WRITE)
    async def word_capture(timeout_ms: int = 30000) -> dict[str, Any]:
        """等待并获取已捕获的 Word 选区，超时单位为毫秒。"""
        return _checked(await service.handle_capture(timeout_ms))

    @mcp.tool(annotations=LOCAL_WRITE)
    async def word_modify(region_id: str, new_text: str) -> dict[str, Any]:
        """将新文本写回选区；写回前检查用户是否修改过原文并保留格式。"""
        return _checked(await service.handle_modify(region_id, new_text))

    @mcp.tool(annotations=FORCE_WRITE)
    async def word_force(region_id: str, new_text: str) -> dict[str, Any]:
        """忽略冲突并强制覆盖选区，可能覆盖用户在处理期间所做的修改。"""
        return _checked(await service.handle_force(region_id, new_text))

    @mcp.tool(annotations=READ_ONLY)
    async def word_diff(region_id: str) -> dict[str, Any]:
        """返回原文、Word 当前文本和 AI 建议文本的冲突详情。"""
        return _checked(await service.handle_diff(region_id))

    @mcp.tool(annotations=LOCAL_WRITE)
    async def word_reject(region_id: str) -> dict[str, Any]:
        """拒绝待处理的 AI 修改并保持 Word 原文不变。"""
        return _checked(await service.handle_reject(region_id))

    @mcp.tool(annotations=READ_ONLY)
    async def word_list() -> dict[str, Any]:
        """列出当前仍然有效的已捕获 Word 区域。"""
        return _checked(await service.handle_list())

    @mcp.tool(annotations=READ_ONLY)
    async def word_list_documents() -> dict[str, Any]:
        """列出当前打开的 Word 文档及其激活状态。"""
        return _checked(await service.handle_list_documents())

    @mcp.tool(annotations=READ_ONLY)
    async def word_read_document(document_id: str = "") -> dict[str, Any]:
        """读取打开的 Word 文档正文全部内容并捕获为 region（仅读取不修改）。document_id 用 word_list_documents 返回的 path；留空读活动文档（多文档时需指定）。"""
        return _checked(await service.handle_read_document(document_id))

    @mcp.tool(annotations=READ_ONLY)
    async def word_find(text: str, max_matches: int = 20) -> dict[str, Any]:
        """在 Word 文档中查找文本并捕获为可编辑区域（仅定位，不修改）。返回 region_id，后续用 word_patch 做最小修订。"""
        return _checked(await service.handle_find(text, max_matches))

    @mcp.tool(annotations=LOCAL_WRITE)
    async def word_create_document(
        title: str = "",
        content: str = "",
        filename: str = "",
    ) -> dict[str, Any]:
        """用 Markdown 风格内容生成 .docx 并在 Word 中打开。content 支持 #/##/### 标题、- 列表、1. 编号、纯文本段落。"""
        return _checked(await service.handle_create_document(title, content, filename))

    @mcp.tool(annotations=LOCAL_WRITE)
    async def word_get_active_selection() -> dict[str, Any]:
        """立即读取 Word 当前选区并缓存其文本、格式及回填位置。"""
        return _checked(await service.handle_get_selection())

    @mcp.tool(annotations=READ_ONLY)
    async def word_status(region_id: str) -> dict[str, Any]:
        """查询一个已捕获 Word 区域的详细处理状态。"""
        return _checked(await service.handle_status(region_id))

    @mcp.tool(annotations=LOCAL_WRITE)
    async def word_patch(region_id: str, patches: list[TextPatch]) -> dict[str, Any]:
        """按唯一原文片段做精确替换，只在 Word 中产生最小范围修订。"""
        return _checked(
            await service.handle_patch(
                region_id, [patch.model_dump() for patch in patches]
            )
        )

    @mcp.tool(annotations=NETWORK_TASK)
    async def translate_long_text(
        text: str = "",
        domain: Literal["legal", "business", "general"] = "legal",
        provider: Literal["kimi", "deepseek", "siliconflow"] | None = None,
        model: str | None = None,
        task_id: str | None = None,
        glossary: dict[str, str] | None = None,
    ) -> dict[str, Any]:
        """分块翻译长文本；支持自定义术语表及通过 task_id 断点续传。"""
        return _checked(
            await service.handle_translate(text, domain, provider, model, task_id, glossary)
        )

    @mcp.tool(annotations=READ_ONLY)
    async def translate_status(task_id: str) -> dict[str, Any]:
        """查询长文本翻译任务的进度和结果状态。"""
        return _checked(await service.handle_translate_status(task_id))

    @mcp.tool(annotations=LOCAL_WRITE)
    async def translate_glossary(
        action: Literal["list", "add", "delete"],
        source_term: str = "",
        target_term: str = "",
        domain: str = "general",
    ) -> dict[str, Any]:
        """列出、添加或删除本地翻译术语。"""
        return _checked(
            await service.handle_translate_glossary(action, source_term, target_term, domain)
        )

    return mcp
