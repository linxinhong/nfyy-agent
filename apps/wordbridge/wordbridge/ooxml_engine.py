"""
OOXML 格式引擎
────────────────────────────────────────

负责 Word OOXML 的解析与格式保留重建。

策略层级（从最安全到降级）：
  Level 1 — node_replace:   仅替换 <w:t> 文本节点，不触碰任何格式标签
  Level 2 — para_rebuild:   段落级重建，保留段落属性+字符格式模板
  Level 3 — plain_fallback: 纯文本降级（最后手段）

设计约束：
  - 所有 XML 操作必须是防御性的，解析失败立即降级
  - 不对任意 XML 做结构性假设
  - 自动编号（w:numPr）绝对不应丢失
  - 修订模式（w:ins/w:del）中的文本不参与替换
"""

from __future__ import annotations

import re
import time
from dataclasses import dataclass, field
from typing import Optional
from xml.etree import ElementTree as ET
from xml.sax.saxutils import escape as xml_escape  # noqa: F401 — re-exported

try:
    from pywintypes import com_error  # type: ignore
except ImportError:  # macOS: 无 pywin32,com_error 退化为普通异常
    com_error = Exception  # type: ignore

from .config import OoxmlConfig, get_config
from .utils import get_logger

logger = get_logger("ooxml")

# ── Word OOXML 命名空间 ──
NS_W = "http://schemas.openxmlformats.org/wordprocessingml/2006/main"
NS_R = "http://schemas.openxmlformats.org/officeDocument/2006/relationships"
NS_M = "http://schemas.openxmlformats.org/officeDocument/2006/math"

# 所有 OOXML 命名空间映射
OOXML_NS = {"w": NS_W, "r": NS_R, "m": NS_M}

# ── XPath helpers ──


def _tag(local: str) -> str:
    """生成完整的 w 命名空间标签名。"""
    return f"{{{NS_W}}}{local}"


# ── 数据类 ──

@dataclass
class RunInfo:
    """单个 Run（文字运行）的信息。"""
    text: str = ""
    rpr_xml: str = ""        # <w:rPr>...</w:rPr> XML 字符串


@dataclass
class ParaInfo:
    """单个段落的解析信息。"""
    ppr_xml: str = ""        # <w:pPr>...</w:pPr> XML 字符串
    runs: list[RunInfo] = field(default_factory=list)
    raw_p_xml: str = ""      # 原始 <w:p> XML（降级用）


@dataclass
class OoxmlParseResult:
    """OOXML 解析结果。"""
    paragraphs: list[ParaInfo] = field(default_factory=list)
    parse_level: str = "node_replace"  # 实际使用的解析层级
    warnings: list[str] = field(default_factory=list)
    raw_xml: str = ""  # 原始 XML（降级备用）


# ══════════════════════════════════════════════
# 格式保留替换引擎
# ══════════════════════════════════════════════

