@echo off
rem mcp-clickhouse team setup - launcher.
rem Downloads the latest setup-mcp-clickhouse.ps1 from Google Drive and runs it,
rem so updating the team = upload a new version to Drive, everyone re-runs this file.
rem Double-click from the project root, or run: setup-mcp-clickhouse.bat "C:\path\to\project"
setlocal
set "SETUP_ID=13MBdsdbm7XCFxG1ih9DK4V7OWZrWqCQH"
set "SETUP_URL=https://drive.usercontent.google.com/download?id=%SETUP_ID%&export=download&confirm=t"
set "CACHE_DIR=%LOCALAPPDATA%\mcp-clickhouse"
set "CACHE=%CACHE_DIR%\setup-mcp-clickhouse.ps1"
set "DL=%TEMP%\setup-mcp-clickhouse-%RANDOM%%RANDOM%.ps1"

set "TARGET=%~1"
if "%TARGET%"=="" set "TARGET=%CD%"
if "%TARGET:~-1%"=="\" set "TARGET=%TARGET:~0,-1%"

echo Downloading latest setup script...
rem Exit codes: 1 = download failed, 2 = not the setup script (e.g. Drive error page), 3 = script has syntax errors
powershell -NoProfile -ExecutionPolicy Bypass -Command "$ProgressPreference = 'SilentlyContinue'; [Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor 3072; try { Invoke-WebRequest -UseBasicParsing -Uri '%SETUP_URL%' -OutFile '%DL%' -TimeoutSec 60 } catch { exit 1 }; $t = [IO.File]::ReadAllText('%DL%'); if ($t -match '^\s*<(!doctype|html)' -or $t -notlike '*mcp-clickhouse team setup*') { exit 2 }; $e = $null; [void][Management.Automation.Language.Parser]::ParseFile('%DL%', [ref]$null, [ref]$e); if ($e) { exit 3 }; exit 0"
set "DL_RC=%ERRORLEVEL%"

if "%DL_RC%"=="0" (
    if not exist "%CACHE_DIR%" mkdir "%CACHE_DIR%"
    move /y "%DL%" "%CACHE%" >nul
    goto run
)

if exist "%DL%" del "%DL%" >nul 2>&1
echo.
if "%DL_RC%"=="2" (echo [WARN] The download did not return the setup script - contact the setup maintainer.) else if "%DL_RC%"=="3" (echo [WARN] The downloaded setup script is incomplete or broken - not running it.) else (echo [WARN] Could not download the setup script - check your internet / proxy connection.)
if not exist "%CACHE%" (
    echo [FAILED] No saved copy from an earlier run either, so setup cannot continue.
    set "RC=1"
    goto end
)
echo [WARN] Running the copy saved from the last successful download instead - it may be outdated.

:run
echo.
powershell -NoProfile -ExecutionPolicy Bypass -File "%CACHE%" -ProjectPath "%TARGET%" %2 %3 %4
set "RC=%ERRORLEVEL%"

:end
echo.
pause
exit /b %RC%
