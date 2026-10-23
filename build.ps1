# 生成可以直接双击使用的 Windows 64 位程序，不需要安装 Go 运行时。
$ErrorActionPreference = 'Stop'
$taskOldGOOS = $env:GOOS
$taskOldGOARCH = $env:GOARCH
$taskOldCGO = $env:CGO_ENABLED
Push-Location $PSScriptRoot
try {
    $env:GOOS = 'windows'
    $env:GOARCH = 'amd64'
    $env:CGO_ENABLED = '0'
    if (-not (Test-Path -LiteralPath 'assets\app.ico')) {
        & '.\assets\generate-icon.ps1'
    }
    go generate .
    if ($LASTEXITCODE -ne 0) { throw '生成 Windows 资源失败' }
    go test ./...
    if ($LASTEXITCODE -ne 0) { throw '测试未通过' }
    go vet ./...
    if ($LASTEXITCODE -ne 0) { throw '静态检查未通过' }
    go build -trimpath -ldflags '-H=windowsgui -s -w' -o '按键发言工具.exe' .
    if ($LASTEXITCODE -ne 0) { throw '编译失败' }
    Write-Output (Join-Path $PSScriptRoot '按键发言工具.exe')
} finally {
    $env:GOOS = $taskOldGOOS
    $env:GOARCH = $taskOldGOARCH
    $env:CGO_ENABLED = $taskOldCGO
    Pop-Location
}
