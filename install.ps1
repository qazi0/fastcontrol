param([switch]$SkipSpeech, [string]$PackageDirectory)
$ErrorActionPreference = 'Stop'
if ($env:OS -ne 'Windows_NT') { throw 'Run This Installer on Windows' }
if ([Runtime.InteropServices.RuntimeInformation]::OSArchitecture -ne 'Arm64') { throw 'fastcontrol Currently Supports Windows ARM64 Only' }
$Repo = 'qazi0/fastcontrol'
$Version = if ($env:FASTCONTROL_VERSION) { $env:FASTCONTROL_VERSION } else { 'latest' }
$Work = Join-Path ([IO.Path]::GetTempPath()) ([Guid]::NewGuid().ToString())
$Directory = Join-Path ([Environment]::GetFolderPath('UserProfile')) '.fastcontrol'
$TaskName = 'fastcontrol'
New-Item -ItemType Directory $Work | Out-Null
try {
    if ($PackageDirectory) {
        Copy-Item (Join-Path $PackageDirectory 'fastcontrol_windows_arm64.zip') $Work
        Copy-Item (Join-Path $PackageDirectory 'SHA256SUMS_windows_arm64') $Work
    } else {
        if (-not (Get-Command gh -ErrorAction SilentlyContinue)) { throw 'Install GitHub CLI and Run gh auth login First' }
        $Arguments = @('release', 'download', '--repo', $Repo, '--dir', $Work, '--pattern', 'fastcontrol_windows_arm64.zip', '--pattern', 'SHA256SUMS_windows_arm64')
        if ($Version -ne 'latest') { $Arguments += $Version }
        & gh @Arguments
        if ($LASTEXITCODE -ne 0) { throw 'Release Download Failed. Check Repository Access' }
    }
    $Asset = Join-Path $Work 'fastcontrol_windows_arm64.zip'
    $Line = Get-Content (Join-Path $Work 'SHA256SUMS_windows_arm64') | Where-Object { $_ -match '^[0-9a-fA-F]{64}\s+fastcontrol_windows_arm64\.zip$' }
    if (@($Line).Count -ne 1) { throw 'Release Checksum Is Missing or Ambiguous' }
    if ((Get-FileHash $Asset -Algorithm SHA256).Hash -ne ($Line -split '\s+')[0]) { throw 'Release Checksum Failed' }
    $Expanded = Join-Path $Work 'expanded'
    Expand-Archive $Asset $Expanded
    & (Join-Path $Expanded 'fastcontrol.exe') -speech-status | Out-Null
    if ($LASTEXITCODE -ne 0) { throw 'Downloaded Relay Failed' }
    & (Join-Path $Expanded 'speech/whisper-cli.exe') --version
    if ($LASTEXITCODE -ne 0) { throw 'Downloaded Speech Runtime Failed' }
    $Task = Get-ScheduledTask -TaskName $TaskName -ErrorAction SilentlyContinue
    if ($Task) { Stop-ScheduledTask -TaskName $TaskName }
    # Stop only this user's installed relay and its child processes before replacement.
    Get-CimInstance Win32_Process | Where-Object { $_.ExecutablePath -eq (Join-Path $Directory 'fastcontrol.exe') } | ForEach-Object { & taskkill.exe /PID $_.ProcessId /T /F | Out-Null }
    Start-Sleep -Milliseconds 300
    New-Item -ItemType Directory $Directory -Force | Out-Null
    Copy-Item (Join-Path $Expanded '*') $Directory -Recurse -Force
    $Skip = $SkipSpeech -or $env:FASTCONTROL_SKIP_SPEECH -eq '1'
    $Status = (& (Join-Path $Directory 'fastcontrol.exe') -speech-status | ConvertFrom-Json)
    if (-not $Skip -and -not $Status.available) {
        $Choice = Read-Host 'Install the Speech Model Now? [Y/n]'
        $Skip = $Choice -match '^(n|no)$'
    }
    if (-not $Skip) {
        & (Join-Path $Directory 'fastcontrol.exe') -install-speech
        if ($LASTEXITCODE -ne 0) { throw 'Speech Setup Failed. Run Again or Use -SkipSpeech' }
    }
    $User = [Security.Principal.WindowsIdentity]::GetCurrent().Name
    $Action = New-ScheduledTaskAction -Execute 'powershell.exe' -Argument ('-NoProfile -NonInteractive -WindowStyle Hidden -ExecutionPolicy Bypass -File "' + (Join-Path $Directory 'run-windows.ps1') + '"') -WorkingDirectory $Directory
    $Trigger = New-ScheduledTaskTrigger -AtLogOn -User $User
    $Principal = New-ScheduledTaskPrincipal -UserId $User -LogonType Interactive -RunLevel Limited
    $Settings = New-ScheduledTaskSettingsSet -ExecutionTimeLimit ([TimeSpan]::Zero) -RestartCount 3 -RestartInterval (New-TimeSpan -Minutes 1) -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries
    Register-ScheduledTask -TaskName $TaskName -Action $Action -Trigger $Trigger -Principal $Principal -Settings $Settings -Force | Out-Null
    Start-ScheduledTask -TaskName $TaskName
    Write-Host "Installed fastcontrol. Pairing links appear in $Directory\relay.log. Allow incoming access on your trusted private network if Windows prompts."
} finally { Remove-Item $Work -Recurse -Force }
