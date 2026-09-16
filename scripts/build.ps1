# Sailor - Windows 构建脚本（在 PowerShell 中运行）
#
# 前置要求：
#   - Go 1.24+、Node 18+
#   - Wails CLI：go install github.com/wailsapp/wails/v2/cmd/wails@latest
#   - WebView2 Runtime（Win11 自带；Win10 缺失时 Wails 启动会提示安装）
#
# 用法：
#   powershell -ExecutionPolicy Bypass -File scripts\build.ps1
#   可附加 wails 参数，例如：.\scripts\build.ps1 -debug
$ErrorActionPreference = "Stop"
Set-Location (Join-Path $PSScriptRoot "..")

# 定位 Wails CLI：优先 PATH，其次 %USERPROFILE%\go\bin\wails.exe
$wails = Get-Command wails -ErrorAction SilentlyContinue
if ($wails) {
    $wails = $wails.Source
} else {
    $candidate = Join-Path $env:USERPROFILE "go\bin\wails.exe"
    if (Test-Path $candidate) {
        $wails = $candidate
    } else {
        Write-Host "==> 未找到 wails CLI，请先执行: go install github.com/wailsapp/wails/v2/cmd/wails@latest"
        exit 1
    }
}

Write-Host "==> 使用 Wails CLI: $wails"
Write-Host "==> 构建前端静态资源"
npm run build:ui

Write-Host "==> wails build"
& $wails build @args
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }

Write-Host "==> 完成: build\bin\Sailor.exe"
