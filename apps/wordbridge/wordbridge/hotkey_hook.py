"""
全局热键钩子模块
────────────────────────────────────────

注册系统级热键，在用户按下 Ctrl+Shift+H 时触发选区捕获。

技术选型：
  方案 A: win32api.RegisterHotKey + Windows 消息循环
    → 最底层，最可靠，不依赖任何第三方键盘库
    → 需要独立的消息窗口（隐藏）

  方案 B: keyboard 库
    → 更简洁的 API，但依赖底层 hook，可能被杀毒软件拦截

  默认使用方案 A，因为它不注入 DLL，不会触发安全软件的警觉。

流程：
  Ctrl+Shift+H → WM_HOTKEY → on_hotkey() → ComWorker.capture() → WebSocket 推送
"""

from __future__ import annotations

import ctypes
import ctypes.wintypes
import threading
from typing import Callable, Optional

from .utils import get_logger

logger = get_logger("hotkey")

# ── Windows API 常量 ──
try:
    import win32api
    import win32con
    import win32gui
    HAS_WIN32 = True
except ImportError:
    HAS_WIN32 = False
    logger.warning("pywin32 不可用，热键功能将受限")

try:
    import keyboard
    HAS_KEYBOARD = True
except ImportError:
    HAS_KEYBOARD = False

# 热键虚拟键码映射
VK_MAP = {
    "A": 0x41, "B": 0x42, "C": 0x43, "D": 0x44, "E": 0x45,
    "F": 0x46, "G": 0x47, "H": 0x48, "I": 0x49, "J": 0x4A,
    "K": 0x4B, "L": 0x4C, "M": 0x4D, "N": 0x4E, "O": 0x4F,
    "P": 0x50, "Q": 0x51, "R": 0x52, "S": 0x53, "T": 0x54,
    "U": 0x55, "V": 0x56, "W": 0x57, "X": 0x58, "Y": 0x59,
    "Z": 0x5A,
    "0": 0x30, "1": 0x31, "2": 0x32, "3": 0x33, "4": 0x34,
    "5": 0x35, "6": 0x36, "7": 0x37, "8": 0x38, "9": 0x39,
    "F1": 0x70, "F2": 0x71, "F3": 0x72, "F4": 0x73,
    "F5": 0x74, "F6": 0x75, "F7": 0x76, "F8": 0x77,
    "F9": 0x78, "F10": 0x79, "F11": 0x7A, "F12": 0x7B,
    "Space": 0x20, "Tab": 0x09, "Enter": 0x0D,
    "Escape": 0x1B, "Backspace": 0x08,
    "Up": 0x26, "Down": 0x28, "Left": 0x25, "Right": 0x27,
}


def _parse_modifiers(modifiers: list[str]) -> int:
    """将修饰键列表转换为 Windows 修饰键位掩码。"""
    mod = 0
    for m in modifiers:
        m_lower = m.lower()
        if m_lower in ("ctrl", "control"):
            mod |= win32con.MOD_CONTROL
        elif m_lower == "shift":
            mod |= win32con.MOD_SHIFT
        elif m_lower == "alt":
            mod |= win32con.MOD_ALT
        elif m_lower in ("win", "windows", "cmd"):
            mod |= win32con.MOD_WIN
    return mod


def _parse_key(key: str) -> int:
    """将键名转换为虚拟键码。"""
    upper = key.upper()
    if upper in VK_MAP:
        return VK_MAP[upper]
    # 如果是十六进制字符串
    if key.lower().startswith("0x"):
        return int(key, 16)
    # 如果是数字字符串
    if key.isdigit():
        return int(key)
    # 尝试从 win32con 获取
    const_name = f"VK_{upper}"
    if hasattr(win32con, const_name):
        return getattr(win32con, const_name)
    raise ValueError(f"无法解析键名: {key}")


# ══════════════════════════════════════════════
# 方案 A: Windows 消息循环热键
# ══════════════════════════════════════════════

