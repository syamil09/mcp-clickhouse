<#
.SYNOPSIS
    mcp-clickhouse team setup - installs the ClickHouse MCP server for Claude Code in a project.

.DESCRIPTION
    Safe to re-run (re-running = update). It:
      1. Downloads the latest mcp-clickhouse.exe from Google Drive into the project root
         (same layout as the mssql/postgresql MCPs: exe + config side by side).
      2. Asks for your ClickHouse user + password (hidden input) and writes them to
         .mcp-clickhouse-config.json in the project root. The exe and config are added to the
         project's .gitignore so they don't get committed. Saved credentials are reused on re-run.
      3. Tests the connection with the real driver (mcp-clickhouse.exe --check) and lets you
         re-enter credentials if it fails.
      4. Adds the "clickhouse" server to the project's .mcp.json (merges - other servers
         untouched) as "./mcp-clickhouse.exe" - the exe finds its config next to itself.
      5. Enables the server in .claude\settings.local.json (no approval prompt).

    Distribution: this file and mcp-clickhouse.exe live on Google Drive and
    setup-mcp-clickhouse.bat downloads the latest copies on every run. To publish an update:
    bump $SetupVersion, then in Drive use "Manage versions > Upload new version" on the same
    file (keeps the link/ID). To test local changes before publishing, run this .ps1 directly
    with -ExePath pointing at a locally built exe.

.EXAMPLE
    .\setup-mcp-clickhouse.ps1 -ProjectPath "C:\Projects\MyProject"

.EXAMPLE
    .\setup-mcp-clickhouse.ps1 -ProjectPath "C:\Projects\MyProject" -ExePath "..\mcp-clickhouse.exe" -ResetCredentials
#>
param(
    [string]$ProjectPath = (Get-Location).Path,
    # Use a local exe instead of downloading from Google Drive (for testing before publishing)
    [string]$ExePath,
    # Ask for user/password again even if saved credentials exist
    [switch]$ResetCredentials,
    [switch]$Force
)

$ErrorActionPreference = "Stop"
# Windows PowerShell 5.1 redraws Invoke-WebRequest's progress bar for every chunk, which makes
# the 11 MB exe download ~40x slower (measured: 88s with progress, 2s without).
$ProgressPreference = "SilentlyContinue"
$SetupVersion = "2026-10-23.1"

# ---------------------------------------------------------------------------
# Google Drive file ID of mcp-clickhouse.exe - the part between /d/ and /view in the
# share link. The file must be shared as "Anyone with the link can view".
# ---------------------------------------------------------------------------
$ExeDriveId = "1sqszqa1KljYUotAk1vW5EfFQUQtvUKa3"

# ---------------------------------------------------------------------------
# Connection defaults written to every team member's config. Only user and password are
# asked for; change anything else here and publish a new version.
# ---------------------------------------------------------------------------
$Connection = [ordered]@{
    Name             = "price"
    Server           = "10.40.48.80"
    Port             = 30278
    Protocol         = "http"
    Secure           = $false
    Database         = "db_price"
    DefaultUser      = "default"
    QueryTimeout     = 60
    MaxRows          = 100
    BlockedTables    = @()
    SensitiveColumns = @("password", "password_hash", "token", "secret", "api_key")
}
$OutputFormat = "toon"
$ExportMaxRows = 10000

$ServerName = "clickhouse"
$ExeFileName = "mcp-clickhouse.exe"
$ConfigFileName = ".mcp-clickhouse-config.json"
$McpCommand = "./$ExeFileName"

$Utf8NoBom = New-Object System.Text.UTF8Encoding($false)
$Warnings = New-Object System.Collections.Generic.List[string]
$Stamp = Get-Date -Format "yyyyMMdd-HHmmss"

function Write-Step([string]$Text) { Write-Host ""; Write-Host "==> $Text" -ForegroundColor Cyan }
function Write-Ok([string]$Text)   { Write-Host "    [OK]   $Text" -ForegroundColor Green }
function Write-Info([string]$Text) { Write-Host "    [..]   $Text" }
function Write-Warn([string]$Text) { Write-Host "    [WARN] $Text" -ForegroundColor Yellow; $Warnings.Add($Text) }
function Fail([string]$Text)       { Write-Host ""; Write-Host "[FAILED] $Text" -ForegroundColor Red; exit 1 }

