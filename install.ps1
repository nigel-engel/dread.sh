#Requires -Version 5.1
$ErrorActionPreference = "Stop"
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12

$Repo      = "nigel-engel/dread.sh"
$Binary    = "dread"
$InstallDir = Join-Path $env:LOCALAPPDATA "Programs\dread"

$arch = switch ($env:PROCESSOR_ARCHITECTURE) {
  "AMD64" { "amd64" }
  "ARM64" { "arm64" }
  default { Write-Error "Unsupported architecture: $env:PROCESSOR_ARCHITECTURE"; exit 1 }
}

$tarball = "${Binary}_windows_${arch}.tar.gz"
$url     = "https://github.com/$Repo/releases/latest/download/$tarball"

$tmp = Join-Path ([System.IO.Path]::GetTempPath()) ("dread-install-" + [guid]::NewGuid().ToString("N"))
New-Item -ItemType Directory -Path $tmp | Out-Null

try {
  Write-Host "Downloading dread for windows/$arch..."
  $tarPath = Join-Path $tmp $tarball
  Invoke-WebRequest -Uri $url -OutFile $tarPath -UseBasicParsing

  if (-not (Get-Command tar.exe -ErrorAction SilentlyContinue)) {
    Write-Error "tar.exe not found. Requires Windows 10 1803 or later."
    exit 1
  }
  tar.exe -xzf $tarPath -C $tmp
  if ($LASTEXITCODE -ne 0) { Write-Error "Failed to extract archive."; exit 1 }

  New-Item -ItemType Directory -Path $InstallDir -Force | Out-Null

  # Stop running dread processes so we can replace the binary
  Get-Process -Name $Binary -ErrorAction SilentlyContinue | Stop-Process -Force -ErrorAction SilentlyContinue
  schtasks.exe /End /TN DreadWatch 2>$null | Out-Null

  $dest = Join-Path $InstallDir "$Binary.exe"
  Move-Item -Force -Path (Join-Path $tmp "$Binary.exe") -Destination $dest

  $newVersion = "latest"
  try { $newVersion = (& $dest --version 2>$null) } catch {}
  Write-Host "Installed dread $newVersion to $dest"

  # Add to user PATH if not present
  $userPath = [Environment]::GetEnvironmentVariable("Path", "User")
  if (-not $userPath) { $userPath = "" }
  if (($userPath -split ";") -notcontains $InstallDir) {
    $newPath = if ($userPath) { "$userPath;$InstallDir" } else { $InstallDir }
    [Environment]::SetEnvironmentVariable("Path", $newPath, "User")
    $env:Path = "$env:Path;$InstallDir"
    Write-Host "Added $InstallDir to user PATH"
  }

  # Register background task for notifications (uses dread's own service installer)
  try {
    & $dest service install | Out-Null
    Write-Host "Background notifications enabled (Task Scheduler)"
  } catch {
    Write-Host "Warning: could not register background service: $_"
  }

  # Report successful install (non-blocking, silent)
  $osVersion = try { (Get-CimInstance Win32_OperatingSystem).Caption } catch { "Windows" }
  $hostname  = $env:COMPUTERNAME
  $payload = @{ os_version = $osVersion; hostname = $hostname; dread_version = "$newVersion" } | ConvertTo-Json -Compress
  try {
    Invoke-RestMethod -Uri "https://dread.sh/api/installed" -Method Post -Body $payload -ContentType "application/json" -TimeoutSec 5 | Out-Null
  } catch {}

  Write-Host ""
  Write-Host 'Next: dread new "My Channel"'
  Write-Host "(restart your terminal if 'dread' isn't on PATH yet)"
} finally {
  Remove-Item -Recurse -Force -Path $tmp -ErrorAction SilentlyContinue
}
