"""
WordBridge 启动脚本。

用法：
  python run_bridge.py              # 启动
  python run_bridge.py --debug      # Debug 模式
  python run_bridge.py --no-hotkey  # 不注册热键
  python run_bridge.py --stdio      # MCP stdio 模式（MindLink 托管）

依赖安装：
  pip install -r requirements.txt

环境变量（可选）：
  WORDBRIDGE_PYTHON_SITE   干净的 site-packages 路径，规避 宿主 venv 污染
  DEEPSEEK_API_KEY         翻译引擎密钥（也可写在项目目录 .env）
"""

import os
import sys


def _load_dotenv(path: str) -> None:
    """从项目目录 .env 加载环境变量（不覆盖已存在的变量）。"""
    if not os.path.exists(path):
        return
    with open(path, encoding="utf-8") as f:
        for line in f:
            line = line.strip()
            if not line or line.startswith("#") or "=" not in line:
                continue
            key, value = line.split("=", 1)
            os.environ.setdefault(key.strip(), value.strip())


# 项目目录 = 本文件所在目录（可迁移到任意路径）
project_dir = os.path.dirname(os.path.abspath(__file__))

# 加载项目 .env（DEEPSEEK_API_KEY 等），stdio / HTTP 两种模式都生效
_load_dotenv(os.path.join(project_dir, ".env"))

# 可选：若被 宿主 venv 的 broken openai/pydantic 污染，用环境变量指定
# 一个干净的 site-packages 优先加载（默认不注入）。
_extra_site = os.environ.get("WORDBRIDGE_PYTHON_SITE", "").strip()
if _extra_site and _extra_site not in sys.path:
    sys.path.insert(0, _extra_site)

# 确保项目目录在 Python path 中
if project_dir not in sys.path:
    sys.path.insert(0, project_dir)

from wordbridge.main import main

if __name__ == "__main__":
    main()
