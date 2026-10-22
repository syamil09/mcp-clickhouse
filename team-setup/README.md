# mcp-clickhouse team setup

Team members double-click `setup-mcp-clickhouse.bat` in a project root. It downloads the latest
`setup-mcp-clickhouse.ps1` from Google Drive and runs it, which:

1. Downloads `mcp-clickhouse.exe` from Drive into the project root.
2. Asks for the connection step by step (host, port, database, user, then the password, hidden),
   showing a default in `[brackets]` that Enter accepts. The defaults come from the project's
   saved config, or from the `$Connection` block in the `.ps1` when there is none (or it's
   empty). The answers are saved to `.mcp-clickhouse-config.json` in the project root, next to the exe. In a git repo,
   `.mcp-clickhouse-config.json`, `mcp-clickhouse.exe` and `mcp-clickhouse.exe.old` are added to
   `.gitignore` first, and the script warns if any of them was already committed.
3. Tests the connection with `mcp-clickhouse.exe --check`, offering to re-enter on failure.
4. Adds `clickhouse` to the project's `.mcp.json` and enables it in `.claude\settings.local.json`:

   ```json
   "clickhouse": { "command": "./mcp-clickhouse.exe" }
   ```

   This is the same layout as the mssql and postgresql MCPs: the exe reads the config next to itself.

Re-running updates the exe and keeps the project's saved credentials (answer `n` to re-enter
them, or pass `-ResetCredentials`). Each project has its own exe and credentials file.

## First-time publishing

1. Build the exe: `go build -ldflags "-s -w" -o mcp-clickhouse.exe .` in the repo root.
2. Upload `mcp-clickhouse.exe` and `setup-mcp-clickhouse.ps1` to Google Drive and share both as
   **Anyone with the link can view**.
3. Put the exe's file ID in `$ExeDriveId` in `setup-mcp-clickhouse.ps1`, then upload the edited
   `.ps1` again with **Manage versions > Upload new version** (keeps the ID).
4. Put the `.ps1`'s file ID in `SETUP_ID` in `setup-mcp-clickhouse.bat`.
5. Send the team only the `.bat`.

The file ID is the part between `/d/` and `/view` in the share link.

## Updating

- **New exe:** rebuild, then **Upload new version** on the same Drive file. Team members re-run the `.bat`.
- **New connection defaults** (host, database, row limit, blocked tables...): edit the `$Connection`
  block in the `.ps1`, bump `$SetupVersion`, and upload a new version.

## Testing before publishing

Run the `.ps1` directly with a local exe; nothing is downloaded:

```powershell
.\setup-mcp-clickhouse.ps1 -ProjectPath "C:\path\to\project" -ExePath "..\mcp-clickhouse.exe"
```
