param(
    [string]$Project = 'compute-platform',
    [string]$LokiURL = 'http://127.0.0.1:3100',
    [switch]$RestartCollector
)
$ErrorActionPreference = 'Stop'
$id = [guid]::NewGuid().ToString('N')
$included = "platform-log-test-$id"
$excluded = "platform-log-excluded-$id"
$marker = "log-e2e-$id"
$line = @{event='log_verification'; node_id='validation-node'; deployment_id='validation-deployment'; service_id='validation-node/test/validation-deployment'; message=$marker} | ConvertTo-Json -Compress

function Find-Log([string]$text) {
    # Exclude Loki's own query logs: they contain the searched marker too.
    $query = [uri]::EscapeDataString('{job="compute-platform",service_name="log-verification"} |= "' + $text + '"')
    $result = Invoke-RestMethod "$LokiURL/loki/api/v1/query_range?query=$query&since=10m&limit=100"
    foreach ($entry in $result.data.result) { if ($null -ne $entry -and $entry.stream) { $entry } }
}
function Wait-Log([string]$text) {
    $deadline = [DateTime]::UtcNow.AddSeconds(55)
    do {
        $entries = @(Find-Log $text)
        if ($entries.Count -gt 0) { return $entries }
        Start-Sleep -Seconds 2
    } while ([DateTime]::UtcNow -lt $deadline)
    throw "Log was not delivered within 55 seconds: $text"
}

try {
    # These test containers are uniquely named and removed in finally.
    docker run -d --name $included --label "com.docker.compose.project=$Project" --label com.docker.compose.service=log-verification --env "LOG_LINE=$line" alpine:3.20 sh -c 'printf "%s\n" "$LOG_LINE"; sleep 300' | Out-Null
    if ($LASTEXITCODE -ne 0) { throw 'Included container failed' }
    docker run -d --name $excluded --label "com.docker.compose.project=excluded-$id" --label com.docker.compose.service=log-verification --env "LOG_LINE=excluded-$marker" alpine:3.20 sh -c 'printf "%s\n" "$LOG_LINE"; sleep 300' | Out-Null
    if ($LASTEXITCODE -ne 0) { throw 'Excluded container failed' }
    $entries = @(Wait-Log $marker)
    if ($entries[0].stream.service_id -ne 'validation-node/test/validation-deployment' -or $entries[0].stream.project -ne $Project) { throw "Structured service identity lost: $($entries | ConvertTo-Json -Depth 5 -Compress)" }
    if (@(Find-Log "excluded-$marker").Count -ne 0) { throw 'Unrelated project logs were collected' }
    if ($RestartCollector) {
        $collectors = @(docker ps -q --filter "label=com.docker.compose.project=$Project" --filter label=com.docker.compose.service=alloy)
        if ($collectors.Count -ne 1) { throw 'Expected one collector for the selected project' }
        docker restart $collectors[0] | Out-Null
        if ($LASTEXITCODE -ne 0) { throw 'Collector restart failed' }
        docker exec --env "LOG_LINE=after-restart-$marker" $included sh -c 'printf "%s\n" "$LOG_LINE" > /proc/1/fd/1'
        if ($LASTEXITCODE -ne 0) { throw 'Restart marker write failed' }
        $null = Wait-Log "after-restart-$marker"
        if (@(Find-Log "excluded-$marker").Count -ne 0) { throw 'Restart collected unrelated project logs' }
    }
    @{project=$Project; marker=$marker; structured_identity=$true; unrelated_project_excluded=$true; collector_restart_tested=[bool]$RestartCollector} | ConvertTo-Json
} finally {
    docker rm -f $included $excluded 2>$null | Out-Null
}
