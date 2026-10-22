# mcp-clickhouse

A read-only MCP (Model Context Protocol) server that connects AI agents like Claude Code to ClickHouse. It ships as a single Go binary with defense-in-depth read-only safeguards.

## Features

- Read-only enforcement for `SELECT`, `WITH`, `SHOW`, `DESCRIBE`, `EXPLAIN` and `EXISTS`
- Server-side `readonly=2` per query when the user isn't already read-only (auto-detected)
- Dangerous keyword blocking for writes, mutations, DDL, `INTO OUTFILE` and `FORMAT`
- External table function blocking (`file`, `url`, `s3`, `remote`, `mysql`, `postgresql`, `executable`, ...)
- Table blocklist through config, including comma joins, `IN table`, subqueries, `DESCRIBE` and `SHOW CREATE`
- Sensitive column masking in query results, table metadata and DDL
- Auto row limiting with `LIMIT` plus a hard client-side cap (`truncated: true` when hit)
- Query timeout enforced client-side and via `max_execution_time`
- Server-side query parameters (`{name:Type}` placeholders)
- Parameterized view execution through `exec_view` (the ClickHouse counterpart of stored procedures)
- Native (TCP) and HTTP protocols, with TLS for ClickHouse Cloud
- Multi-connection support
- Per-project config via `.mcp-clickhouse-config.json`
- Query benchmarking with rows/bytes read
- `EXPLAIN` with index usage, storage/compression stats, running queries and query history
- JSON, NDJSON and CSV export with nested JSON column parsing
- Optional TOON output format for token-efficient tabular output

## Tools

| Tool | Description | Parameters |
|------|-------------|------------|
| `list_connections` | List configured connections, default connection and server version. | none |
| `list_databases` | List databases and engines. | `include_system`, `connection` |
| `list_tables` | List tables with engine, rows and size. | `database`, `include_system`, `connection` |
| `describe_table` | Columns, types, codecs, key membership, engine, keys and view parameters. | `table_name`, `connection` |
| `show_create_table` | CREATE statement for a table, view or dictionary. | `table_name`, `connection` |
| `query_database` | Execute a read-only query, optionally with `{name:Type}` parameters. | `sql`, `params`, `connection` |
| `exec_view` | Query a view or parameterized view. | `view`, `params`, `connection` |
| `explain_query` | EXPLAIN a query without running it. | `sql`, `type`, `connection` |
| `benchmark_query` | Compare one or two query runtimes. | `query_a`, `query_b`, `label_a`, `label_b`, `iterations`, `connection` |
| `table_stats` | Rows, parts, partitions, compressed size and compression ratio. | `table_name`, `database`, `limit`, `connection` |
| `running_queries` | Queries currently running (`system.processes`). | `connection` |
| `query_history` | Slow, heavy or failed recent queries (`system.query_log`). | `minutes`, `min_duration_ms`, `sort`, `failed_only`, `limit`, `connection` |
| `export_query` | Export query or view results to a file. | `sql` or `view`, `query_params`, `params`, `format`, `json_columns`, `source_column`, `filename`, `connection` |

## Build

```bash
go build -ldflags "-s -w" -o mcp-clickhouse.exe .
```

Cross-compile examples:

```bash
GOOS=linux GOARCH=amd64 go build -o mcp-clickhouse .
GOOS=darwin GOARCH=amd64 go build -o mcp-clickhouse-mac .
```

Run the unit tests:

```bash
go test ./...
```

## Configuration

Copy `.mcp-clickhouse-config.json.example` to `.mcp-clickhouse-config.json` and fill in credentials. The config file is looked up next to the executable first, then in the current working directory. The recommended format is multi-connection, where every named connection has its own credentials, table blocklist, sensitive columns, row limit, and optional default marker.

Multi-connection:

```json
{
  "connections": {
    "dev": {
      "server": "localhost",
      "port": 9000,
      "protocol": "native",
      "database": "analytics",
      "user": "readonly_user",
      "password": "secret",
      "blocked_tables": ["users", "audit_log", "system.query_log"],
      "sensitive_columns": ["password", "token", "secret"],
      "max_rows": 200,
      "default": true
    },
    "cloud": {
      "server": "abc123.eu-west-1.aws.clickhouse.cloud",
      "port": 8443,
      "protocol": "http",
      "secure": true,
      "user": "default",
      "password": "secret",
      "max_rows": 50
    }
  },
  "output_format": "toon",
  "export_max_rows": 10000
}
```

Single-connection format is also supported:

```json
{
  "server": "localhost",
  "port": 9000,
  "database": "analytics",
  "user": "readonly_user",
  "password": "secret",
  "blocked_tables": ["users", "audit_log"],
  "sensitive_columns": ["password", "token", "secret"],
  "max_rows": 200,
  "output_format": "toon"
}
```

### Connection fields

