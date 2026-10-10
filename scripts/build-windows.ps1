param([string]$Destination = 'dist')
$ErrorActionPreference = 'Stop'
if ([Runtime.InteropServices.RuntimeInformation]::OSArchitecture -ne 'Arm64') { throw 'Build on Windows ARM64 Only' }
$Root = Split-Path $PSScriptRoot -Parent
Set-Location $Root
$Destination = [IO.Path]::GetFullPath($Destination)
New-Item -ItemType Directory -Force $Destination | Out-Null
$Work = Join-Path ([IO.Path]::GetTempPath()) ([Guid]::NewGuid().ToString())
New-Item -ItemType Directory $Work | Out-Null
try {
    $SourceArchive = Join-Path $Work 'whisper.tar.gz'
    Invoke-WebRequest 'https://github.com/ggml-org/whisper.cpp/archive/refs/tags/v1.9.5.tar.gz' -OutFile $SourceArchive
    if ((Get-FileHash $SourceArchive -Algorithm SHA256).Hash.ToLowerInvariant() -ne 'ff1a9053feb509ff9d7729703355541ae9690073a6b1c40eb692c962e0dc1720') { throw 'Whisper Source Checksum Failed' }
    tar -xzf $SourceArchive -C $Work
    if ($LASTEXITCODE -ne 0) { throw 'Whisper Extraction Failed' }
    $Build = Join-Path $Work 'build'
    cmake -S (Join-Path $Work 'whisper.cpp-1.9.5') -B $Build -A ARM64 '-DCMAKE_MSVC_RUNTIME_LIBRARY=MultiThreaded' -DBUILD_SHARED_LIBS=OFF -DWHISPER_BUILD_TESTS=OFF -DWHISPER_BUILD_SERVER=OFF -DWHISPER_CURL=OFF -DGGML_NATIVE=OFF -DGGML_OPENMP=OFF
    if ($LASTEXITCODE -ne 0) { throw 'Speech Configure Failed' }
    cmake --build $Build --config Release --target whisper-cli --parallel 2
    if ($LASTEXITCODE -ne 0) { throw 'Speech Build Failed' }
    $Package = Join-Path $Work 'package'
    New-Item -ItemType Directory (Join-Path $Package 'speech') -Force | Out-Null
    Copy-Item (Join-Path $Build 'bin/Release/whisper-cli.exe') (Join-Path $Package 'speech/whisper-cli.exe')
    Copy-Item (Join-Path $Work 'whisper.cpp-1.9.5/LICENSE') (Join-Path $Package 'speech/Whisper-LICENSE.txt')
    Copy-Item licenses (Join-Path $Package 'licenses') -Recurse
    Copy-Item contrib/run-windows.ps1 $Package
    $env:CGO_ENABLED = '0'
    go build -trimpath -ldflags '-s -w' -o (Join-Path $Package 'fastcontrol.exe') .
    if ($LASTEXITCODE -ne 0) { throw 'Relay Build Failed' }
    & (Join-Path $Package 'speech/whisper-cli.exe') --version
    if ($LASTEXITCODE -ne 0) { throw 'Packaged Speech Runtime Failed' }
    & (Join-Path $Package 'fastcontrol.exe') -speech-status
    if ($LASTEXITCODE -ne 0) { throw 'Packaged Relay Failed' }
    $Asset = Join-Path $Destination 'fastcontrol_windows_arm64.zip'
    Compress-Archive -Path (Join-Path $Package '*') -DestinationPath $Asset -Force
    $Hash = (Get-FileHash $Asset -Algorithm SHA256).Hash.ToLowerInvariant()
    "$Hash  fastcontrol_windows_arm64.zip" | Set-Content (Join-Path $Destination 'SHA256SUMS_windows_arm64') -Encoding ascii
} finally { Remove-Item $Work -Recurse -Force }
