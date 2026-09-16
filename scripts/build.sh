#!/bin/bash
# Armada 桌面端构建脚本。
#
# 为什么需要这个脚本：macOS 27 SDK 的 .tbd 文件带有旧 clang 不认识的
# arm64e.x1 变体，CommandLineTools 自带的 SDK 会导致链接报
# "unknown architecture"。如果装了完整 Xcode，这里显式指向 Xcode 的 SDK。
set -euo pipefail
cd "$(dirname "$0")/.."

WAILS="${WAILS:-$HOME/go/bin/wails}"
command -v wails >/dev/null 2>&1 && WAILS="$(command -v wails)"

if [ "$(uname)" = "Darwin" ] && [ -d "/Applications/Xcode.app" ]; then
    XCODE_SDK="$(ls -d /Applications/Xcode.app/Contents/Developer/Platforms/MacOSX.platform/Developer/SDKs/MacOSX*.sdk 2>/dev/null | sort -V | tail -1)"
    if [ -n "${XCODE_SDK:-}" ]; then
        export SDKROOT="$XCODE_SDK"
        echo "==> 使用 Xcode SDK: $SDKROOT"
    fi
fi

echo "==> 构建前端静态资源"
npm run build:ui

echo "==> wails build"
"$WAILS" build "$@"

echo "==> 完成: build/bin/Sailor.app"