| Field | Default | Description |
|-------|---------|-------------|
| `server` | | Hostname. Required unless `dsn` is set. |
| `port` | 9000 / 9440 (native), 8123 / 8443 (http) | Default depends on `protocol` and `secure`. |
| `protocol` | `native` | `native` (TCP) or `http`. |
| `secure` | `false` | Use TLS. Required for ClickHouse Cloud. |
| `skip_verify` | `false` | Skip TLS certificate verification. |
| `database` | `default` | Default database for unqualified table names. |
| `user` | `default` | Username. |
| `password` | | Password. |
| `dsn` | | Full DSN instead of the fields above, e.g. `clickhouse://user:pass@host:9440/db?secure=true`. |
| `connection_timeout` | 30 | Dial timeout in seconds. |
| `query_timeout` | 60 | Max query runtime in seconds. |
| `blocked_tables` | `[]` | Table names or `database.table` that can't be read or listed. |
| `sensitive_columns` | password, token, ... | Column names removed from results and metadata. |
| `max_rows` | 100 | Row cap for `query_database` and `exec_view`. |
| `default` | `false` | Marks the default connection. |

### Global fields

| Field | Default | Description |
|-------|---------|-------------|
| `output_format` | `json` | `json` or `toon`. |
| `export_max_rows` | 10000 | Row cap for `export_query`. |
| `export_dir` | `exportDatabaseSql/` next to the exe | Where export files are written. |

## Claude Code MCP Config

Using the compiled binary:

```json
{
  "mcpServers": {
    "clickhouse": {
      "command": "C:/path/to/mcp-clickhouse.exe"
    }
  }
}
```

For development with `go run`:

```json
{
  "mcpServers": {
    "clickhouse": {
      "command": "go",
      "args": ["run", "."],
      "cwd": "C:/path/to/mcp-clickhouse"
    }
  }
}
```

## Environment Fallbacks

- `CLICKHOUSE_CONFIG_FILE`: explicit config file path
- `CLICKHOUSE_DSN`: ClickHouse DSN for the default connection
- `CLICKHOUSE_BLOCKED_TABLES`: comma-separated fallback table blocklist
- `CLICKHOUSE_SENSITIVE_COLUMNS`: comma-separated fallback sensitive columns

DSN example:

```text
clickhouse://user:password@localhost:9000/analytics?dial_timeout=30s
```

## Query Parameters

`query_database` binds `{name:Type}` placeholders on the server, so values never get spliced into SQL:

```text
query_database(
  sql: "SELECT count() FROM events WHERE user_id = {uid:UInt64} AND day >= {from:Date}",
  params: {"uid": 42, "from": "2026-01-01"}
)
```

## Parameterized Views

ClickHouse has no stored procedures. The closest equivalent is a parameterized view:

```sql
CREATE VIEW reports.daily_sales AS
SELECT region, sum(amount) AS total
FROM sales
WHERE day = {day:Date}
GROUP BY region
```

Call it with `exec_view`:

```text
exec_view(view: "reports.daily_sales", params: "day='2026-01-01'")
```

The server checks that the target is a view, isn't blocked, and that every declared parameter is supplied. If parameters are missing, the error lists their names and types. `describe_table` also shows them under `view_parameters`.

## Read-Only Enforcement

Several independent layers:

1. Statement allowlist: only `SELECT`, `WITH`, `SHOW`, `DESCRIBE`/`DESC`, `EXPLAIN` and `EXISTS`, single statement.
2. Keyword and table function blocking on SQL with literals and comments masked.
3. Server-side settings. At connect time the server reads `getSetting('readonly')`:
   - `0`: every query runs with `readonly=2`, `max_execution_time`, and `max_result_rows` + `result_overflow_mode=break`.
   - `1`: the user is already read-only, and such users can't change any setting, so none are sent.
   - `2`: limits are sent without changing `readonly`.
4. A client-side row cap and query timeout that apply regardless of server settings.

Queries can't override `readonly`, `max_result_rows`, `result_overflow_mode` or `max_execution_time` through a `SETTINGS` clause.

When `blocked_tables` is non-empty, `merge()`, `cluster()` and `clusterAllReplicas()` are also blocked, because they can read tables by name or regex.

## Project Structure

```text
mcp-clickhouse/
├── main.go           # entry point, stdio MCP server
├── config.go         # config loading, multi-connection support
├── connmgr.go        # lazy connection pool manager
├── db.go             # ClickHouse execution, settings, value normalization
├── security.go       # query validation, blocklist, masking, audit log
├── tools.go          # MCP tool registration and handlers
├── export.go         # export_query and exec_view
├── toon.go           # TOON serialization
├── security_test.go  # unit tests
├── go.mod
└── go.sum
```

## Dependencies

| Package | Purpose |
|---------|---------|
| `github.com/mark3labs/mcp-go` | MCP protocol implementation |
| `github.com/ClickHouse/clickhouse-go/v2` | ClickHouse driver (native and HTTP) |

## Notes

- Use a read-only ClickHouse user (`readonly = 1` in its settings profile) even though the server enforces read-only behavior.
- `table_stats`, `running_queries` and `query_history` need `SELECT` on `system.parts`, `system.processes` and `system.query_log`. Add those to `blocked_tables` to disable the tools for a connection.
- Over HTTP, `benchmark_query` reports timing only; rows/bytes read come from native-protocol progress packets.
