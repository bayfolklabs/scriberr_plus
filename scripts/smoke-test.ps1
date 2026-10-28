# End-to-end smoke test against a running local Scriberr: login -> upload -> transcribe -> transcript.
#   powershell -ExecutionPolicy Bypass -File scripts\smoke-test.ps1 [-Model base.en] [-Audio path\to.wav]
# A local test account is created on first run and its credentials are kept in
# data\dev-credentials.json (gitignored), so you can also sign in to the UI with it.
param(
    [string]$BaseUrl = 'http://127.0.0.1:8080',
    [string]$Audio = (Join-Path (Split-Path -Parent $PSScriptRoot) 'tests\data\AMI-Corpus-IB4002.Mix-Headset-clip.wav'),
    [string]$Model = 'base.en',
    [string]$ComputeType = 'int8',
    [int]$TimeoutSec = 900
)

$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
$credFile = Join-Path $root 'data\dev-credentials.json'

function Invoke-Api($Method, $Path, $Body = $null, $Token = $null) {
    $headers = @{}
    if ($Token) { $headers['Authorization'] = "Bearer $Token" }
    $args = @{ Method = $Method; Uri = "$BaseUrl$Path"; Headers = $headers; UseBasicParsing = $true }
    if ($null -ne $Body) { $args['Body'] = ($Body | ConvertTo-Json -Depth 5); $args['ContentType'] = 'application/json' }
    Invoke-RestMethod @args
}

# --- account ---------------------------------------------------------------
if (Test-Path $credFile) {
    $cred = Get-Content $credFile -Raw | ConvertFrom-Json
} else {
    $bytes = New-Object byte[] 18; [Security.Cryptography.RandomNumberGenerator]::Create().GetBytes($bytes)
    $cred = [pscustomobject]@{ username = 'devtester'; password = [Convert]::ToBase64String($bytes) }
    $status = Invoke-Api GET '/api/v1/auth/registration-status'
    if ($status.registration_enabled -eq $false) {
        throw "An account already exists and $credFile is missing; create it with your own username/password."
    }
    Invoke-Api POST '/api/v1/auth/register' @{ username = $cred.username; password = $cred.password; confirmPassword = $cred.password } | Out-Null
    New-Item -ItemType Directory -Force (Split-Path $credFile) | Out-Null
    $cred | ConvertTo-Json | Out-File $credFile -Encoding utf8
    Write-Host "Created local test account '$($cred.username)' (credentials in data\dev-credentials.json)"
}
$token = (Invoke-Api POST '/api/v1/auth/login' @{ username = $cred.username; password = $cred.password }).token
Write-Host "Logged in as $($cred.username)"

# --- upload ----------------------------------------------------------------
Add-Type -AssemblyName System.Net.Http
$client = New-Object System.Net.Http.HttpClient
$client.DefaultRequestHeaders.Authorization = New-Object System.Net.Http.Headers.AuthenticationHeaderValue('Bearer', $token)
$form = New-Object System.Net.Http.MultipartFormDataContent
$fileContent = New-Object System.Net.Http.ByteArrayContent(, [IO.File]::ReadAllBytes($Audio))
$fileContent.Headers.ContentType = [System.Net.Http.Headers.MediaTypeHeaderValue]::Parse('audio/wav')
$form.Add($fileContent, 'audio', [IO.Path]::GetFileName($Audio))
$form.Add((New-Object System.Net.Http.StringContent("Smoke test $(Get-Date -Format s)")), 'title')
$resp = $client.PostAsync("$BaseUrl/api/v1/transcription/upload", $form).Result
$respBody = $resp.Content.ReadAsStringAsync().Result
if (-not $resp.IsSuccessStatusCode) { throw "upload failed: $($resp.StatusCode) $respBody" }
$job = $respBody | ConvertFrom-Json
Write-Host "Uploaded -> job $($job.id)"

# --- transcribe ------------------------------------------------------------
$params = @{ model_family = 'whisper'; model = $Model; device = 'cpu'; compute_type = $ComputeType; batch_size = 4; language = 'en'; vad_method = 'silero' }
Invoke-Api POST "/api/v1/transcription/$($job.id)/start" $params $token | Out-Null
$t0 = Get-Date
do {
    Start-Sleep -Seconds 3
    $st = Invoke-Api GET "/api/v1/transcription/$($job.id)/status" -Token $token
    Write-Host ("  {0,5:N0}s  {1}" -f ((Get-Date) - $t0).TotalSeconds, $st.status)
} while ($st.status -in @('pending', 'processing') -and ((Get-Date) - $t0).TotalSeconds -lt $TimeoutSec)

if ($st.status -ne 'completed') {
    Write-Host "Job ended as '$($st.status)': $($st.error_message)" -ForegroundColor Red
    Write-Host "Log: data\transcripts\$($job.id)\transcription.log"
    exit 1
}
$tr = Invoke-Api GET "/api/v1/transcription/$($job.id)/transcript" -Token $token
$text = $tr.transcript.text
if (-not $text) { $text = ($tr.transcript.segments | ForEach-Object { $_.text }) -join ' ' }
Write-Host ("Completed in {0:N0}s. Words with timings: {1}" -f ((Get-Date) - $t0).TotalSeconds, @($tr.transcript.word_segments).Count) -ForegroundColor Green
Write-Host "Transcript: $text"
