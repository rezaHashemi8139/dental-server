<#
.SYNOPSIS
  Runs the server in development mode: settings from .env, plain HTTP/2 (h2c), no build artifacts.

.EXAMPLE
  .\scripts\dev.ps1             # run the server
  .\scripts\dev.ps1 -Generate   # regenerate gen/ from proto/ first
#>
param(
    # Lint the protos and regenerate gen/ before starting.
    [switch]$Generate
)

Push-Location (Join-Path $PSScriptRoot '..')
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
        [IO.File]::WriteAllLines((Join-Path (Get-Location) '.env'), [string[]]$content)
        Write-Warning '.env was missing: created it from .env.example with new random keys. Set DATABASE_URL, then run this script again.'
        exit 1
    }

    if ($Generate) {
        go generate ./...
        if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
    }

    go run .
    exit $LASTEXITCODE
}
finally {
    Pop-Location
}