# ---------------------------------------------------------------------------
# JSON helpers. Windows PowerShell 5.1's ConvertTo-Json produces odd indentation and
# mangles single-element arrays, so values are serialized here.
# ---------------------------------------------------------------------------
function ConvertTo-JsonString([string]$s) {
    $sb = New-Object System.Text.StringBuilder
    [void]$sb.Append('"')
    foreach ($ch in $s.ToCharArray()) {
        switch ($ch) {
            '"'  { [void]$sb.Append('\"') }
            '\'  { [void]$sb.Append('\\') }
            "`n" { [void]$sb.Append('\n') }
            "`r" { [void]$sb.Append('\r') }
            "`t" { [void]$sb.Append('\t') }
            default {
                if ([int]$ch -lt 0x20) { [void]$sb.AppendFormat('\u{0:x4}', [int]$ch) }
                else { [void]$sb.Append($ch) }
            }
        }
    }
    [void]$sb.Append('"')
    return $sb.ToString()
}

function ConvertTo-PrettyJson($Value, [int]$Indent = 0) {
    $pad = '  ' * ($Indent + 1)
    $end = '  ' * $Indent
    if ($null -eq $Value) { return 'null' }
    if ($Value -is [bool]) { if ($Value) { return 'true' } else { return 'false' } }
    if ($Value -is [string]) { return ConvertTo-JsonString $Value }
    if ($Value -is [int] -or $Value -is [long] -or $Value -is [decimal] -or $Value -is [double]) {
        return ([string]::Format([Globalization.CultureInfo]::InvariantCulture, '{0}', $Value))
    }
    if ($Value -is [System.Collections.IDictionary]) {
        if ($Value.Count -eq 0) { return '{}' }
        $items = foreach ($k in $Value.Keys) { $pad + (ConvertTo-JsonString ([string]$k)) + ': ' + (ConvertTo-PrettyJson $Value[$k] ($Indent + 1)) }
        return "{`n" + ($items -join ",`n") + "`n$end}"
    }
    if ($Value -is [System.Management.Automation.PSCustomObject]) {
        $props = @($Value.PSObject.Properties)
        if ($props.Count -eq 0) { return '{}' }
        $items = foreach ($p in $props) { $pad + (ConvertTo-JsonString $p.Name) + ': ' + (ConvertTo-PrettyJson $p.Value ($Indent + 1)) }
        return "{`n" + ($items -join ",`n") + "`n$end}"
    }
    if ($Value -is [System.Collections.IEnumerable]) {
        $arr = @($Value)
        if ($arr.Count -eq 0) { return '[]' }
        $items = foreach ($v in $arr) { $pad + (ConvertTo-PrettyJson $v ($Indent + 1)) }
        return "[`n" + ($items -join ",`n") + "`n$end]"
    }
    return ConvertTo-JsonString ([string]$Value)
}

function Read-JsonFile([string]$Path) {
    if (-not (Test-Path -LiteralPath $Path)) { return $null }
    $raw = [IO.File]::ReadAllText($Path).TrimStart([char]0xFEFF)
    if (-not $raw.Trim()) { return $null }
    try { return $raw | ConvertFrom-Json }
    catch { Fail "$Path is not valid JSON - fix it first, then re-run. ($($_.Exception.Message))" }
}

function Save-JsonFile([string]$Path, $Value) {
    [IO.File]::WriteAllText($Path, (ConvertTo-PrettyJson $Value) + "`n", $Utf8NoBom)
}

function Set-JsonProperty($Object, [string]$Name, $Value) {
    if ($Object.PSObject.Properties[$Name]) { $Object.$Name = $Value }
    else { $Object | Add-Member -NotePropertyName $Name -NotePropertyValue $Value }
}

# Runs the installed exe and returns its combined output as text. Native stderr must not
# become a terminating error under $ErrorActionPreference = "Stop" (Windows PowerShell 5.1).
function Invoke-Exe([string[]]$Arguments) {
    $prev = $ErrorActionPreference
    $ErrorActionPreference = "Continue"
    try { return (& $InstalledExe @Arguments 2>&1 | ForEach-Object { "$_" }) -join "`n" }
    finally { $ErrorActionPreference = $prev }
}

