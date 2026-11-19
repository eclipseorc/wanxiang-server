@echo off
REM 一键停止万象四个服务（双击即可）
cd /d "%~dp0.."
powershell -NoProfile -ExecutionPolicy Bypass -File "%~dp0run.ps1" stop
echo.
pause
