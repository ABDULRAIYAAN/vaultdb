@echo off
title VaultDB
cd /d "%~dp0"

if exist "%ProgramFiles%\Go\bin\go.exe" set "PATH=%ProgramFiles%\Go\bin;%PATH%"

where go >nul 2>nul
if errorlevel 1 (
  echo Go is not installed on this computer yet.
  echo Installing Go with winget. Click Yes if Windows asks for permission.
  echo.
  winget install -e --id GoLang.Go --accept-source-agreements --accept-package-agreements
  echo.
  echo Done. Close this window and double-click run.bat again.
  pause
  exit /b
)

echo ===== Go version =====
go version
echo.
echo ===== Running tests =====
go test ./...
echo.
echo ===== Starting VaultDB =====
echo Your data is saved in data\vaultdb.log and comes back next time you run this.
echo Try: SET name raiyaan   then   EXIT   then run this again and type   GET name
echo.
go run ./cmd/vaultdb
echo.
pause
