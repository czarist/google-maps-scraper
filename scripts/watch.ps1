# Keeps the MySQL compose stack running and rebuilds the scraper on every code change.
# Started at logon by the "google-maps-scraper watch" scheduled task (see register-watch.ps1).
# Log: scripts\watch.log

$ErrorActionPreference = 'Continue'
$root = Split-Path -Parent $PSScriptRoot
$compose = Join-Path $root 'docker-compose.mysql.yaml'
$log = Join-Path $PSScriptRoot 'watch.log'

function Write-Log($msg) {
    "$(Get-Date -Format 'yyyy-MM-dd HH:mm:ss') $msg" | Out-File -FilePath $log -Append -Encoding utf8
}

Set-Location $root

while ($true) {
    # Docker Desktop can take a minute to start after logon.
    docker info *> $null
    if ($LASTEXITCODE -ne 0) {
        Start-Sleep -Seconds 10
        continue
    }

    Write-Log 'starting docker compose watch'
    docker compose -f $compose up -d *>> $log
    docker compose -f $compose watch *>> $log
    Write-Log "docker compose watch exited with code $LASTEXITCODE; restarting in 15s"
    Start-Sleep -Seconds 15
}
