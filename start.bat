@echo off
:: Double-click launcher for Synaptic on Windows.
:: Defers to tools\install\start.ps1.
PowerShell -NoProfile -ExecutionPolicy Bypass -File "%~dp0tools\install\start.ps1" %*
