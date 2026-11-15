<#
.SYNOPSIS
  Runs the server in development mode: settings from .env, plain HTTP/2 (h2c).
  Watches the source and rebuilds + restarts the server on every change.

.DESCRIPTION
  Watched: *.go, *.sql, *.html and *.json (embedded language files) in the project,
  *.proto in proto/, go.mod, go.sum and .env.
  A changed .proto first runs `go generate` (lint + regenerate gen/).
  If the build fails, the previous server keeps running until the next change.
  Stop with Ctrl+C.

.EXAMPLE
  .\scripts\dev.ps1             # run and restart on changes
  .\scripts\dev.ps1 -Generate   # regenerate gen/ from proto/ first
  .\scripts\dev.ps1 -NoWatch    # run once (go run .), no restarts
#>
param(
    # Lint the protos and regenerate gen/ before starting.
    [switch]$Generate,
    # Run once without watching for changes.
    [switch]$NoWatch
)

$root = (Resolve-Path (Join-Path $PSScriptRoot '..')).Path
$exe = Join-Path $root 'bin\dev\dental-server.exe'
$next = Join-Path $root 'bin\dev\dental-server.next.exe'
$server = $null
$exitReported = $false

function Write-Status([string]$Text, [string]$Color = 'Cyan') {
    Write-Host "[dev $(Get-Date -Format HH:mm:ss)] $Text" -ForegroundColor $Color
}

# A fingerprint of the watched files (path + last write time): it changes whenever one is saved, added or removed.
function Get-Fingerprint([string[]]$Dirs, [string[]]$Exts, [string[]]$Names = @()) {
    $files = @(Get-ChildItem $root -File | Where-Object { $Exts -contains $_.Extension -or $Names -contains $_.Name })
    $files += @(Get-ChildItem ($Dirs | ForEach-Object { Join-Path $root $_ }) -Recurse -File -ErrorAction SilentlyContinue |
        Where-Object { $Exts -contains $_.Extension })
    ($files | Sort-Object FullName | ForEach-Object { "$($_.FullName)|$($_.LastWriteTimeUtc.Ticks)" }) -join "`n"
}
# .json: the language files in internal/languages/files are embedded in the binary.
function Get-SourceFingerprint { Get-Fingerprint -Dirs 'internal', 'gen' -Exts '.go', '.sql', '.html', '.json' -Names 'go.mod', 'go.sum', '.env' }
function Get-ProtoFingerprint { Get-Fingerprint -Dirs 'proto' -Exts '.proto' }

function Stop-Server {
    if ($script:server -and -not $script:server.HasExited) {
        Stop-Process -Id $script:server.Id -Force -ErrorAction SilentlyContinue
        $script:server.WaitForExit()
    }
}

# Builds into a side file first, so a failed build leaves the running server untouched.
function Update-Server {
    go build -o $next .
    if ($LASTEXITCODE -ne 0) {
        if ($script:server -and -not $script:server.HasExited) {
            Write-Status 'Build failed; the previous server keeps running. Fix the error and save again.' 'Red'
        } else {
            Write-Status 'Build failed. Fix the error and save again.' 'Red'
        }
        return
    }
    Stop-Server
    Move-Item -Force $next $exe
    $script:server = Start-Process -FilePath $exe -WorkingDirectory $root -NoNewWindow -PassThru
    $null = $script:server.Handle   # keeps the handle so ExitCode is readable after the process ends
    $script:exitReported = $false
}

Push-Location $root
try {
    if (-not (Test-Path .env)) {
        # Fill the empty keys with fresh random ones; the rest (DATABASE_URL) still needs editing.
        $rng = [Security.Cryptography.RandomNumberGenerator]::Create()
        $content = (Get-Content .env.example) | ForEach-Object {
            if ($_ -match '^(KEY_ENCRYPTION_KEY|SIGNING_KEY)=$') {
                $bytes = New-Object byte[] 32
                $rng.GetBytes($bytes)
                $_ + [Convert]::ToBase64String($bytes)
            } else { $_ }
        }
        [IO.File]::WriteAllLines((Join-Path $root '.env'), [string[]]$content)
        Write-Warning '.env was missing: created it from .env.example with new random keys. Set DATABASE_URL, then run this script again.'
        exit 1
    }

    if ($Generate) {
        go generate ./...
        if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
    }

    if ($NoWatch) {
        go run .
        exit $LASTEXITCODE
    }

    New-Item -ItemType Directory -Force (Split-Path $exe) | Out-Null
    $sources = Get-SourceFingerprint
    $protos = Get-ProtoFingerprint
    Write-Status 'Watching for changes. Press Ctrl+C to stop.'
    Update-Server

    while ($true) {
        Start-Sleep -Milliseconds 500

        if ($server -and $server.HasExited -and -not $exitReported) {
            Write-Status "Server exited with code $($server.ExitCode). Waiting for changes..." 'Yellow'
            $exitReported = $true
        }

        $nowSources = Get-SourceFingerprint
        $nowProtos = Get-ProtoFingerprint
        if ($nowSources -eq $sources -and $nowProtos -eq $protos) { continue }

        # Editors often write several files in a burst: wait until things settle.
        $protosBefore = $protos
        do {
            $sources = $nowSources
            $protos = $nowProtos
            Start-Sleep -Milliseconds 300
            $nowSources = Get-SourceFingerprint
            $nowProtos = Get-ProtoFingerprint
        } while ($nowSources -ne $sources -or $nowProtos -ne $protos)

        if ($protos -ne $protosBefore) {
            Write-Status 'Proto changed: running go generate...'
            go generate ./...
            $sources = Get-SourceFingerprint   # gen/ was rewritten by us; not a new change
            if ($LASTEXITCODE -ne 0) {
                Write-Status 'go generate failed; the previous server keeps running.' 'Red'
                continue
            }
        }

        Write-Status 'Change detected: rebuilding...'
        Update-Server
    }
}
finally {
    Stop-Server
    Pop-Location
}
