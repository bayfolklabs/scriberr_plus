# Simulate a browser live session against a running local Scriberr, end to end:
# audio -> WebM/Opus (like MediaRecorder) -> 1 s chunks POSTed in real time -> live text
# polled from /state -> stop -> finalize -> transcript.
#   powershell -ExecutionPolicy Bypass -File scripts\live-simulate.ps1 [-Repeat 3] [-Speed 1.0]
# -Speed 2 posts chunks twice as fast as real time (stress test). Uses the local test
# account from scripts\smoke-test.ps1 (data\dev-credentials.json).
param(
    [string]$BaseUrl = 'http://127.0.0.1:8080',
    [string]$Audio = (Join-Path (Split-Path -Parent $PSScriptRoot) 'tests\data\AMI-Corpus-IB4002.Mix-Headset-clip.wav'),
    [int]$Repeat = 1,
    [double]$Speed = 1.0,
    [string]$Title = "Live simulation $(Get-Date -Format s)",
    [switch]$AutoRetranscribe,
    # Resilience testing: post only chunks [FromSeq, UntilSeq) of an existing or new session,
    # optionally without stopping, e.g. kill/restart the server in between two runs:
    #   -UntilSeq 25 -NoStop   ...restart server...   -SessionId <id> -FromSeq 25
    [string]$SessionId = '',
    [int]$FromSeq = 0,
    [int]$UntilSeq = -1,
    [switch]$NoStop
)

$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
$env:Path = [Environment]::GetEnvironmentVariable('Path', 'Machine') + ';' + [Environment]::GetEnvironmentVariable('Path', 'User')

$credFile = Join-Path $root 'data\dev-credentials.json'
if (-not (Test-Path $credFile)) { throw "Run scripts\smoke-test.ps1 once first to create the local test account." }
$cred = Get-Content $credFile -Raw | ConvertFrom-Json
$login = Invoke-RestMethod -Method POST -Uri "$BaseUrl/api/v1/auth/login" -ContentType 'application/json' -UseBasicParsing `
    -Body (@{ username = $cred.username; password = $cred.password } | ConvertTo-Json)
$headers = @{ Authorization = "Bearer $($login.token)" }

# --- encode like MediaRecorder: WebM/Opus ------------------------------------
$tmp = Join-Path ([IO.Path]::GetTempPath()) "scriberr-live-sim-$PID.webm"
$loops = [Math]::Max(0, $Repeat - 1)
# bitexact: identical bytes on every run, so a resumed run continues the same stream
& ffmpeg -hide_banner -loglevel error -y -stream_loop $loops -i $Audio -c:a libopus -b:a 128k -ac 1 -fflags +bitexact -flags:a +bitexact -f webm $tmp
if ($LASTEXITCODE) { throw 'ffmpeg encode failed' }
$bytes = [IO.File]::ReadAllBytes($tmp)
Remove-Item $tmp
$duration = [double](& ffprobe -v error -show_entries format=duration -of csv=p=0 $Audio) * $Repeat
$nChunks = [Math]::Max(1, [int][Math]::Ceiling($duration))
$chunkSize = [int][Math]::Ceiling($bytes.Length / $nChunks)
Write-Host ("Audio {0:N1}s -> {1:N0} KB WebM/Opus -> {2} chunks of ~{3:N1} KB at {4}x real time" -f $duration, ($bytes.Length / 1KB), $nChunks, ($chunkSize / 1KB), $Speed)

# --- session -----------------------------------------------------------------
if ($SessionId) {
    $id = $SessionId
    Write-Host "Resuming session $id from seq $FromSeq"
} else {
    $sess = Invoke-RestMethod -Method POST -Uri "$BaseUrl/api/v1/live/sessions" -Headers $headers -ContentType 'application/json' -UseBasicParsing `
        -Body (@{ title = $Title; mime_type = 'audio/webm;codecs=opus'; auto_retranscribe = [bool]$AutoRetranscribe } | ConvertTo-Json)
    $id = $sess.id
    Write-Host "Session $id"
}
$endSeq = if ($UntilSeq -ge 0) { [Math]::Min($UntilSeq, $nChunks) } else { $nChunks }

