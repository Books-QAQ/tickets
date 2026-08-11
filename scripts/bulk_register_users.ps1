param(
    [string]$CsvPath = ".\scripts\users.example.csv",
    [string]$BaseUrl = "http://127.0.0.1:8080",
    [switch]$StopOnError
)

$ErrorActionPreference = "Stop"

if (-not (Test-Path -LiteralPath $CsvPath)) {
    throw "CSV file not found: $CsvPath"
}

$users = Import-Csv -LiteralPath $CsvPath
if (-not $users -or $users.Count -eq 0) {
    throw "CSV file is empty: $CsvPath"
}

$registerUrl = ($BaseUrl.TrimEnd("/")) + "/register"
$successCount = 0
$failureCount = 0

foreach ($user in $users) {
    if ([string]::IsNullOrWhiteSpace($user.username) -or
        [string]::IsNullOrWhiteSpace($user.password) -or
        [string]::IsNullOrWhiteSpace($user.full_name)) {
        Write-Host "SKIP  missing field(s): username='$($user.username)' full_name='$($user.full_name)'" -ForegroundColor Yellow
        $failureCount++
        if ($StopOnError) {
            exit 1
        }
        continue
    }

    $payload = @{
        username  = $user.username
        password  = $user.password
        full_name = $user.full_name
    } | ConvertTo-Json

    try {
        $response = Invoke-RestMethod -Method Post -Uri $registerUrl -ContentType "application/json" -Body $payload
        Write-Host "OK    $($response.username)" -ForegroundColor Green
        $successCount++
    }
    catch {
        $statusCode = $_.Exception.Response.StatusCode.value__
        $responseBody = ""

        if ($_.ErrorDetails -and $_.ErrorDetails.Message) {
            $responseBody = $_.ErrorDetails.Message
        }

        Write-Host "FAIL  $($user.username) [HTTP $statusCode] $responseBody" -ForegroundColor Red
        $failureCount++

        if ($StopOnError) {
            exit 1
        }
    }
}

Write-Host ""
Write-Host "Done. Success: $successCount  Failed: $failureCount"

if ($failureCount -gt 0) {
    exit 1
}
