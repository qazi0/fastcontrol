param([switch]$SkipSpeech, [string]$PackageDirectory)
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
try { [Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12 } catch {}
if ($env:OS -ne 'Windows_NT') { throw 'Run This Installer On Windows' }
if (-not $PackageDirectory -and $env:FASTCONTROL_PACKAGE_DIR) { $PackageDirectory = $env:FASTCONTROL_PACKAGE_DIR }
$ArchRaw = ''
try {
    $ArchObj = [Runtime.InteropServices.RuntimeInformation]::OSArchitecture
    if ($null -ne $ArchObj) { $ArchRaw = "$ArchObj" }
} catch {}
if (-not $ArchRaw) {
    $ArchRaw = if ($env:PROCESSOR_ARCHITEW6432) { $env:PROCESSOR_ARCHITEW6432 } else { $env:PROCESSOR_ARCHITECTURE }
}
if (-not $ArchRaw) {
    try {
        $Cpu = Get-CimInstance Win32_Processor -ErrorAction SilentlyContinue | Select-Object -First 1
        if ($null -ne $Cpu -and $null -ne $Cpu.Architecture) { $ArchRaw = "$($Cpu.Architecture)" }
    } catch {}
}
if ($null -eq $ArchRaw) { $ArchRaw = '' }
$Architecture = switch ($ArchRaw.Trim().ToUpperInvariant()) {
    'ARM64' { 'arm64' }
    '12' { 'arm64' }
    'X64' { 'amd64' }
    'AMD64' { 'amd64' }
    '9' { 'amd64' }
    default { throw "fastcontrol requires 64-bit Windows (x64 or ARM64), detected: $ArchRaw" }
}
$AssetName = "fastcontrol_windows_$Architecture.zip"
$ChecksumName = "SHA256SUMS_windows_$Architecture"
$Repo = 'qazi0/fastcontrol'
$Version = if ($env:FASTCONTROL_VERSION) { $env:FASTCONTROL_VERSION } else { 'latest' }
$Work = Join-Path ([IO.Path]::GetTempPath()) ([Guid]::NewGuid().ToString())
$Directory = Join-Path ([Environment]::GetFolderPath('UserProfile')) '.fastcontrol'
$TaskName = 'fastcontrol'
New-Item -ItemType Directory $Work | Out-Null
try {
    if ($PackageDirectory) {
        Copy-Item (Join-Path $PackageDirectory $AssetName) $Work
        Copy-Item (Join-Path $PackageDirectory $ChecksumName) $Work
    } else {
        $Base = if ($Version -eq 'latest') { "https://github.com/$Repo/releases/latest/download" } else { "https://github.com/$Repo/releases/download/$Version" }
        Invoke-WebRequest -Uri "$Base/$AssetName" -OutFile (Join-Path $Work $AssetName) -UseBasicParsing
        Invoke-WebRequest -Uri "$Base/$ChecksumName" -OutFile (Join-Path $Work $ChecksumName) -UseBasicParsing
    }
    $Asset = Join-Path $Work $AssetName
    $Lines = @(Get-Content (Join-Path $Work $ChecksumName) | Where-Object { $_ -match ('^[0-9a-fA-F]{64}\s+' + [regex]::Escape($AssetName) + '$') })
    if ($Lines.Count -ne 1) { throw 'Release Checksum Is Missing Or Ambiguous' }
    $ExpectedHash = ($Lines[0].Trim() -split '\s+')[0]
    $ActualHash = (Get-FileHash -Path $Asset -Algorithm SHA256).Hash
    if ($ActualHash -ne $ExpectedHash) { throw 'Release Checksum Failed' }
    $Expanded = Join-Path $Work 'expanded'
    Expand-Archive -Path $Asset -DestinationPath $Expanded -Force
    & (Join-Path $Expanded 'fastcontrol.exe') -speech-status | Out-Null
    if ($LASTEXITCODE -ne 0) { throw 'Downloaded Relay Failed' }
    & (Join-Path (Join-Path $Expanded 'speech') 'whisper-cli.exe') --version
    if ($LASTEXITCODE -ne 0) { throw 'Downloaded Speech Runtime Failed' }
    $Task = Get-ScheduledTask -TaskName $TaskName -ErrorAction SilentlyContinue
    if ($Task) { Stop-ScheduledTask -TaskName $TaskName -ErrorAction SilentlyContinue }
    # Stop only this user's installed relay and its child processes before replacement.
    $InstalledExe = Join-Path $Directory 'fastcontrol.exe'
    Get-CimInstance Win32_Process -ErrorAction SilentlyContinue | Where-Object { $_.ExecutablePath -eq $InstalledExe } | ForEach-Object {
        try { Stop-Process -Id $_.ProcessId -Force -ErrorAction SilentlyContinue } catch {}
        try { & taskkill.exe /PID $_.ProcessId /T /F *> $null } catch {}
    }
    Start-Sleep -Milliseconds 300
    New-Item -ItemType Directory $Directory -Force | Out-Null
    Copy-Item (Join-Path $Expanded '*') $Directory -Recurse -Force
    $Skip = $SkipSpeech -or $env:FASTCONTROL_SKIP_SPEECH -eq '1'
    $StatusRaw = (& (Join-Path $Directory 'fastcontrol.exe') -speech-status) -join "`n"
    $Status = if ($StatusRaw) { $StatusRaw | ConvertFrom-Json } else { $null }
    if (-not $Skip -and (-not $Status -or -not $Status.available)) {
        $Choice = Read-Host 'Install The Speech Model Now? [Y/n]'
        $Skip = $Choice -match '^(n|no)$'
    }
    if (-not $Skip) {
        & (Join-Path $Directory 'fastcontrol.exe') -install-speech
        if ($LASTEXITCODE -ne 0) { throw 'Speech Setup Failed. Run Again Or Use -SkipSpeech' }
    }
    $CurrentIdentity = [Security.Principal.WindowsIdentity]::GetCurrent()
    $User = if ($null -ne $CurrentIdentity -and $CurrentIdentity.Name) { $CurrentIdentity.Name } else { "$env:USERDOMAIN\$env:USERNAME" }
    $Action = New-ScheduledTaskAction -Execute 'powershell.exe' -Argument ('-NoProfile -NonInteractive -WindowStyle Hidden -ExecutionPolicy Bypass -File "' + (Join-Path $Directory 'run-windows.ps1') + '"') -WorkingDirectory $Directory
    $Trigger = New-ScheduledTaskTrigger -AtLogOn -User $User
    $Principal = New-ScheduledTaskPrincipal -UserId $User -LogonType Interactive -RunLevel Limited
    $Settings = New-ScheduledTaskSettingsSet -ExecutionTimeLimit ([TimeSpan]::Zero) -RestartCount 3 -RestartInterval (New-TimeSpan -Minutes 1) -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries
    Register-ScheduledTask -TaskName $TaskName -Action $Action -Trigger $Trigger -Principal $Principal -Settings $Settings -Force | Out-Null
    Start-ScheduledTask -TaskName $TaskName
    Write-Host "Installed fastcontrol. Show the pairing QR: & '$Directory\fastcontrol.exe' -pair"
    $CanShowPair = $false
    try { $CanShowPair = [Environment]::UserInteractive -and -not [Console]::IsOutputRedirected } catch { $CanShowPair = $false }
    if ($CanShowPair) { & (Join-Path $Directory 'fastcontrol.exe') -pair }
} finally {
    if (Test-Path $Work) { Remove-Item $Work -Recurse -Force -ErrorAction SilentlyContinue }
}
