$ErrorActionPreference = "Stop"

$root = Split-Path -Parent $PSScriptRoot
$dist = Join-Path $root "dist"
$go = Join-Path $env:LOCALAPPDATA "go\bin\go.exe"
if (-not (Test-Path -LiteralPath $go)) {
    $goCommand = Get-Command go.exe -ErrorAction Stop
    $go = $goCommand.Source
}

foreach ($arch in @("amd64", "arm64")) {
    $target = Join-Path $dist ("release-" + $arch)
    New-Item -ItemType Directory -Path (Join-Path $target "deploy") -Force | Out-Null
    New-Item -ItemType Directory -Path (Join-Path $target "docs") -Force | Out-Null
    Copy-Item -LiteralPath (Join-Path $root "README-go.md") -Destination $target -Force
    Copy-Item -LiteralPath (Join-Path $root "config.example.yaml") -Destination $target -Force
    Copy-Item -Path (Join-Path $root "deploy\*") -Destination (Join-Path $target "deploy") -Recurse -Force
    Copy-Item -Path (Join-Path $root "docs\turnstile-token-flow.md"), (Join-Path $root "docs\go-config.example.yaml") -Destination (Join-Path $target "docs") -Force

    $env:CGO_ENABLED = "0"
    $env:GOOS = "linux"
    $env:GOARCH = $arch
    & $go build -buildvcs=false -trimpath -ldflags="-s -w" -o (Join-Path $target "chatgpt2api") (Join-Path $root "cmd\chatgpt2api")
    if ($LASTEXITCODE -ne 0) {
        throw "Linux $arch build failed"
    }
}

& python (Join-Path $root "scripts\package_go_linux.py")
if ($LASTEXITCODE -ne 0) {
    throw "Linux archive packaging failed"
}
