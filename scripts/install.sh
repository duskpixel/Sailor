#!/bin/bash
# 构建并将 Sailor.app 安装到 /Applications（无权限时回退 ~/Applications），然后启动。
set -euo pipefail
cd "$(dirname "$0")/.."

APP_SRC="build/bin/Sailor.app"
APP_DST="/Applications/Sailor.app"

if [ ! -d "$APP_SRC" ]; then
    echo "==> 未找到构建产物，先执行构建"
    ./scripts/build.sh
fi

echo "==> 结束正在运行的 Sailor"
pkill -f "Sailor.app/Contents/MacOS/Sailor" 2>/dev/null || true
sleep 1

echo "==> 安装到 $APP_DST"
rm -rf "$APP_DST"
if ! ditto "$APP_SRC" "$APP_DST" 2>/dev/null; then
    APP_DST="$HOME/Applications/Sailor.app"
    echo "==> /Applications 无写入权限，改用 $APP_DST"
    rm -rf "$APP_DST"
    ditto "$APP_SRC" "$APP_DST"
fi

echo "==> 启动 $APP_DST"
open "$APP_DST"
echo "==> 完成：已安装并启动"
