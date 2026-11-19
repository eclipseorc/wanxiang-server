@echo off
REM 一键编译万象四个服务到 bin\（双击即可）
cd /d "%~dp0.."
powershell -NoProfile -ExecutionPolicy Bypass -File "%~dp0run.ps1" build
echo.
pause
