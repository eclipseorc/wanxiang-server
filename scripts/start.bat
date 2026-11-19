@echo off
REM 一键启动万象四个服务（双击即可）
REM 首次使用请先双击 build.bat 编译，或确保 bin\ 下已有编译好的 .exe
cd /d "%~dp0.."
powershell -NoProfile -ExecutionPolicy Bypass -File "%~dp0run.ps1" start
echo.
pause
