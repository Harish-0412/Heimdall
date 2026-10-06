$ErrorActionPreference = 'Stop'
$localRoot = [System.IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../../out/control-local'))
New-Item -ItemType Directory -Force -Path $localRoot | Out-Null
$keyPath = Join-Path $localRoot 'credential-key'
if (-not (Test-Path -LiteralPath $keyPath)) {
    $keyBytes = [System.Security.Cryptography.RandomNumberGenerator]::GetBytes(32)
    $keyFile = [System.IO.File]::Open($keyPath, [System.IO.FileMode]::CreateNew)
    try { $keyFile.Write($keyBytes) } finally { $keyFile.Dispose() }
}
Write-Host 'Local credential key prepared. Start PostgreSQL and Redis, run the setup migration, then start the API as documented.'