Add-Type -AssemblyName System.Net.Http
$http = New-Object System.Net.Http.HttpClient
$http.DefaultRequestHeaders.Authorization = New-Object System.Net.Http.Headers.AuthenticationHeaderValue('Bearer', $login.token)

$lastSeg = -1
function Show-State {
    $st = Invoke-RestMethod -Uri "$BaseUrl/api/v1/live/sessions/$id/state?after=$script:lastSeg" -Headers $headers -UseBasicParsing
    foreach ($s in $st.segments) {
        Write-Host ("  [{0,6:N1}-{1,6:N1}] {2}" -f $s.start, $s.end, $s.text) -ForegroundColor Green
        $script:lastSeg = [Math]::Max($script:lastSeg, [int]$s.seq)
    }
    return $st
}

$t0 = (Get-Date).AddSeconds(-$FromSeq / $Speed)
for ($seq = $FromSeq; $seq -lt $endSeq; $seq++) {
    $off = $seq * $chunkSize
    $len = [Math]::Min($chunkSize, $bytes.Length - $off)
    $content = New-Object System.Net.Http.ByteArrayContent($bytes, $off, $len)
    $content.Headers.ContentType = [System.Net.Http.Headers.MediaTypeHeaderValue]::Parse('application/octet-stream')
    $resp = $http.PostAsync("$BaseUrl/api/v1/live/sessions/$id/chunks?seq=$seq", $content).Result
    if (-not $resp.IsSuccessStatusCode) { throw "chunk $seq failed: $($resp.StatusCode) $($resp.Content.ReadAsStringAsync().Result)" }
    $st = Show-State
    if ($seq % 5 -eq 4) {
        $p = if ($st.partial) { $st.partial.text } else { '' }
        Write-Host ("  t={0,5:N1}s received={1,5:N1}s asr={2} processed={3,5:N1}s lag={4,4:N1}s  {5}" -f `
            ((Get-Date) - $t0).TotalSeconds, $st.received_audio_sec, $st.asr.state, $st.asr.processed_sec, $st.asr.lag_sec, $p) -ForegroundColor DarkGray
    }
    $due = $t0.AddSeconds(($seq + 1) / $Speed)
    $wait = ($due - (Get-Date)).TotalMilliseconds
    if ($wait -gt 0) { Start-Sleep -Milliseconds ([int]$wait) }
}

if ($NoStop) {
    Write-Host "Posted chunks $FromSeq..$($endSeq - 1) of $nChunks; not stopping. Resume with: -SessionId $id -FromSeq $endSeq"
    return
}

# --- stop + finalize -----------------------------------------------------------
$stopAt = Get-Date
Invoke-RestMethod -Method POST -Uri "$BaseUrl/api/v1/live/sessions/$id/stop" -Headers $headers -ContentType 'application/json' -UseBasicParsing `
    -Body (@{ last_seq = $nChunks - 1 } | ConvertTo-Json) | Out-Null
do {
    Start-Sleep -Milliseconds 500
    $st = Show-State
} while ($st.live_status -notin @('finalized', 'failed') -and ((Get-Date) - $stopAt).TotalSeconds -lt 180)
Write-Host ("Stop -> {0} in {1:N1}s (asr restarts: {2})" -f $st.live_status, ((Get-Date) - $stopAt).TotalSeconds, $st.asr.restarts)

$tr = Invoke-RestMethod -Uri "$BaseUrl/api/v1/transcription/$id/transcript" -Headers $headers -UseBasicParsing
Write-Host ("Transcript ({0}): {1} segments, {2} words" -f $tr.transcript_source, @($tr.transcript.segments).Count, @($tr.transcript.word_segments).Count)
Write-Host "Open: $BaseUrl/audio/$id"
