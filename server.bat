@echo off
title VaultDB server
cd /d "%~dp0"
if exist "%ProgramFiles%\Go\bin\go.exe" set "PATH=%ProgramFiles%\Go\bin;%PATH%"
go run ./cmd/vaultdb -serve localhost:7379
pause
