@echo off
title VaultDB website
cd /d "%~dp0"
if exist "%ProgramFiles%\Go\bin\go.exe" set "PATH=%ProgramFiles%\Go\bin;%PATH%"
echo Starting the VaultDB website. Your browser opens in a few seconds.
echo Press Ctrl+C in this window to stop it.
start "" cmd /c "timeout /t 10 >nul & start http://localhost:8080"
go run ./cmd/vaultdb-web -addr localhost:8080
pause
