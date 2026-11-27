$ErrorActionPreference = 'Continue'
$Relay = Join-Path $PSScriptRoot 'fastcontrol.exe'
$Log = Join-Path $PSScriptRoot 'relay.log'
while ($true) {
    try {
        & $Relay -no-qr -tls *>> $Log
    } catch {
        $_ | Out-String | Out-File -FilePath $Log -Append -Encoding utf8
    }
    Start-Sleep -Seconds 3
}