# ---------------------------------------------------------------------------
# 0. Target folder
# ---------------------------------------------------------------------------
if (-not (Test-Path -LiteralPath $ProjectPath -PathType Container)) { Fail "Project folder not found: $ProjectPath" }
$Root = (Resolve-Path -LiteralPath $ProjectPath).Path
$InstallDir = $Root
$InstalledExe = Join-Path $Root $ExeFileName
$ConfigPath = Join-Path $Root $ConfigFileName

Write-Host "mcp-clickhouse team setup - version $SetupVersion" -ForegroundColor Cyan
Write-Host "Project: $Root"

$markers = @(".git", ".claude", "CLAUDE.md", ".mcp.json", "*.sln", "*.csproj", "package.json", "go.mod") |
    Where-Object { Test-Path (Join-Path $Root $_) }
if (-not $markers -and -not $Force) {
    Write-Host ""
    Write-Host "This folder doesn't look like a project root (no .git, .claude, CLAUDE.md, .mcp.json, ...)." -ForegroundColor Yellow
    $answer = Read-Host "Continue anyway? (y/N)"
    if ("$answer".Trim() -notmatch '^(y|yes)$') { Fail "Cancelled. Put setup-mcp-clickhouse.bat in your project root, or run: setup-mcp-clickhouse.bat `"C:\path\to\project`"" }
}

# Validate existing JSON up front so a broken file stops us before anything is written
$McpJsonPath = Join-Path $Root ".mcp.json"
$LocalSettingsPath = Join-Path $Root ".claude\settings.local.json"
$null = Read-JsonFile $McpJsonPath
$null = Read-JsonFile $LocalSettingsPath

# ---------------------------------------------------------------------------
# 1. Install / update mcp-clickhouse.exe
# ---------------------------------------------------------------------------
Write-Step "Installing mcp-clickhouse.exe"
New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
$tmpExe = Join-Path $InstallDir "mcp-clickhouse.download-$Stamp.exe"

if ($ExePath) {
    if (-not (Test-Path -LiteralPath $ExePath)) { Fail "-ExePath not found: $ExePath" }
    Copy-Item -LiteralPath $ExePath -Destination $tmpExe -Force
    Write-Info "Using local exe: $((Resolve-Path -LiteralPath $ExePath).Path)"
} else {
    if ($ExeDriveId -like "PUT_*") { Fail "The setup script has no download link for mcp-clickhouse.exe yet - contact the setup maintainer." }
    [Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12
    # confirm=t skips Drive's "can't scan for viruses" page (shown for large/executable files)
    $url = "https://drive.usercontent.google.com/download?id=$ExeDriveId&export=download&confirm=t"
    Write-Info "Downloading mcp-clickhouse.exe..."
    try {
        Invoke-WebRequest -Uri $url -OutFile $tmpExe -UseBasicParsing -TimeoutSec 300
    } catch {
        Remove-Item -LiteralPath $tmpExe -ErrorAction SilentlyContinue
        if (Test-Path -LiteralPath $InstalledExe) {
            Write-Warn "Could not download the latest exe ($($_.Exception.Message)) - keeping the installed one"
        } else {
            Fail "Could not download mcp-clickhouse.exe: $($_.Exception.Message)"
        }
    }
}

if (Test-Path -LiteralPath $tmpExe) {
    # A Drive error page is HTML; a real Windows exe starts with "MZ"
    $head = New-Object byte[] 2
    $fs = [IO.File]::OpenRead($tmpExe)
    try { [void]$fs.Read($head, 0, 2) } finally { $fs.Close() }
    if ($head[0] -ne 0x4D -or $head[1] -ne 0x5A) {
        Remove-Item -LiteralPath $tmpExe -ErrorAction SilentlyContinue
        # A Drive error page means the file is no longer shared as "Anyone with the link"
        Fail "The download did not return a valid mcp-clickhouse.exe - contact the setup maintainer."
    }

    if ((Test-Path -LiteralPath $InstalledExe) -and
        ((Get-FileHash -LiteralPath $InstalledExe).Hash -eq (Get-FileHash -LiteralPath $tmpExe).Hash)) {
        Remove-Item -LiteralPath $tmpExe
        Write-Ok "mcp-clickhouse.exe is already up to date"
    } else {
        if (Test-Path -LiteralPath $InstalledExe) {
            # A running exe (open Claude Code session) can't be overwritten, but it can be renamed
            $old = "$InstalledExe.old"
            Remove-Item -LiteralPath $old -Force -ErrorAction SilentlyContinue
            try { Move-Item -LiteralPath $InstalledExe -Destination $old -Force }
            catch { Remove-Item -LiteralPath $tmpExe -ErrorAction SilentlyContinue; Fail "Could not replace the installed exe - close Claude Code and run the setup again." }
        }
        Move-Item -LiteralPath $tmpExe -Destination $InstalledExe -Force
        Write-Ok "Installed $InstalledExe"
    }
}
$exeVersion = Invoke-Exe @("--version")
if ($LASTEXITCODE -ne 0 -or "$exeVersion" -notlike "mcp-clickhouse*") { Fail "The installed mcp-clickhouse.exe does not run: $exeVersion" }
Write-Ok "$exeVersion"

# ---------------------------------------------------------------------------
# 2. Connection + credentials (.mcp-clickhouse-config.json in the project root, git-ignored)
# ---------------------------------------------------------------------------
Write-Step "ClickHouse connection"

# Values offered as defaults: the saved config first, then the team defaults in $Connection
$saved = $null
$existing = Read-JsonFile $ConfigPath
if ($existing -and $existing.PSObject.Properties["connections"] -and $existing.connections) {
    $savedProp = $existing.connections.PSObject.Properties[$Connection.Name]
    if (-not $savedProp) { $savedProp = @($existing.connections.PSObject.Properties)[0] }
    if ($savedProp) { $saved = $savedProp.Value }
}

function Get-SavedValue([string]$Name, $Fallback) {
    if ($saved -and $saved.PSObject.Properties[$Name] -and "$($saved.$Name)" -ne "") { return $saved.$Name }
    return $Fallback
}

$Conn = [ordered]@{
    Server   = [string](Get-SavedValue "server" $Connection.Server)
    Port     = [int](Get-SavedValue "port" $Connection.Port)
    Database = [string](Get-SavedValue "database" $Connection.Database)
    User     = [string](Get-SavedValue "user" $Connection.DefaultUser)
    Password = ""
}
if ($saved -and $saved.PSObject.Properties["password"]) { $Conn.Password = "$($saved.password)" }

function Read-WithDefault([string]$Label, $Default) {
    $v = Read-Host "    $Label [$Default]"
    if (-not "$v".Trim()) { return $Default }
    return "$v".Trim()
}

function Read-Password([string]$Label) {
    if ([Console]::IsInputRedirected) {
        # Scripted run (input piped in): -AsSecureString would wait on the console forever
        return [string](Read-Host)
    }
    $secure = Read-Host "    $Label" -AsSecureString
    $bstr = [Runtime.InteropServices.Marshal]::SecureStringToBSTR($secure)
    try { return [Runtime.InteropServices.Marshal]::PtrToStringBSTR($bstr) }
    finally { [Runtime.InteropServices.Marshal]::ZeroFreeBSTR($bstr) }
}

# Asks for every setting in turn; the current value is the default, so Enter keeps it
function Read-ConnectionSettings {
    Write-Host "    Press Enter to accept the value in [brackets]."
    $Conn.Server = Read-WithDefault "Host" $Conn.Server
    while ($true) {
        $p = Read-WithDefault "Port" $Conn.Port
        $n = 0
        if ([int]::TryParse("$p", [ref]$n) -and $n -ge 1 -and $n -le 65535) { $Conn.Port = $n; break }
        Write-Host "    Port must be a number between 1 and 65535." -ForegroundColor Yellow
    }
    $Conn.Database = Read-WithDefault "Database" $Conn.Database
    $Conn.User = Read-WithDefault "User" $Conn.User
    $Conn.Password = Read-Password "Password for $($Conn.User) (hidden)"
}

function Write-Config {
    $c = [ordered]@{
        server             = $Conn.Server
        port               = $Conn.Port
        protocol           = $Connection.Protocol
        secure             = $Connection.Secure
        database           = $Conn.Database
        user               = $Conn.User
        password           = $Conn.Password
        connection_timeout = 30
        query_timeout      = $Connection.QueryTimeout
        blocked_tables     = $Connection.BlockedTables
        sensitive_columns  = $Connection.SensitiveColumns
        max_rows           = $Connection.MaxRows
        default            = $true
    }
    $cfg = [ordered]@{
        connections     = [ordered]@{ $Connection.Name = $c }
        output_format   = $OutputFormat
        export_max_rows = $ExportMaxRows
    }
    Save-JsonFile $ConfigPath $cfg
}

if ($saved -and "$($saved.user)" -ne "" -and -not $ResetCredentials) {
    $keep = Read-Host "    Saved connection found: $($Conn.User)@$($Conn.Server):$($Conn.Port)/$($Conn.Database). Keep it? (Y/n)"
    if ("$keep".Trim() -match '^(n|no)$') { Read-ConnectionSettings }
} else {
    if (-not $saved) { Write-Info "No saved connection yet - enter the connection details." }
    Read-ConnectionSettings
}

# Keep the credentials file and the exe out of git before the password is written
if (Test-Path -LiteralPath (Join-Path $Root ".git")) {
    $gitignore = Join-Path $Root ".gitignore"
    $ignoreEntries = @($ConfigFileName, $ExeFileName, "$ExeFileName.old")
    $existingLines = @()
    if (Test-Path -LiteralPath $gitignore) {
        $existingLines = @([IO.File]::ReadAllLines($gitignore) | ForEach-Object { $_.Trim().TrimStart('/') })
    }
    $missing = @($ignoreEntries | Where-Object { $existingLines -notcontains $_ })
    if ($missing.Count -eq 0) {
        Write-Ok ".gitignore already excludes the ClickHouse MCP files"
    } else {
        $existingIgnore = if (Test-Path -LiteralPath $gitignore) { [IO.File]::ReadAllText($gitignore) } else { "" }
        $nl = if ($existingIgnore.Contains("`r`n") -or $existingIgnore.Length -eq 0) { "`r`n" } else { "`n" }
        $add = ""
        if ($existingIgnore.Length -gt 0 -and -not $existingIgnore.EndsWith("`n")) { $add += $nl }
        $add += "# ClickHouse MCP (mcp-clickhouse team setup)$nl" + (($missing | ForEach-Object { "$_$nl" }) -join "")
        [IO.File]::AppendAllText($gitignore, $add, $Utf8NoBom)
        Write-Ok "Added $($missing -join ', ') to .gitignore"
    }
    $git = Get-Command git -ErrorAction SilentlyContinue
    if ($git) {
        foreach ($f in @($ConfigFileName, $ExeFileName)) {
            $prev = $ErrorActionPreference
            $ErrorActionPreference = "Continue"
            & git -C $Root ls-files --error-unmatch $f 2>&1 | Out-Null
            $tracked = ($LASTEXITCODE -eq 0)
            $ErrorActionPreference = $prev
            if ($tracked) { Write-Warn "$f is already committed in git - run: git rm --cached $f" }
        }
    }
}

