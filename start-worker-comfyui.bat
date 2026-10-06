@echo off
setlocal
cd /d "%~dp0"

echo T2VA ComfyUI Worker
echo ==================
echo This starts only the Worker on this GPU computer.
echo Cloud and ComfyUI must already be running.
echo.

where go >nul 2>nul
if errorlevel 1 (
  echo ERROR: Go was not found. Install Go, reopen this window, and run this file again.
  pause
  exit /b 1
)

set "COMFYUI_WORKFLOW=%~dp0workflows\minimax_h3_api.json"
if not exist "%COMFYUI_WORKFLOW%" (
  echo ERROR: Workflow file was not found:
  echo   %COMFYUI_WORKFLOW%
  echo Copy minimax_h3_api.json into the project's workflows folder.
  pause
  exit /b 1
)

set /p "CLOUD_URL=Cloud URL (example: https://t2va.example.com): "
if not defined CLOUD_URL (
  echo ERROR: A Cloud URL is required.
  pause
  exit /b 1
)
set "WORKER_TOKEN="
set /p "WORKER_TOKEN=Worker token: "
if not defined WORKER_TOKEN (
  echo ERROR: A Worker token is required.
  pause
  exit /b 1
)

set "WORKER_ID=gpu-worker-1"
set /p "WORKER_ID=Worker ID [gpu-worker-1]: "
if not defined WORKER_ID set "WORKER_ID=gpu-worker-1"

set "COMFYUI_URL=http://127.0.0.1:8188"
set /p "COMFYUI_URL=ComfyUI URL [http://127.0.0.1:8188]: "
if not defined COMFYUI_URL set "COMFYUI_URL=http://127.0.0.1:8188"

set "INFERENCE_MODE=comfyui"

echo.
echo Checking Cloud connection at %CLOUD_URL% ...
powershell.exe -NoProfile -Command "$u=[uri]'%CLOUD_URL%'; if (($u.Scheme -ne 'http') -and ($u.Scheme -ne 'https')) { exit 1 }; if (Test-NetConnection -ComputerName $u.DnsSafeHost -Port $u.Port -InformationLevel Quiet) { exit 0 } else { exit 1 }" >nul 2>nul
if errorlevel 1 (
  echo ERROR: Cloud is not reachable at %CLOUD_URL%.
  echo Check the URL, DNS, server firewall, and that Cloud is running.
  pause
  exit /b 1
)

powershell.exe -NoProfile -Command "try { Invoke-WebRequest -Uri '%CLOUD_URL%/healthz' -TimeoutSec 8 -UseBasicParsing ^| Out-Null; exit 0 } catch { exit 1 }" >nul 2>nul
if errorlevel 1 (
  echo ERROR: Cloud TCP port is reachable, but /healthz did not return successfully.
  pause
  exit /b 1
)

echo Checking ComfyUI at %COMFYUI_URL% ...
powershell.exe -NoProfile -Command "try { Invoke-WebRequest -Uri '%COMFYUI_URL%/system_stats' -TimeoutSec 5 -UseBasicParsing ^| Out-Null; exit 0 } catch { exit 1 }" >nul 2>nul
if errorlevel 1 (
  echo WARNING: ComfyUI did not respond to /system_stats.
  echo The Worker will still start, but generation may fail until ComfyUI is reachable.
)

echo.
echo Starting Worker. Press Ctrl+C to stop it.
echo Workflow: %COMFYUI_WORKFLOW%
echo.
go run ./cmd/worker
set "WORKER_EXIT=%ERRORLEVEL%"

echo.
if not "%WORKER_EXIT%"=="0" (
  echo Worker exited with code %WORKER_EXIT%.
) else (
  echo Worker stopped.
)
pause
exit /b %WORKER_EXIT%