class HotkeyHook:
    """
    Windows 原生热键钩子。

    使用 RegisterHotKey API 注册系统级热键，
    通过隐藏窗口的消息循环接收 WM_HOTKEY 消息。

    线程安全：在独立线程中运行消息循环。
    """

    def __init__(self):
        if not HAS_WIN32:
            raise RuntimeError("需要 pywin32 来使用热键功能")

        self._callback: Optional[Callable[[], None]] = None
        self._hotkey_id: int = 1
        self._hwnd: Optional[int] = None
        self._thread: Optional[threading.Thread] = None
        self._running = False
        self._registered = False

        # 窗口类名
        self._wnd_class = "WordBridgeHotkeyWindow"

    # ── 公共接口 ──

    def register(
        self,
        modifiers: list[str],
        key: str,
        callback: Callable[[], None],
    ) -> bool:
        """
        注册全局热键。

        参数：
          modifiers: 修饰键列表，如 ["Control", "Shift"]
          key:       主键名，如 "H" 或 "0x48"
          callback:  触发时调用的无参回调函数
        """
        if self._running:
            logger.warning("热键已注册，先调用 unregister()")
            return False

        self._callback = callback
        mod_mask = _parse_modifiers(modifiers)
        vk = _parse_key(key)

        self._running = True
        self._thread = threading.Thread(
            target=self._message_loop,
            args=(mod_mask, vk),
            name="WordBridge-Hotkey",
            daemon=True,
        )
        self._thread.start()

        logger.info(
            "热键已注册: %s + %s (MOD=0x%X, VK=0x%X)",
            "+".join(modifiers), key, mod_mask, vk
        )
        return True

    def register_by_vk(
        self,
        mod_mask: int,
        vk: int,
        callback: Callable[[], None],
    ) -> bool:
        """使用裸虚拟键码注册热键。"""
        if self._running:
            logger.warning("热键已注册")
            return False

        self._callback = callback
        self._running = True
        self._thread = threading.Thread(
            target=self._message_loop,
            args=(mod_mask, vk),
            name="WordBridge-Hotkey",
            daemon=True,
        )
        self._thread.start()

        logger.info("热键已注册: MOD=0x%X, VK=0x%X", mod_mask, vk)
        return True

    def unregister(self) -> None:
        """注销热键。"""
        self._running = False

        # 向消息窗口发送退出消息
        if self._hwnd and HAS_WIN32:
            try:
                win32gui.PostMessage(self._hwnd, win32con.WM_QUIT, 0, 0)
            except Exception:
                pass

        if self._thread and self._thread.is_alive():
            self._thread.join(timeout=2.0)

        logger.info("热键已注销")

    @property
    def is_running(self) -> bool:
        return self._running

    # ── 消息循环 ──

    def _message_loop(self, mod_mask: int, vk: int) -> None:
        """隐藏窗口的消息循环。"""
        try:
            # 注册窗口类
            wc = win32gui.WNDCLASS()
            wc.lpfnWndProc = self._wnd_proc
            wc.lpszClassName = self._wnd_class
            wc.hInstance = ctypes.windll.kernel32.GetModuleHandleW(0)

            try:
                win32gui.RegisterClass(wc)
            except Exception:
                # 窗口类可能已注册（重启时）
                pass

            # 创建隐藏窗口
            self._hwnd = win32gui.CreateWindow(
                self._wnd_class,
                "WordBridge Hotkey",
                0, 0, 0, 0, 0,
                0, 0,
                wc.hInstance,
                None,
            )

            # 将 callback 挂到窗口上（通过 GWL_USERDATA）
            # 注意：ctypes 方式存 Python 对象可能导致引用泄漏
            # 这里用全局变量代替（简单可靠）

            # 注册热键
            try:
                win32gui.RegisterHotKey(self._hwnd, self._hotkey_id, mod_mask, vk)
                self._registered = True
                logger.debug("RegisterHotKey 成功: id=%d", self._hotkey_id)
            except Exception as e:
                logger.error("RegisterHotKey 失败: %s", e)
                self._running = False
                return

            # 消息循环
            while self._running:
                # PumpWaitingMessages 在 Python 中即为 GetMessage + DispatchMessage
                # win32gui.PumpWaitingMessages 会阻塞直到 WM_QUIT
                try:
                    got_msg = win32gui.PumpWaitingMessages()
                    if not got_msg:
                        break
                except Exception:
                    break

        except Exception as e:
            logger.error("热键消息循环异常: %s", e)

        finally:
            # 清理
            if self._registered and self._hwnd:
                try:
                    win32gui.UnregisterHotKey(self._hwnd, self._hotkey_id)
                except Exception:
                    pass
                self._registered = False

            if self._hwnd:
                try:
                    win32gui.DestroyWindow(self._hwnd)
                except Exception:
                    pass
                self._hwnd = None

            logger.debug("热键消息循环已退出")

    def _wnd_proc(self, hwnd, msg, wparam, lparam):
        """窗口消息处理。"""
        if msg == win32con.WM_HOTKEY:
            if wparam == self._hotkey_id:
                logger.debug("热键触发")
                if self._callback:
                    try:
                        self._callback()
                    except Exception as e:
                        logger.error("热键回调异常: %s", e)
            return 0

        elif msg == win32con.WM_DESTROY:
            win32gui.PostQuitMessage(0)
            return 0

        return win32gui.DefWindowProc(hwnd, msg, wparam, lparam)


# ══════════════════════════════════════════════
# 方案 B: keyboard 库（备选）
# ══════════════════════════════════════════════

class KeyboardHook:
    """
    基于 keyboard 库的热键钩子（备选方案）。

    当方案 A 有问题时使用。
    """

    def __init__(self):
        if not HAS_KEYBOARD:
            raise RuntimeError("需要安装 keyboard 库: pip install keyboard")

    def register(
        self,
        hotkey_str: str,
        callback: Callable[[], None],
    ) -> bool:
        """
        注册热键。

        参数：
          hotkey_str: 如 "ctrl+shift+h"
          callback:   触发回调
        """
        try:
            keyboard.add_hotkey(hotkey_str, callback)
            logger.info("Keyboard 热键已注册: %s", hotkey_str)
            return True
        except Exception as e:
            logger.error("Keyboard 热键注册失败: %s", e)
            return False

    def unregister(self, hotkey_str: str) -> None:
        """注销热键。"""
        try:
            keyboard.remove_hotkey(hotkey_str)
        except Exception:
            pass


# ══════════════════════════════════════════════
# 工厂函数
# ══════════════════════════════════════════════

def create_hotkey_hook(prefer: str = "win32") -> object:
    """
    创建热键钩子实例。

    参数：
      prefer: "win32" (默认) 或 "keyboard"
    """
    if prefer == "keyboard" and HAS_KEYBOARD:
        return KeyboardHook()
    elif HAS_WIN32:
        return HotkeyHook()
    elif HAS_KEYBOARD:
        logger.warning("win32 方案不可用，降级为 keyboard")
        return KeyboardHook()
    else:
        raise RuntimeError("没有可用的热键方案（pywin32 和 keyboard 都不可用）")
