"""
WordBridge — Hermes ↔ Microsoft Word 后台桥接服务

零 UI 痕迹的 Word 集成方案：
  - COM Automation 后台直连 Word 进程
  - 全局热键触发选区捕获
  - OOXML 级别格式保留
  - 光标/滚动完全解耦
"""

__version__ = "1.0.0"
__author__ = "WordBridge"
