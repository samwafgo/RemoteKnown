@echo off
REM RemoteKnown Windows Build Script
REM Usage: build.bat

setlocal

echo ==================================
echo RemoteKnown Build Script
echo ==================================

REM Check Go
where go >nul 2>&1
if errorlevel 1 (
    echo [ERROR] Go not found. Please install Go 1.21+
    exit /b 1
)

REM Check Node.js
where node >nul 2>&1
if errorlevel 1 (
    echo [ERROR] Node.js not found. Please install Node.js 18+
    exit /b 1
)

echo.
echo.
echo [1/4] Building Go backend...
cd /d "%~dp0"
go build -ldflags="-H windowsgui" -o RemoteKnown-daemon.exe ./cmd/main.go
if errorlevel 1 (
    echo [ERROR] Go build failed
    exit /b 1
)
echo [OK] Go backend compiled (RemoteKnown-daemon.exe)

echo.
echo [2/4] Installing Electron dependencies...
cd web
call npm install
if errorlevel 1 (
    echo [ERROR] npm install failed
    exit /b 1
)
echo [OK] Electron dependencies installed

echo.
echo [3/4] Packaging Electron application...
call npm run build:win
if errorlevel 1 (
    echo [ERROR] Electron build failed
    exit /b 1
)
echo [OK] Installer created in web/dist

echo.
echo ==================================
echo Build completed successfully!
echo Installer is located in: web\dist
echo ==================================

endlocal
pause
