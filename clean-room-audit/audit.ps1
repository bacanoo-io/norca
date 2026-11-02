# SPDX-FileCopyrightText: 2026 Norca contributors
# SPDX-License-Identifier: MPL-2.0

<#
.SYNOPSIS
  Clean room similarity audit: compares Norca code with HashiCorp code under
  BUSL/BSL using Dolos, in a throwaway container.

.DESCRIPTION
  Run by the auditor role only (see CLEAN_ROOM.md). How the comparison works
  is described in README.md next to this script.

  The container gets this directory (read-only), the output directory and the
  Norca checkout this script belongs to (read-only); it fetches HashiCorp
  Nomad from GitHub, extracts the compared files, runs Dolos and writes only
  file pairs and similarity scores to -OutDir. The HashiCorp code never
  reaches this computer's file system: it exists only inside the container,
  which is removed at the end.

  Needs Docker and PowerShell (Windows PowerShell 5.1 or PowerShell 7).

.EXAMPLE
  .\clean-room-audit\audit.ps1
  .\clean-room-audit\audit.ps1 -Sets bugfix -BslTag v1.10.5 -Threshold 0.3
  .\clean-room-audit\audit.ps1 -NorcaUrl https://github.com/<owner>/norca.git -OutDir out
#>
param(
    # The Norca revision to audit.
    [string]$NorcaRef = 'main',
    # Where Norca is cloned from: the path of a local checkout (mounted
    # read-only), or a URL. Default: the checkout this script belongs to.
    [string]$NorcaUrl = (Split-Path $PSScriptRoot -Parent),
    # A Nomad tag, or 'latest' for the newest v1.x.y release tag.
    [string]$BslTag = 'latest',
    [string]$Sets = 'busl,bugfix',
    [double]$Threshold = 0.4,
    # Default: clean-room-result\ of the checkout this script belongs to.
    [string]$OutDir = '',
    [string]$DolosVersion = '2.10.0',
    [string]$Image = 'node:24-bookworm'
)
$ErrorActionPreference = 'Stop'

$Here = $PSScriptRoot
if (-not $OutDir) {
    $OutDir = Join-Path (Split-Path $Here -Parent) 'clean-room-result'
}
New-Item -ItemType Directory -Force $OutDir | Out-Null
$OutDir = (Resolve-Path $OutDir).Path

function DockerPath([string]$Path) { $Path -replace '\\', '/' }

# A local checkout is mounted read-only and cloned from inside the container.
if (Test-Path -LiteralPath $NorcaUrl -PathType Container) {
    $Source = (Resolve-Path -LiteralPath $NorcaUrl).Path
    $SourceArgs = @('-v', "$(DockerPath $Source):/norca-src:ro", '-e', 'NORCA_URL=/norca-src')
    $Where = "the local checkout $Source"
} else {
    $SourceArgs = @('-e', "NORCA_URL=$NorcaUrl")
    $Where = $NorcaUrl
}

Write-Host "==> Clean room audit of $NorcaRef ($Where) against Nomad $BslTag (sets: $Sets) into $OutDir" -ForegroundColor Cyan
# run.sh logs its progress to stderr. Windows PowerShell 5.1 turns every
# stderr line of a native command into an error record, which is fatal under
# ErrorActionPreference 'Stop'. So the docker call runs with 'Continue' and
# its output is converted to plain text; only the exit code decides.
$ErrorActionPreference = 'Continue'
& docker run --rm @SourceArgs `
    -v "$(DockerPath $Here):/audit:ro" -v "$(DockerPath $OutDir):/out" `
    -e "NORCA_REF=$NorcaRef" -e "BSL_TAG=$BslTag" -e "SETS=$Sets" `
    -e "THRESHOLD=$Threshold" -e "DOLOS_VERSION=$DolosVersion" `
    $Image bash /audit/run.sh 2>&1 | ForEach-Object { Write-Host "$_" }
$ExitCode = $LASTEXITCODE
$ErrorActionPreference = 'Stop'
if ($ExitCode -ne 0) { throw "the audit container failed with exit code $ExitCode" }
Write-Host "==> Result: $(Join-Path $OutDir 'summary.md') and $(Join-Path $OutDir 'pairs.csv')" -ForegroundColor Cyan
