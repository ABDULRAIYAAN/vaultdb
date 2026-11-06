@echo off
title VaultDB client
cd /d "%~dp0"
if exist "%ProgramFiles%\Go\bin\go.exe" set "PATH=%ProgramFiles%\Go\bin;%PATH%"
go run ./cmd/vaultdb-cli -addr localhost:7379
pause