# Test with the real driver; offer to re-enter on failure
$env:CLICKHOUSE_CONFIG_FILE = $ConfigPath
for ($attempt = 1; ; $attempt++) {
    Write-Config
    Write-Info "Testing connection..."
    $check = Invoke-Exe @("--check")
    if ($LASTEXITCODE -eq 0) {
        Write-Ok ($check.Trim() -replace '^OK\s+', '')
        break
    }
    $firstLine = ($check.Trim() -split "`r?`n")[0] -replace '^FAIL\s+', ''
    Write-Host "    [FAIL] $firstLine" -ForegroundColor Red
    if ($check -match 'Authentication failed|REQUIRED_PASSWORD|code: 516|code: 194') {
        Write-Host "    The user or password is wrong." -ForegroundColor Yellow
    } elseif ($check -match 'timeout|refused|no such host|unreachable|connectex|did not properly respond') {
        Write-Host "    The server is not reachable - are you on the office network / VPN?" -ForegroundColor Yellow
    }
    if ($attempt -ge 3) {
        Write-Warn "Connection test failed - config saved anyway. Re-run the setup to fix the connection details."
        break
    }
    $retry = Read-Host "    Enter the connection details again? (Y/n)"
    if ("$retry".Trim() -match '^(n|no)$') {
        Write-Warn "Connection test failed - config saved anyway. Re-run the setup to fix the connection details."
        break
    }
    Read-ConnectionSettings
}
Remove-Item Env:\CLICKHOUSE_CONFIG_FILE -ErrorAction SilentlyContinue
Write-Ok "Config saved to $ConfigPath"

