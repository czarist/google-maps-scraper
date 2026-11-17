# Registers a scheduled task that runs watch.ps1 hidden at every logon, and starts it now.
# Remove it with: Unregister-ScheduledTask -TaskName 'google-maps-scraper watch' -Confirm:$false

$name = 'google-maps-scraper watch'
$script = Join-Path $PSScriptRoot 'watch.ps1'

$action = New-ScheduledTaskAction -Execute 'powershell.exe' `
    -Argument "-NoProfile -WindowStyle Hidden -ExecutionPolicy Bypass -File `"$script`""
$trigger = New-ScheduledTaskTrigger -AtLogOn -User $env:USERNAME
$settings = New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries `
    -ExecutionTimeLimit ([TimeSpan]::Zero) -MultipleInstances IgnoreNew

Register-ScheduledTask -TaskName $name -Action $action -Trigger $trigger -Settings $settings -Force | Out-Null
Start-ScheduledTask -TaskName $name

Get-ScheduledTask -TaskName $name | Select-Object TaskName, State
