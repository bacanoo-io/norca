@echo off
REM SPDX-FileCopyrightText: 2026 Norca contributors
REM SPDX-License-Identifier: MPL-2.0
REM
REM Builds the Windows release of Norca from this checkout:
REM   1. regenerates the embedded web UI, nomad\command\agent\bindata_assetfs.go
REM      (as ember-dist and static-assets in nomad\GNUmakefile), built from
REM      nomad\ui,
REM   2. builds norca.exe and norca-natsbridge.exe like
REM      .github\workflows\release.yml,
REM   3. writes a CycloneDX SBOM per binary, and packages everything into
REM      dist\norca_<version>_windows_<arch>.zip with a SHA256SUMS file.
REM
REM Usage: windows.bat [skipui] [arm64]
REM   skipui    keep the current bindata_assetfs.go (no Node or Yarn needed)
REM   arm64     build for windows/arm64 instead of windows/amd64
REM
REM Needs git and Go (go.mod selects the toolchain); for the UI also Node 20
REM or later and Yarn 1.22.

setlocal EnableExtensions DisableDelayedExpansion

set "ROOT=%~dp0"
if "%ROOT:~-1%"=="\" set "ROOT=%ROOT:~0,-1%"
set "NOMAD=%ROOT%\nomad"
set "DIST=%ROOT%\dist"
set "TOOLS=%NOMAD%\build\tools"
set "TAR=%SystemRoot%\System32\tar.exe"
set SKIPUI=0
set ARCH=amd64

:args
if "%~1"=="" goto argsdone
if /i "%~1"=="skipui" (set SKIPUI=1) else if /i "%~1"=="arm64" (set ARCH=arm64) else (
  echo unknown option: %~1
  echo usage: windows.bat [skipui] [arm64]
  exit /b 2
)
shift
goto args
:argsdone

where git >nul 2>&1 || (echo git is not on the PATH & exit /b 1)
where go >nul 2>&1 || (echo go is not on the PATH & exit /b 1)
if %SKIPUI%==1 goto binaries
where node >nul 2>&1 || (echo node is not on the PATH; install Node 20 or later, or use: windows.bat skipui & exit /b 1)
where yarn >nul 2>&1 || (echo yarn is not on the PATH; install Yarn 1.22, or use: windows.bat skipui & exit /b 1)

set HUSKY_SKIP_INSTALL=1

REM --- 1. Web UI ---------------------------------------------------------------
echo ==^> Building the web UI (nomad\ui)
pushd "%NOMAD%\ui" || goto fail
call yarn install --frozen-lockfile --network-timeout 300000 || goto fail
call npm run build || goto fail
popd

echo ==^> Embedding the UI into nomad\command\agent\bindata_assetfs.go
set "GOBIN=%TOOLS%"
go -C "%NOMAD%" install github.com/hashicorp/go-bindata/go-bindata@bf7910af899725e4938903fb32048c7c0b15f12e || goto fail
go -C "%NOMAD%" install github.com/elazarl/go-bindata-assetfs/go-bindata-assetfs@234c15e7648ff35458026de92b34c637bae5e6f7 || goto fail
set GOBIN=
set "PATH=%TOOLS%;%PATH%"
pushd "%NOMAD%" || goto fail
go-bindata-assetfs -pkg agent -prefix ui -modtime 1480000000 -tags ui -o bindata_assetfs.go ./ui/dist/... || goto fail
move /y bindata_assetfs.go command\agent\bindata_assetfs.go >nul || goto fail
popd

REM --- 2. Binaries -------------------------------------------------------------
:binaries
for /f tokens^=2^ delims^=^" %%v in ('findstr /c:"NorcaVersion = " "%NOMAD%\version\version.go"') do set "VERSION=%%v"
if not defined VERSION (echo NorcaVersion not found in nomad\version\version.go & goto fail)
for /f %%s in ('git -C "%ROOT%" rev-parse HEAD') do set "SHA=%%s"
set TZ=UTC0
for /f %%t in ('git -C "%ROOT%" show -s "--format=%%cd" "--date=format-local:%%Y-%%m-%%dT%%H:%%M:%%SZ" HEAD') do set "BUILDDATE=%%t"
set TZ=
set "NAME=norca_%VERSION%_windows_%ARCH%"
set "OUT=%DIST%\%NAME%"
set "SBOM=%DIST%\sbom"

set DIRTY=
for /f %%l in ('git -C "%ROOT%" status --porcelain') do set DIRTY=1
if defined DIRTY echo WARNING: the checkout has uncommitted changes; the binaries are built from them but carry commit %SHA%

echo ==^> Building norca %VERSION% (%SHA%, %BUILDDATE%) for windows/%ARCH%
if exist "%OUT%" rmdir /s /q "%OUT%"
mkdir "%OUT%" || goto fail
set CGO_ENABLED=0
set GOOS=windows
set GOARCH=%ARCH%
go -C "%NOMAD%" build -trimpath -tags ui,codegen_generated,release,timetzdata -ldflags "-X github.com/hashicorp/nomad/version.GitCommit=%SHA% -X github.com/hashicorp/nomad/version.BuildDate=%BUILDDATE%" -o "%OUT%\norca.exe" . || goto fail
go -C "%NOMAD%\natsbridge" build -trimpath -o "%OUT%\norca-natsbridge.exe" ./cmd/norca-natsbridge || goto fail
set CGO_ENABLED=
set GOOS=
set GOARCH=

copy /y "%NOMAD%\LICENSE" "%OUT%\LICENSE" >nul || goto fail
copy /y "%ROOT%\NOTICE" "%OUT%\NOTICE" >nul || goto fail
copy /y "%ROOT%\THIRD_PARTY_NOTICES.md" "%OUT%\THIRD_PARTY_NOTICES.md" >nul || goto fail

REM --- 3. SBOM, archive, checksums ---------------------------------------------
echo ==^> Writing the SBOMs
if not exist "%SBOM%" mkdir "%SBOM%"
for %%b in (norca norca-natsbridge) do (
  go -C "%NOMAD%\tools" run ./thirdparty sbom -root "%ROOT%" -strict -name %%b -binary "%OUT%\%%b.exe" -source-url "https://github.com/bacanoo-io/norca/tree/%SHA%" -out "%SBOM%\%%b_%VERSION%_windows_%ARCH%.cdx.json" || goto fail
)

echo ==^> Packaging dist\%NAME%.zip
if exist "%DIST%\%NAME%.zip" del "%DIST%\%NAME%.zip"
"%TAR%" -a -c -f "%DIST%\%NAME%.zip" -C "%OUT%" LICENSE NOTICE THIRD_PARTY_NOTICES.md norca.exe norca-natsbridge.exe || goto fail

(
  for /f %%h in ('certutil -hashfile "%DIST%\%NAME%.zip" SHA256 ^| findstr /v ":"') do echo %%h  %NAME%.zip
  for %%b in (norca norca-natsbridge) do (
    for /f %%h in ('certutil -hashfile "%SBOM%\%%b_%VERSION%_windows_%ARCH%.cdx.json" SHA256 ^| findstr /v ":"') do echo %%h  sbom/%%b_%VERSION%_windows_%ARCH%.cdx.json
  )
) > "%DIST%\%NAME%_SHA256SUMS"

"%OUT%\norca.exe" version || goto fail
echo ==^> Done: %OUT%
echo           %DIST%\%NAME%.zip
type "%DIST%\%NAME%_SHA256SUMS"
exit /b 0

:fail
echo ==^> FAILED
exit /b 1
