@echo off
title VaultDB chat
cd /d "%~dp0"
if exist "%ProgramFiles%\Go\bin\go.exe" set "PATH=%ProgramFiles%\Go\bin;%PATH%"
if not exist vaultdb-chat.env copy vaultdb-chat.env.example vaultdb-chat.env >nul
if "%~1"=="" (
  echo Settings are in vaultdb-chat.env. First time? Run: chat.bat ingest ^<folder^>
  echo.
  go run ./cmd/vaultdb-chat chat
) else (
  go run ./cmd/vaultdb-chat %*
)
pause