class OoxmlEngine:
    """
    OOXML 格式保留引擎。

    使用方式：
        engine = OoxmlEngine()
        new_ooxml = engine.transform(original_ooxml, new_plain_text)
    """

    def __init__(self, config: Optional[OoxmlConfig] = None):
        self.config = config or get_config().ooxml
        self.strategies = self.config.strategy_order

    # ── 公共入口 ──

    def transform(
        self,
        original_ooxml: str,
        new_text: str,
        force_strategy: Optional[str] = None,
    ) -> str:
        """
        将 new_text 注入 original_ooxml 的格式骨架中。

        参数：
          original_ooxml: Word 选区导出的 OOXML
          new_text:       AI 处理后的纯文本
          force_strategy: 强制使用指定策略（测试用）

        返回：
          可直接用于 Range.InsertXML() 的 OOXML

        异常：
          此方法本身不抛异常 — 所有错误降级为 plain_fallback
        """
        start_time = time.time()

        if not original_ooxml or not original_ooxml.strip():
            return self._plain_fallback(new_text)

        # 确保 OOXML 被正确包裹在根元素中
        ooxml = self._ensure_body_wrap(original_ooxml)

        strategies = [force_strategy] if force_strategy else self.strategies

        for strategy in strategies:
            try:
                if strategy == "node_replace":
                    result = self._node_replace(ooxml, new_text)
                    if result:
                        elapsed = (time.time() - start_time) * 1000
                        logger.debug("node_replace 成功 (%.1fms)", elapsed)
                        return result

                elif strategy == "para_rebuild":
                    result = self._para_rebuild(ooxml, new_text)
                    if result:
                        elapsed = (time.time() - start_time) * 1000
                        logger.debug("para_rebuild 成功 (%.1fms)", elapsed)
                        return result

            except ET.ParseError as e:
                logger.warning("%s: XML 解析失败 — %s", strategy, e)
            except Exception as e:
                logger.warning("%s: 处理异常 — %s", strategy, e)

        # 最终降级
        elapsed = (time.time() - start_time) * 1000
        logger.warning("所有策略失败，降级为 plain_fallback (%.1fms)", elapsed)
        return self._plain_fallback(new_text, original_ooxml)

    # ── 策略 1: 文本节点替换 ──

    def _node_replace(self, ooxml: str, new_text: str) -> Optional[str]:
        """
        仅替换 <w:t> 节点的文本内容，不修改任何格式标签。

        前提条件：
          - 段落数量不变
          - 每段内的 Run 数量不变
          - 仅文本内容变化

        这能保留最完整的格式（每个 Run 的独立字体、加粗、斜体等）。

        流程：
          1. 收集所有 <w:t> 节点按顺序列表
          2. 合并原文本 → 计算每个 run 的长度占比
          3. 将新文本按占比分配回各 <w:t> 节点
        """
        try:
            root = ET.fromstring(ooxml)
        except ET.ParseError:
            return None

        # 收集所有 <w:t> 节点
        t_tag = _tag("t")
        t_nodes = root.findall(f".//{t_tag}")

        if not t_nodes:
            logger.debug("node_replace: 未找到 <w:t> 节点")
            return None

        # 特殊处理：单节点直接替换
        if len(t_nodes) == 1:
            t_nodes[0].text = new_text
            return ET.tostring(root, encoding="unicode")

        # 多节点：按原长度比例分配新文本
        original_texts = [n.text or "" for n in t_nodes]
        total_len = sum(len(t) for t in original_texts)

        if total_len == 0:
            # 所有节点都为空，全部填入新文本
            t_nodes[0].text = new_text
            for n in t_nodes[1:]:
                n.text = ""
            return ET.tostring(root, encoding="unicode")

        pos = 0
        remaining_len = len(new_text)

        for i, node in enumerate(t_nodes):
            if i == len(t_nodes) - 1:
                # 最后一个节点：放剩余全部文本
                node.text = new_text[pos:]
            else:
                orig_len = len(original_texts[i])
                # 按原始长度比例分配
                alloc = int(len(new_text) * orig_len / total_len)
                alloc = max(0, min(alloc, remaining_len))
                node.text = new_text[pos:pos + alloc]
                pos += alloc
                remaining_len -= alloc

        return ET.tostring(root, encoding="unicode")

    # ── 策略 2: 段落级重建 ──

    def _para_rebuild(self, ooxml: str, new_text: str) -> Optional[str]:
        """
        提取第一段的段落属性（<w:pPr>）和第一跑的字符属性（<w:rPr>）
        作为模板，按新文本的段落结构重建 OOXML。

        保留：
          - 段落缩进、对齐方式
          - 行距、段前段后间距
          - 自动编号（<w:numPr>）
          - 段落样式引用（<w:pStyle>）
          - 字符字体、大小、加粗、颜色

        局限性：
          - 同一段内不同 Run 的差异化格式会丢失（统一为第一跑的格式）
          - 各级编号的独立缩进会丢失（统一为第一段的缩进）
        """
        try:
            root = ET.fromstring(ooxml)
        except ET.ParseError:
            return None

        w_p = _tag("p")
        w_pPr = _tag("pPr")
        w_r = _tag("r")
        w_rPr = _tag("rPr")

        # 找到第一个 <w:p>
        first_p = root.find(f".//{w_p}")
        if first_p is None:
            # 没有段落结构，回退到 plain
            return None

        # 提取段落属性模板
        ppr = first_p.find(w_pPr)
        ppr_xml = ET.tostring(ppr, encoding="unicode") if ppr is not None else ""

        # 提取字符属性模板
        first_run = first_p.find(w_r)
        rpr_xml = ""
        if first_run is not None:
            rpr_el = first_run.find(w_rPr)
            if rpr_el is not None:
                rpr_xml = ET.tostring(rpr_el, encoding="unicode")

        # 按新文本的换行切分段落
        new_paras = new_text.split('\n')

        # 构建新 OOXML
        paras_xml = []
        for para_text in new_paras:
            safe_text = xml_escape(para_text)
            para_xml = (
                f'<w:p xmlns:w="{NS_W}">'
                f'{ppr_xml}'
                f'<w:r>'
                f'{rpr_xml}'
                f'<w:t xml:space="preserve">{safe_text}</w:t>'
                f'</w:r>'
                f'</w:p>'
            )
            paras_xml.append(para_xml)

        return "".join(paras_xml)

    # ── 策略 3: 纯文本降级 ──

    def _plain_fallback(self, new_text: str, original_ooxml: Optional[str] = None) -> str:
        """
        最终降级：生成最基本的段落格式 OOXML。

        如果 original_ooxml 可用，仍然尝试提取段落属性。
        """
        # 尝试从原始 OOXML 提取段落属性
        ppr_xml = ""
        if original_ooxml:
            try:
                root = ET.fromstring(original_ooxml)
                first_p = root.find(f".//{_tag('p')}")
                if first_p is not None:
                    ppr = first_p.find(_tag("pPr"))
                    if ppr is not None:
                        ppr_xml = ET.tostring(ppr, encoding="unicode")
            except Exception:
                pass

        paras = new_text.split('\n')
        paras_xml = []
        for para_text in paras:
            safe_text = xml_escape(para_text)
            if ppr_xml:
                para_xml = (
                    f'<w:p xmlns:w="{NS_W}">'
                    f'{ppr_xml}'
                    f'<w:r><w:t xml:space="preserve">{safe_text}</w:t></w:r>'
                    f'</w:p>'
                )
            else:
                para_xml = (
                    f'<w:p xmlns:w="{NS_W}">'
                    f'<w:r><w:t xml:space="preserve">{safe_text}</w:t></w:r>'
                    f'</w:p>'
                )
            paras_xml.append(para_xml)

        return "".join(paras_xml)

    # ── 辅助方法 ──

    def _ensure_body_wrap(self, ooxml: str) -> str:
        """确保 OOXML 有根元素包裹。"""
        stripped = ooxml.strip()
        if stripped.startswith('<w:'):
            return f'<w:root xmlns:w="{NS_W}">{stripped}</w:root>'
        if stripped.startswith('<?xml'):
            return ooxml
        if not stripped.startswith('<'):
            return self._plain_fallback(ooxml)
        return ooxml

    # ── 公共工具方法 ──

    def extract_plain_text(self, ooxml: str) -> str:
        """从 OOXML 中提取纯文本（调试用）。"""
        try:
            root = ET.fromstring(self._ensure_body_wrap(ooxml))
            t_nodes = root.findall(f".//{_tag('t')}")
            return "".join(n.text or "" for n in t_nodes)
        except Exception:
            return ""

    def has_list_numbering(self, ooxml: str) -> bool:
        """检查 OOXML 是否包含列表编号。"""
        try:
            root = ET.fromstring(self._ensure_body_wrap(ooxml))
            return root.find(f".//{_tag('numPr')}") is not None
        except Exception:
            return False