# ---------------------------------------------------------------------------
# 3. .mcp.json + .claude\settings.local.json (merged, other entries preserved)
# ---------------------------------------------------------------------------
Write-Step "Configuring Claude Code (.mcp.json, settings.local.json)"

$mcp = Read-JsonFile $McpJsonPath
if (-not $mcp) { $mcp = [PSCustomObject]@{} }
if (-not $mcp.PSObject.Properties["mcpServers"]) { Set-JsonProperty $mcp "mcpServers" ([PSCustomObject]@{}) }
# The exe reads .mcp-clickhouse-config.json from its own folder, so no env/args are needed
$wanted = [PSCustomObject]@{ command = $McpCommand }
$current = $mcp.mcpServers.PSObject.Properties[$ServerName]
if ($current -and ((ConvertTo-PrettyJson $current.Value) -eq (ConvertTo-PrettyJson $wanted))) {
    Write-Ok ".mcp.json already has the '$ServerName' server"
} else {
    if ($current) {
        Copy-Item -LiteralPath $McpJsonPath -Destination "$McpJsonPath.bak-$Stamp"
        Write-Info "Replacing the existing '$ServerName' entry (old file backed up to .mcp.json.bak-$Stamp)"
    }
    Set-JsonProperty $mcp.mcpServers $ServerName $wanted
    Save-JsonFile $McpJsonPath $mcp
    Write-Ok "Added '$ServerName' server to .mcp.json"
}

