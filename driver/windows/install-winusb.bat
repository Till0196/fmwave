@echo off
rem Copyright 2026 Till0196
rem SPDX-License-Identifier: GPL-2.0-or-later
rem
rem Assigns the in-box WinUSB driver to the sticks the *-winusb.inf files list.
rem Double-click it to install every INF sitting next to it, or drag one or more
rem onto it. Elevates itself and asks for nothing else.
rem
rem The PowerShell half of this script lives below the marker line at the end
rem of the batch section; the batch half reads the file back and hands that
rem text to PowerShell.

setlocal EnableExtensions
set "FMWAVE_WINUSB_SELF=%~f0"
set "FMWAVE_WINUSB_INF="
for %%F in (%*) do call :append "%%~fF"
powershell -NoProfile -ExecutionPolicy Bypass -Command "$m='#@P'+'S'; $s=Get-Content -LiteralPath $env:FMWAVE_WINUSB_SELF -Raw; Invoke-Expression (($s -split $m,2)[1])"
exit /b %ERRORLEVEL%

:append
set "FMWAVE_WINUSB_INF=%FMWAVE_WINUSB_INF%%~1|"
goto :eof

#@PS
$ErrorActionPreference = 'Stop'

function Quote([string]$s) { "'" + ($s -replace "'", "''") + "'" }

$self = $env:FMWAVE_WINUSB_SELF
$here = Split-Path -Parent $self

$infs = @()
if ($env:FMWAVE_WINUSB_INF) {
  $infs = @($env:FMWAVE_WINUSB_INF -split '\|' | Where-Object { $_ })
}
if ($infs.Count -eq 0) {
  $infs = @(Get-ChildItem -LiteralPath $here -Filter '*-winusb.inf' | ForEach-Object { $_.FullName })
}
if ($infs.Count -eq 0) {
  Write-Host 'No INF found next to this script.' -ForegroundColor Red
  Read-Host 'Press Enter to close'
  exit 1
}
foreach ($inf in $infs) {
  if (-not (Test-Path -LiteralPath $inf -PathType Leaf) -or [IO.Path]::GetExtension($inf) -ne '.inf') {
    Write-Host "Not an INF file: $inf" -ForegroundColor Red
    Read-Host 'Press Enter to close'
    exit 1
  }
}

# Installing a driver needs an elevated token, and a dragged-and-dropped batch
# file never has one. Re-launch the same script under UAC and let the child do
# the work; the parent just exits.
$id = [Security.Principal.WindowsIdentity]::GetCurrent()
$isAdmin = ([Security.Principal.WindowsPrincipal]$id).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
if (-not $isAdmin) {
  $joined = ($infs -join '|')
  $boot = '$m=' + (Quote ('#@P' + 'S')) + '; $s=Get-Content -LiteralPath $env:FMWAVE_WINUSB_SELF -Raw; Invoke-Expression (($s -split $m,2)[1])'
  $cmd = '$env:FMWAVE_WINUSB_SELF=' + (Quote $self) + '; $env:FMWAVE_WINUSB_INF=' + (Quote $joined) + '; ' + $boot
  Start-Process -Verb RunAs -FilePath 'powershell.exe' `
    -ArgumentList '-NoProfile', '-ExecutionPolicy', 'Bypass', '-Command', $cmd | Out-Null
  exit 0
}

# One self-signed code-signing certificate, reused across runs and across INFs.
# Windows accepts a driver package whose catalog is signed by a certificate it
# trusts, so the same certificate goes into Root and TrustedPublisher too.
$subject = 'CN=fmwave WinUSB signing'
$cert = Get-ChildItem Cert:\LocalMachine\My |
  Where-Object { $_.Subject -eq $subject -and $_.NotAfter -gt (Get-Date) } |
  Select-Object -First 1
if ($cert) {
  Write-Host "Reusing certificate $($cert.Thumbprint)"
} else {
  $cert = New-SelfSignedCertificate -Type CodeSigningCert -Subject $subject `
    -CertStoreLocation Cert:\LocalMachine\My -NotAfter (Get-Date).AddYears(2)
  Write-Host "Created certificate $($cert.Thumbprint)"
}

$cer = Join-Path $env:TEMP ('fmwave-signing-' + $cert.Thumbprint + '.cer')
Export-Certificate -Cert $cert -FilePath $cer | Out-Null
try {
  foreach ($store in 'Root', 'TrustedPublisher') {
    $have = Get-ChildItem "Cert:\LocalMachine\$store" |
      Where-Object { $_.Thumbprint -eq $cert.Thumbprint }
    if (-not $have) {
      Import-Certificate -FilePath $cer -CertStoreLocation "Cert:\LocalMachine\$store" | Out-Null
      Write-Host "Trusted the certificate in LocalMachine\$store"
    }
  }
} finally {
  Remove-Item -LiteralPath $cer -Force -ErrorAction SilentlyContinue
}

$failed = 0
foreach ($inf in $infs) {
  $name = Split-Path -Leaf $inf
  Write-Host ''
  Write-Host "== $name" -ForegroundColor Cyan

  # The catalog name must match the INF's CatalogFile, and New-FileCatalog
  # hashes an entire directory, so each INF is staged alone.
  $hit = Select-String -LiteralPath $inf -Pattern '^\s*CatalogFile\s*=\s*(\S+)' | Select-Object -First 1
  if ($hit) { $cat = $hit.Matches[0].Groups[1].Value } else { $cat = [IO.Path]::GetFileNameWithoutExtension($inf) + '.cat' }

  $stage = Join-Path $env:TEMP ('fmwave-winusb-' + [guid]::NewGuid().ToString('N'))
  New-Item -ItemType Directory -Path $stage | Out-Null
  try {
    Copy-Item -LiteralPath $inf -Destination (Join-Path $stage $name)
    New-FileCatalog -Path $stage -CatalogFilePath (Join-Path $stage $cat) -CatalogVersion 2 | Out-Null
    $sig = Set-AuthenticodeSignature -FilePath (Join-Path $stage $cat) -Certificate $cert
    if ($sig.Status -ne 'Valid') {
      throw "Signing $cat failed: $($sig.StatusMessage)"
    }

    & pnputil.exe /add-driver (Join-Path $stage $name) /install
    $code = $LASTEXITCODE
    switch ($code) {
      0 { Write-Host "$name installed." -ForegroundColor Green }
      259 { Write-Host "$name added to the driver store, but no matching device is plugged in." -ForegroundColor Yellow }
      3010 { Write-Host "$name installed. Windows wants a reboot." -ForegroundColor Yellow }
      default {
        Write-Host "pnputil failed with exit code $code." -ForegroundColor Red
        $failed++
      }
    }
  } catch {
    Write-Host $_.Exception.Message -ForegroundColor Red
    $failed++
  } finally {
    Remove-Item -LiteralPath $stage -Recurse -Force -ErrorAction SilentlyContinue
  }
}

Write-Host ''
if ($failed -gt 0) {
  Write-Host "$failed of $($infs.Count) INF(s) failed." -ForegroundColor Red
} else {
  Write-Host "Done. Unplug and replug the stick if fmwave still does not see it."
}
Read-Host 'Press Enter to close'
exit $failed
