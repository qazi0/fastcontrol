$ErrorActionPreference = 'Stop'
$Relay = Join-Path $PSScriptRoot 'fastcontrol.exe'
while ($true) {
    & $Relay -no-qr -tls *>> (Join-Path $PSScriptRoot 'relay.log')
    Start-Sleep -Seconds 3
}
