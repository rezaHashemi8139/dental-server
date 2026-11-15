<#
.SYNOPSIS
  Builds a production binary into bin/: tests first, then a static, stripped build with the version embedded.

.EXAMPLE
  .\scripts\build.ps1                    # for this machine's OS
  .\scripts\build.ps1 -OS linux          # cross-compile for a Linux server
  .\scripts\build.ps1 -Version 1.2.0     # set the version instead of taking it from git
#>
param(
    # Target OS (GOOS). Defaults to this machine's.
    [ValidateSet('windows', 'linux', 'darwin')]
    [string]$OS = (go env GOOS),
    # Target architecture (GOARCH).
    [ValidateSet('amd64', 'arm64')]
    [string]$Arch = 'amd64',
    # Version reported by the server. Defaults to `git describe`.
    [string]$Version,
    # Skip go vet and go test.
    [switch]$SkipTests
)

Push-Location (Join-Path $PSScriptRoot '..')
$saved = @{ CGO_ENABLED = $env:CGO_ENABLED; GOOS = $env:GOOS; GOARCH = $env:GOARCH }
try {
    if (-not $SkipTests) {
        go vet ./...
        if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
        go test ./...
        if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
    }

    if (-not $Version) {
        $Version = git describe --tags --always --dirty 2>$null
        if ($LASTEXITCODE -ne 0 -or -not $Version) { $Version = 'dev' }
    }

    $out = 'bin/dental-server'
    if ($OS -eq 'windows') { $out += '.exe' }

    # Static binary: no C toolchain needed on the build machine or libc on the server.
    $env:CGO_ENABLED = '0'
    $env:GOOS = $OS
    $env:GOARCH = $Arch
    go build -trimpath -ldflags "-s -w -X main.version=$Version" -o $out .
    if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }

    Write-Host "Built $out ($OS/$Arch, version $Version)"
}
finally {
    foreach ($name in $saved.Keys) {
        if ($null -eq $saved[$name]) { Remove-Item "env:$name" -ErrorAction SilentlyContinue }
        else { Set-Item "env:$name" $saved[$name] }
    }
    Pop-Location
}
