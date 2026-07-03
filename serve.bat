@echo off
REM Synaptic - one-click launcher for Windows
cd /d "%~dp0"
python serve.py %*
pause
