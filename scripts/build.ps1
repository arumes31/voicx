param(
    [Parameter(Position = 0)]
    [ValidateSet("server", "client", "version", "check")]
    [string]$Target = "server",

    [string]$UpdateRepo = "voicx/voicx",
    [string]$UpdatePublicKeys = ""
)

$ErrorActionPreference = "Stop"
$projectRoot = Split-Path -Parent $PSScriptRoot
Push-Location $projectRoot

try {
    if ($Target -eq "version") {
        & go run ./cmd/version
        if ($LASTEXITCODE -ne 0) { throw "version calculation failed" }
        exit 0
    }
    if ($Target -eq "check") {
        & go run ./cmd/version -check
        if ($LASTEXITCODE -ne 0) { throw "version check failed" }
        exit 0
    }
    if ($Target -eq "client" -and -not (Get-Command wails -ErrorAction SilentlyContinue)) {
        throw "Wails CLI is required for a client build. Install it with: go install github.com/wailsapp/wails/v2/cmd/wails@v2.13.0"
    }

    $versionFlags = (& go run ./cmd/version -format ldflags)
    if ($LASTEXITCODE -ne 0) { throw "version calculation failed" }
    $linkerFlags = "-s -w $versionFlags -X=voicx/internal/version.UpdateRepo=$UpdateRepo"
    if ($UpdatePublicKeys) {
        $linkerFlags += " -X=voicx/internal/version.UpdatePublicKeys=$UpdatePublicKeys"
    }

    if ($Target -eq "server") {
        New-Item -ItemType Directory -Force -Path "bin" | Out-Null
        & go build -trimpath -ldflags $linkerFlags -o bin/voicx-server.exe ./cmd/server
        if ($LASTEXITCODE -ne 0) { throw "server build failed" }
        exit 0
    }

    Push-Location "client"
    try {
        & wails build -trimpath -ldflags $linkerFlags
        if ($LASTEXITCODE -ne 0) { throw "client build failed" }
    }
    finally {
        Pop-Location
    }
}
finally {
    Pop-Location
}