New-Item -ItemType Directory -Force -Path (Join-Path $Root ".claude") | Out-Null
$settings = Read-JsonFile $LocalSettingsPath
if (-not $settings) { $settings = [PSCustomObject]@{} }
if ($settings.PSObject.Properties["enableAllProjectMcpServers"] -and $settings.enableAllProjectMcpServers -eq $true) {
    Write-Ok "settings.local.json already enables all project MCP servers"
} else {
    $enabled = @()
    if ($settings.PSObject.Properties["enabledMcpjsonServers"]) { $enabled = @($settings.enabledMcpjsonServers) }
    if ($enabled -contains $ServerName) {
        Write-Ok "settings.local.json already enables '$ServerName'"
    } else {
        Set-JsonProperty $settings "enabledMcpjsonServers" (@($enabled) + $ServerName)
        Save-JsonFile $LocalSettingsPath $settings
        Write-Ok "Enabled '$ServerName' in .claude\settings.local.json"
    }
}
if ($settings.PSObject.Properties["disabledMcpjsonServers"] -and (@($settings.disabledMcpjsonServers) -contains $ServerName)) {
    Write-Warn "'$ServerName' is listed in disabledMcpjsonServers in .claude\settings.local.json - remove it there"
}

# ---------------------------------------------------------------------------
# Done
# ---------------------------------------------------------------------------
Write-Host ""
if ($Warnings.Count -gt 0) {
    Write-Host "Setup finished with $($Warnings.Count) warning(s):" -ForegroundColor Yellow
    $Warnings | ForEach-Object { Write-Host "  - $_" -ForegroundColor Yellow }
} else {
    Write-Host "Setup finished." -ForegroundColor Green
}
Write-Host ""
Write-Host "Next: restart Claude Code in this project (or run /mcp and Reconnect),"
Write-Host "      then check that '$ServerName' shows as connected."
exit 0
