@echo off
rem SoundTouch Radio Bridge – Start per Doppelklick (Windows)
rem Lege diese Datei in denselben Ordner wie strb.exe und strb.yaml.

setlocal
chcp 65001 >nul
title SoundTouch Radio Bridge
cd /d "%~dp0"

rem Passendes Programm waehlen (ARM-Geraete bevorzugt strb-arm64.exe)
set "EXE=strb.exe"
if /i "%PROCESSOR_ARCHITECTURE%"=="ARM64" if exist "strb-arm64.exe" set "EXE=strb-arm64.exe"

if not exist "%EXE%" (
  echo.
  echo   %EXE% fehlt in diesem Ordner:
  echo   %CD%
  echo.
  pause
  exit /b 1
)
if not exist "strb.yaml" (
  echo.
  echo   strb.yaml fehlt in diesem Ordner.
  echo   Kopiere deine strb.yaml hierher ^(Vorlage: strb.example.yaml^).
  echo.
  pause
  exit /b 1
)

echo.
echo   ============================================
echo     SoundTouch Radio Bridge
echo   ============================================
echo     Tasten an der Box druecken - Radio laeuft.
echo     Status:   http://127.0.0.1:8686/status
echo     Beenden:  dieses Fenster schliessen oder Ctrl+C
echo.
echo     Tipp: PC nicht in den Ruhezustand schicken,
echo     sonst bleiben die Tasten stumm.
echo   ============================================
echo.

"%EXE%" run %*

echo.
echo   strb wurde beendet.
pause
