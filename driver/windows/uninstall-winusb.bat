@echo off
rem Copyright 2026 Till0196
rem SPDX-License-Identifier: GPL-2.0-or-later
rem
rem Undoes install-winusb.bat: removes the fmwave driver packages from the
rem driver store and deletes the self-signed certificate it created. Double-click
rem it, or drag specific *-winusb.inf onto it to remove only those. A package
rem is fmwave's when its provider is; coax ships INFs of the same names, and
rem those are left alone.
rem
rem Same shape as install-winusb.bat: the batch half reads the file back and
rem hands the PowerShell half to PowerShell.

setlocal EnableExtensions
set "FMWAVE_WINUSB_SELF=%~f0"
set "FMWAVE_WINUSB_INF="
for %%F in (%*) do call :append "%%~nxF"
powershell -NoProfile -ExecutionPolicy Bypass -Command "$m='#@P'+'S'; $s=Get-Content -LiteralPath $env:FMWAVE_WINUSB_SELF -Raw; Invoke-Expression (($s -split $m,2)[1])"
exit /b %ERRORLEVEL%

:append
set "FMWAVE_WINUSB_INF=%FMWAVE_WINUSB_INF%%~1|"
goto :eof

#@PS
$ErrorActionPreference = 'Stop'

function Quote([string]$s) { "'" + ($s -replace "'", "''") + "'" }

$self = $env:FMWAVE_WINUSB_SELF
$wanted = @()
if ($env:FMWAVE_WINUSB_INF) {
  $wanted = @($env:FMWAVE_WINUSB_INF -split '\|' | Where-Object { $_ })
}

$id = [Security.Principal.WindowsIdentity]::GetCurrent()
$isAdmin = ([Security.Principal.WindowsPrincipal]$id).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
if (-not $isAdmin) {
  $boot = '$m=' + (Quote ('#@P' + 'S')) + '; $s=Get-Content -LiteralPath $env:FMWAVE_WINUSB_SELF -Raw; Invoke-Expression (($s -split $m,2)[1])'
  $cmd = '$env:FMWAVE_WINUSB_SELF=' + (Quote $self) + '; $env:FMWAVE_WINUSB_INF=' + (Quote ($wanted -join '|')) + '; ' + $boot
  Start-Process -Verb RunAs -FilePath 'powershell.exe' `
    -ArgumentList '-NoProfile', '-ExecutionPolicy', 'Bypass', '-Command', $cmd | Out-Null
  exit 0
}

# pnputil prints one blank-line-separated record per package, with localized
# labels. Match on the values instead: the record holds both its published
# oemN.inf name and the original *-winusb.inf name.
$records = ((& pnputil.exe /enum-drivers) -join "`n") -split "`n[ \t]*`n"
$removed = 0
$failed = 0
foreach ($rec in $records) {
  $oem = [regex]::Match($rec, '(?im)\b(oem\d+\.inf)\b')
  $orig = [regex]::Match($rec, '(?im)\b([A-Za-z0-9_.-]+-winusb\.inf)\b')
  if (-not $oem.Success -or -not $orig.Success) { continue }
  # The provider line reads "<label>: fmwave" whatever the label's language.
  if ($rec -notmatch '(?im):\s*fmwave\s*$') { continue }
  $name = $orig.Groups[1].Value
  if ($wanted.Count -gt 0 -and $wanted -notcontains $name) { continue }

  Write-Host ''
  Write-Host "== $name ($($oem.Groups[1].Value))" -ForegroundColor Cyan
  & pnputil.exe /delete-driver $oem.Groups[1].Value /uninstall
  if ($LASTEXITCODE -eq 0 -or $LASTEXITCODE -eq 3010) {
    $removed++
  } else {
    Write-Host "pnputil failed with exit code $LASTEXITCODE." -ForegroundColor Red
    $failed++
  }
}

if ($removed -eq 0 -and $failed -eq 0) {
  Write-Host 'No fmwave WinUSB driver package is installed.'
}

# The certificate is only worth keeping while a package signed by it remains.
$stillThere = @(((& pnputil.exe /enum-drivers) -join "`n") -split "`n[ \t]*`n" |
  Where-Object { $_ -match '-winusb\.inf' -and $_ -match '(?im):\s*fmwave\s*$' }).Count -gt 0
if (-not $stillThere) {
  $subject = 'CN=fmwave WinUSB signing'
  foreach ($store in 'My', 'Root', 'TrustedPublisher') {
    Get-ChildItem "Cert:\LocalMachine\$store" |
      Where-Object { $_.Subject -eq $subject } |
      ForEach-Object {
        Remove-Item -LiteralPath $_.PSPath -Force
        Write-Host "Removed the certificate from LocalMachine\$store"
      }
  }
}

Write-Host ''
if ($failed -gt 0) {
  Write-Host "$failed package(s) failed to uninstall." -ForegroundColor Red
} else {
  Write-Host 'Done.'
}
Read-Host 'Press Enter to close'
exit $failed