# ══════════════════════════════════════════════
# 带 COM 重试的 OOXML 获取（供 ComWorker 使用）
# ══════════════════════════════════════════════

def safe_get_ooxml(rng, max_retries: int = 3, base_delay_ms: int = 200) -> str:
    """
    从 Range 对象安全获取 OOXML，带 COM 错误重试。

    这是在 COM STA 线程内调用的函数。

    参数：
      rng: Word Range COM 对象
      max_retries: 最大重试次数
      base_delay_ms: 初始等待时间

    返回：
      OOXML 字符串，失败时返回空字符串
    """
    for attempt in range(max_retries):
        try:
            return str(rng.XML)
        except com_error as e:
            if e.hresult == -2147418111:  # RPC_E_CALL_REJECTED
                delay = base_delay_ms * (2 ** attempt) / 1000.0
                logger.debug(
                    "safe_get_ooxml: RPC_E_CALL_REJECTED, 重试 %d/%d, 等待 %.1fs",
                    attempt + 1, max_retries, delay
                )
                time.sleep(delay)
            else:
                logger.error("safe_get_ooxml COM 错误: %s", e)
                return ""
        except Exception as e:
            logger.error("safe_get_ooxml 异常: %s", e)
            return ""

    logger.error("safe_get_ooxml: 重试耗尽，返回空字符串")
    return ""
