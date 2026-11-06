param([string]$Destination = 'dist', [ValidateSet('arm64','amd64')][string]$Architecture)
$ErrorActionPreference = 'Stop'
$NativeArchitecture = switch ([Runtime.InteropServices.RuntimeInformation]::OSArchitecture.ToString()) {
    'Arm64' { 'arm64' }
    'X64' { 'amd64' }
    default { throw 'Build on 64-bit Windows (x64 or ARM64)' }
}
if (-not $Architecture) { $Architecture = $NativeArchitecture }
if ($Architecture -ne $NativeArchitecture) { throw 'Build and verify each Windows package on a matching native runner' }
$ExpectedMachine = if ($Architecture -eq 'arm64') { 0xaa64 } else { 0x8664 }
$CMakeArchitecture = if ($Architecture -eq 'arm64') { 'ARM64' } else { 'x64' }
$AssetName = "fastcontrol_windows_$Architecture.zip"
$ChecksumName = "SHA256SUMS_windows_$Architecture"
function Get-PEDependencies([string]$Path) {
    $Bytes = [IO.File]::ReadAllBytes($Path)
    $PE = [BitConverter]::ToInt32($Bytes, 60)
    if ([BitConverter]::ToUInt16($Bytes, $PE + 4) -ne $ExpectedMachine) { throw "Package executable architecture does not match $Architecture" }
    $Count = [BitConverter]::ToUInt16($Bytes, $PE + 6)
    $OptionalSize = [BitConverter]::ToUInt16($Bytes, $PE + 20)
    $Optional = $PE + 24
    $Sections = for ($Index = 0; $Index -lt $Count; $Index++) {
        $At = $Optional + $OptionalSize + 40 * $Index
        [PSCustomObject]@{Size=[Math]::Max([BitConverter]::ToUInt32($Bytes,$At+8),[BitConverter]::ToUInt32($Bytes,$At+16));RVA=[BitConverter]::ToUInt32($Bytes,$At+12);Raw=[BitConverter]::ToUInt32($Bytes,$At+20)}
    }
    $Offset = { param([uint32]$RVA)
        foreach ($Section in $Sections) { if ($RVA -ge $Section.RVA -and $RVA -lt $Section.RVA + $Section.Size) { return [int]($Section.Raw + $RVA - $Section.RVA) } }
        throw 'Invalid PE Address'
    }
    $At = & $Offset ([BitConverter]::ToUInt32($Bytes,$Optional+120))
    while ([BitConverter]::ToUInt32($Bytes,$At+12) -ne 0) {
        $Name = & $Offset ([BitConverter]::ToUInt32($Bytes,$At+12))
        $End = $Name
        while ($Bytes[$End] -ne 0) { $End++ }
        [Text.Encoding]::ASCII.GetString($Bytes,$Name,$End-$Name)
        $At += 20
    }
}
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
    [string[]]$CPUOptions = if ($Architecture -eq 'arm64') { @('-DGGML_CPU_ARM_ARCH=armv8-a') } else { @('-DGGML_AVX=OFF','-DGGML_AVX2=OFF','-DGGML_FMA=OFF','-DGGML_F16C=OFF') }
    cmake -S (Join-Path $Work 'whisper.cpp-1.9.5') -B $Build -A $CMakeArchitecture -T ClangCL '-DCMAKE_POLICY_DEFAULT_CMP0091=NEW' '-DCMAKE_MSVC_RUNTIME_LIBRARY=MultiThreaded' -DBUILD_SHARED_LIBS=OFF -DWHISPER_BUILD_TESTS=OFF -DWHISPER_BUILD_SERVER=OFF -DWHISPER_CURL=OFF -DWHISPER_BUILD_IS_DEV=OFF -DGGML_NATIVE=OFF -DGGML_OPENMP=OFF @CPUOptions
    if ($LASTEXITCODE -ne 0) { throw 'Speech Configure Failed' }
    cmake --build $Build --config Release --target whisper-cli --parallel 2
    if ($LASTEXITCODE -ne 0) { throw 'Speech Build Failed' }
    $Package = Join-Path $Work 'package'
    New-Item -ItemType Directory (Join-Path $Package 'speech') -Force | Out-Null
    Copy-Item (Join-Path $Build 'bin/Release/whisper-cli.exe') (Join-Path $Package 'speech/whisper-cli.exe')
    Copy-Item (Join-Path $Work 'whisper.cpp-1.9.5/LICENSE') (Join-Path $Package 'speech/Whisper-LICENSE.txt')
    Copy-Item licenses (Join-Path $Package 'licenses') -Recurse
    $ModuleLicenses = Join-Path $Package 'licenses/go-modules'
    New-Item -ItemType Directory $ModuleLicenses -Force | Out-Null
    foreach ($Module in (& go list -m -f '{{.Path}}|{{.Dir}}' all)) {
        $Parts = $Module -split '\|', 2
        if ($Parts.Count -eq 2 -and $Parts[1]) {
            foreach ($License in Get-ChildItem $Parts[1] -File | Where-Object { $_.Name -match '^(LICENSE|COPYING|NOTICE)' }) {
                $Name = ($Parts[0] -replace '[^a-zA-Z0-9.-]', '_') + '_' + $License.Name
                Copy-Item $License.FullName (Join-Path $ModuleLicenses $Name)
            }
        }
    }
    Copy-Item contrib/run-windows.ps1 $Package
    $env:CGO_ENABLED = '0'
    $env:GOARCH = $Architecture
    go build -trimpath -ldflags '-s -w' -o (Join-Path $Package 'fastcontrol.exe') .
    if ($LASTEXITCODE -ne 0) { throw 'Relay Build Failed' }
    & (Join-Path $Package 'speech/whisper-cli.exe') --version
    if ($LASTEXITCODE -ne 0) { throw 'Packaged Speech Runtime Failed' }
    & (Join-Path $Package 'fastcontrol.exe') -speech-status
    if ($LASTEXITCODE -ne 0) { throw 'Packaged Relay Failed' }
    foreach ($Executable in @((Join-Path $Package 'fastcontrol.exe'), (Join-Path $Package 'speech/whisper-cli.exe'))) {
        $Dependencies = Get-PEDependencies $Executable
        if ($Dependencies | Where-Object { $_ -match '^(MSVCP|VCRUNTIME|libomp|ggml|whisper)' }) { throw "Unbundled Runtime Dependency in $Executable" }
        Write-Host "Verified $Architecture Dependencies: $($Dependencies -join ', ')"
    }
    $Asset = Join-Path $Destination $AssetName
    Compress-Archive -Path (Join-Path $Package '*') -DestinationPath $Asset -Force
    $Hash = (Get-FileHash $Asset -Algorithm SHA256).Hash.ToLowerInvariant()
    "$Hash  $AssetName" | Set-Content (Join-Path $Destination $ChecksumName) -Encoding ascii
} finally { Remove-Item $Work -Recurse -Force }
