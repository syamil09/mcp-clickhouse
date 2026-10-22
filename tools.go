package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

var (
	limitKeywordPattern     = regexp.MustCompile(`(?i)\bLIMIT\b`)
	limitByPattern          = regexp.MustCompile(`(?i)^\s*[^\s,]+(\s*,\s*[^\s,]+)?(\s+OFFSET\s+[^\s,]+)?\s+BY\b`)
	fetchOrTopPattern       = regexp.MustCompile(`(?i)(\bFETCH\s+(FIRST|NEXT)\b|^\s*SELECT\s+(DISTINCT\s+)?TOP\s+\d)`)
	topLevelSettingsPattern = regexp.MustCompile(`(?i)\bSETTINGS\b`)
)

var systemDatabases = []string{"system", "INFORMATION_SCHEMA", "information_schema"}

// Without annotations MCP clients must assume a tool is destructive and open-world,
// so every tool declares its actual behavior.

// readOnlyTool builds a tool that only reads from the configured ClickHouse servers.
func readOnlyTool(name string, opts ...mcp.ToolOption) mcp.Tool {
	opts = append(opts,
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithOpenWorldHintAnnotation(false),
	)
	return mcp.NewTool(name, opts...)
}

// exportTool builds a tool that reads from ClickHouse and writes a new file to the export directory.
func exportTool(name string, opts ...mcp.ToolOption) mcp.Tool {
	opts = append(opts,
		mcp.WithReadOnlyHintAnnotation(false),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(false),
		mcp.WithOpenWorldHintAnnotation(false),
	)
	return mcp.NewTool(name, opts...)
}

func registerTools(s *server.MCPServer, cm *ConnectionManager) {
	connDesc := mcp.Description("Connection name from config. Uses default if omitted. Use list_connections to see available connections.")

	s.AddTool(
		readOnlyTool("list_connections",
			mcp.WithDescription("List all available ClickHouse connections and which one is the default."),
		),
		handleListConnections(cm),
	)

	s.AddTool(
		readOnlyTool("list_databases",
			mcp.WithDescription("List databases on the ClickHouse server with their engine."),
			mcp.WithBoolean("include_system",
				mcp.Description("Include system and INFORMATION_SCHEMA databases. Default: false.")),
			mcp.WithString("connection", connDesc),
		),
		handleListDatabases(cm),
	)

	s.AddTool(
		readOnlyTool("list_tables",
			mcp.WithDescription("List tables, views and dictionaries with engine, row count and size. Blocked tables are excluded."),
			mcp.WithString("database",
				mcp.Description("Only list tables in this database. Default: all non-system databases.")),
			mcp.WithBoolean("include_system",
				mcp.Description("Include system databases when no database is given. Default: false.")),
			mcp.WithString("connection", connDesc),
		),
		handleListTables(cm),
	)

	s.AddTool(
		readOnlyTool("describe_table",
			mcp.WithDescription("Describe a table: columns (type, default, codec, comment, partition/sorting/primary key membership), engine, keys, row count and size. For parameterized views, lists the view parameters. Blocked tables and sensitive columns are excluded."),
			mcp.WithString("table_name", mcp.Required(),
				mcp.Description("Table name, optionally database-qualified, e.g. 'analytics.events'. Unqualified names use the connection's database.")),
			mcp.WithString("connection", connDesc),
		),
		handleDescribeTable(cm),
	)

	s.AddTool(
		readOnlyTool("show_create_table",
			mcp.WithDescription("Return the CREATE statement (DDL) for a table, view, materialized view or dictionary. Sensitive column definitions are removed."),
			mcp.WithString("table_name", mcp.Required(),
				mcp.Description("Table name, optionally database-qualified, e.g. 'analytics.events'.")),
			mcp.WithString("connection", connDesc),
		),
		handleShowCreateTable(cm),
	)

	s.AddTool(
		readOnlyTool("query_database",
			mcp.WithDescription("Execute a read-only query against ClickHouse (SELECT, WITH, SHOW, DESCRIBE, EXPLAIN, EXISTS). Writes, DDL, external table functions and FORMAT clauses are blocked. Results are row-limited and sensitive columns are masked. Supports server-side query parameters: write {name:Type} placeholders and pass values in 'params'."),
			mcp.WithString("sql", mcp.Required(),
				mcp.Description("The SQL query to execute, e.g. \"SELECT * FROM events WHERE user_id = {uid:UInt64}\".")),
			mcp.WithObject("params",
				mcp.Description(`Optional values for {name:Type} placeholders, e.g. {"uid": 42, "day": "2026-01-01"}. A JSON string is also accepted.`)),
			mcp.WithString("connection", connDesc),
		),
		handleQuery(cm),
	)

	s.AddTool(
		readOnlyTool("exec_view",
			mcp.WithDescription("Query a view, including parameterized views (CREATE VIEW v AS SELECT ... WHERE x = {x:UInt32}). This is the ClickHouse counterpart of a stored procedure. If required parameters are missing, the error lists them."),
			mcp.WithString("view", mcp.Required(),
				mcp.Description("View name, optionally database-qualified, e.g. 'reports.daily_sales'.")),
			mcp.WithString("params",
				mcp.Description(`View arguments, e.g. "start_date='2026-01-01', region_id=3". Leave empty for regular views.`)),
			mcp.WithString("connection", connDesc),
		),
		handleExecView(cm),
	)

	s.AddTool(
		readOnlyTool("explain_query",
			mcp.WithDescription("Show how ClickHouse will execute a SELECT/WITH query, without running it. Use 'indexes' to check whether the primary key, partition pruning and skip indexes are used."),
			mcp.WithString("sql", mcp.Required(),
				mcp.Description("The SELECT/WITH query to explain.")),
			mcp.WithString("type",
				mcp.Description("One of: indexes (default, plan with index usage), plan, pipeline, syntax (rewritten query), estimate (rows/marks/parts to read), ast."),
				mcp.Enum("indexes", "plan", "pipeline", "syntax", "estimate", "ast")),
			mcp.WithString("connection", connDesc),
		),
		handleExplain(cm),
	)

	s.AddTool(
		readOnlyTool("benchmark_query",
			mcp.WithDescription("Benchmark one or two read-only queries and return execution time, result row count and rows/bytes read by the server (no data)."),
			mcp.WithString("query_a", mcp.Required(),
				mcp.Description("The first SQL query to benchmark.")),
			mcp.WithString("query_b",
				mcp.Description("Optional second SQL query to compare against query_a.")),
			mcp.WithString("label_a",
				mcp.Description("Optional label for query_a. Default: 'Query A'.")),
			mcp.WithString("label_b",
				mcp.Description("Optional label for query_b. Default: 'Query B'.")),
			mcp.WithNumber("iterations",
				mcp.Description("Runs per query, 1-20. Reports average, min and max. Default: 1.")),
			mcp.WithString("connection", connDesc),
		),
		handleBenchmark(cm),
	)

	s.AddTool(
		readOnlyTool("table_stats",
			mcp.WithDescription("Storage statistics from active parts: rows, parts, partitions, compressed/uncompressed size and compression ratio. Without table_name, lists the largest tables. With table_name, also returns per-column sizes and codecs."),
			mcp.WithString("table_name",
				mcp.Description("Optional table, e.g. 'analytics.events'.")),
			mcp.WithString("database",
				mcp.Description("Optional database filter when table_name is omitted.")),
			mcp.WithNumber("limit",
				mcp.Description("Max tables to list when table_name is omitted. Default: 20.")),
			mcp.WithString("connection", connDesc),
		),
		handleTableStats(cm),
	)

	s.AddTool(
		readOnlyTool("running_queries",
			mcp.WithDescription("List queries currently running on the server (system.processes) with elapsed time, rows/bytes read and memory usage."),
			mcp.WithString("connection", connDesc),
		),
		handleRunningQueries(cm),
	)

	s.AddTool(
		readOnlyTool("query_history",
			mcp.WithDescription("Find slow, heavy or failed recent queries from system.query_log. Useful for performance investigations."),
			mcp.WithNumber("minutes",
				mcp.Description("Look-back window in minutes. Default: 60.")),
			mcp.WithNumber("min_duration_ms",
				mcp.Description("Only queries that took at least this long. Default: 0.")),
			mcp.WithString("sort",
				mcp.Description("Sort by: duration (default), memory, read_bytes, recent."),
				mcp.Enum("duration", "memory", "read_bytes", "recent")),
			mcp.WithBoolean("failed_only",
				mcp.Description("Only queries that threw an exception. Default: false.")),
			mcp.WithNumber("limit",
				mcp.Description("Max rows, 1-200. Default: 20.")),
			mcp.WithString("connection", connDesc),
		),
		handleQueryHistory(cm),
	)

	s.AddTool(
		exportTool("export_query",
			mcp.WithDescription("Export query or view results to a file as JSON, NDJSON or CSV. JSON exports support nested JSON column parsing and single-column extraction."),
			mcp.WithString("sql",
				mcp.Description("Read-only query to execute and export. Mutually exclusive with 'view'.")),
			mcp.WithObject("query_params",
				mcp.Description(`Optional values for {name:Type} placeholders in 'sql', e.g. {"uid": 42}.`)),
			mcp.WithString("view",
				mcp.Description("View to execute and export. Mutually exclusive with 'sql'.")),
			mcp.WithString("params",
				mcp.Description(`Arguments for a parameterized view, e.g. "start_date='2026-01-01'".`)),
			mcp.WithString("format",
				mcp.Description("Output format: json (default), ndjson or csv."),
				mcp.Enum("json", "ndjson", "csv")),
			mcp.WithString("json_columns",
				mcp.Description("Comma-separated column names to parse as nested JSON objects (json/ndjson). If omitted, auto-detects string values starting with { or [.")),
			mcp.WithString("source_column",
				mcp.Description("Extract JSON from this single column as the output list (json/ndjson).")),
			mcp.WithString("filename",
				mcp.Description("Custom output filename (without extension). Default: <source_name>_<timestamp>.<ext>")),
			mcp.WithString("connection", connDesc),
		),
		handleExport(cm),
	)
}

func handleListConnections(cm *ConnectionManager) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		type connInfo struct {
			Name          string `json:"name"`
			Server        string `json:"server,omitempty"`
			Port          int    `json:"port,omitempty"`
			Database      string `json:"database,omitempty"`
			User          string `json:"user,omitempty"`
			Protocol      string `json:"protocol"`
			Secure        bool   `json:"secure"`
			UsesDSN       bool   `json:"uses_dsn,omitempty"`
			MaxRows       int    `json:"max_rows"`
			QueryTimeout  string `json:"query_timeout"`
			IsDefault     bool   `json:"is_default"`
			Connected     bool   `json:"connected"`
			ServerVersion string `json:"server_version,omitempty"`
			ReadonlyLevel *int   `json:"server_readonly_level,omitempty"`
		}

		var connections []connInfo
		for _, name := range cm.ListConnections() {
			cfg := cm.Configs()[name]
			info := connInfo{
				Name:         name,
				Protocol:     cfg.ProtocolName(),
				Secure:       cfg.Secure,
				UsesDSN:      cfg.DSN != "",
				MaxRows:      cfg.MaxRowsResolved,
				QueryTimeout: cfg.QueryTimeoutDuration().String(),
				IsDefault:    name == cm.DefaultName(),
			}
			if cfg.DSN == "" {
				info.Server = cfg.Server
				info.Port = cfg.ResolvedPort()
				info.Database = cfg.Database
				info.User = cfg.User
			}
			if db := cm.Connected(name); db != nil {
				info.Connected = true
				info.ServerVersion = db.ServerVersion
				level := db.ReadonlyLevel
				info.ReadonlyLevel = &level
			}
			connections = append(connections, info)
		}

		out := map[string]interface{}{
			"connections": connections,
			"count":       len(connections),
			"default":     cm.DefaultName(),
		}
		jsonBytes, _ := json.MarshalIndent(out, "", "  ")
		return mcp.NewToolResultText(string(jsonBytes)), nil
	}
}

func handleListDatabases(cm *ConnectionManager) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		db, _, err := resolveDB(cm, req)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		query := "SELECT name, engine FROM system.databases"
		if !req.GetBool("include_system", false) {
			query += " WHERE name NOT IN (" + quoteStringList(systemDatabases) + ")"
		}
		query += " ORDER BY name"

		result, err := db.ExecuteQuery(ctx, query, QueryOptions{})
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to list databases: %s", err.Error())), nil
		}
		return textOrError(serializeResult(result))
	}
}

func handleListTables(cm *ConnectionManager) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		db, connCfg, err := resolveDB(cm, req)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		database := strings.TrimSpace(req.GetString("database", ""))
		var where string
		switch {
		case database != "":
			where = "database = " + quoteString(database)
		case req.GetBool("include_system", false):
			where = "1"
		default:
			where = "database NOT IN (" + quoteStringList(systemDatabases) + ")"
		}

		result, err := db.ExecuteQuery(ctx, `
			SELECT database, name, engine, total_rows, total_bytes,
			       formatReadableSize(total_bytes) AS size, comment
			FROM system.tables
			WHERE `+where+` AND NOT is_temporary
			ORDER BY database, name
		`, QueryOptions{})
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to list tables: %s", err.Error())), nil
		}

		available := make([]map[string]interface{}, 0, len(result.Rows))
		for _, row := range result.Rows {
			schema := rowString(row, "database")
			name := rowString(row, "name")
			if name == "" || isTableBlocked(schema, name, connCfg.BlockedTablesMap) {
				continue
			}
			available = append(available, map[string]interface{}{
				"table":   schema + "." + name,
				"engine":  row["engine"],
				"rows":    row["total_rows"],
				"size":    row["size"],
				"comment": row["comment"],
			})
		}

		out := map[string]interface{}{
			"tables": available,
			"count":  len(available),
			"note":   "Blocked tables are excluded from this list. rows/size are null for views and engines that don't track them.",
		}
		return textOrError(serializeAny(out))
	}
}

func handleDescribeTable(cm *ConnectionManager) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		db, connCfg, err := resolveDB(cm, req)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		tableName := strings.TrimSpace(req.GetString("table_name", ""))
		if tableName == "" {
			return mcp.NewToolResultError("table_name parameter is required"), nil
		}

		schema, table := splitQualifiedNameRaw(tableName)
		if table == "" {
			return mcp.NewToolResultError("invalid table_name"), nil
		}
		if isTableBlocked(schema, table, connCfg.BlockedTablesMap) {
			return mcp.NewToolResultError(fmt.Sprintf("table '%s' is not accessible", tableName)), nil
		}

		filter := func(nameColumn string) string {
			return "database = " + databaseExpr(schema) + " AND " + nameColumn + " = " + quoteString(table)
		}

		info, err := db.ExecuteQuery(ctx, `
			SELECT database, name, engine, partition_key, sorting_key, primary_key, sampling_key,
			       total_rows, formatReadableSize(total_bytes) AS size, comment, create_table_query
			FROM system.tables
			WHERE `+filter("name"), QueryOptions{})
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to describe table: %s", err.Error())), nil
		}
		if info.Count == 0 {
			return mcp.NewToolResultError(fmt.Sprintf("table '%s' not found", tableName)), nil
		}
		tableInfo := info.Rows[0]
		createQuery := rowString(tableInfo, "create_table_query")
		delete(tableInfo, "create_table_query")

		columns, err := db.ExecuteQuery(ctx, `
			SELECT name AS column_name, type, default_kind, default_expression, compression_codec, comment,
			       is_in_partition_key, is_in_sorting_key, is_in_primary_key
			FROM system.columns
			WHERE `+filter("table")+`
			ORDER BY position
		`, QueryOptions{})
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to describe columns: %s", err.Error())), nil
		}
		columns = filterSensitiveColumnMetadata(columns, connCfg.SensitiveColumnsMap)

		out := map[string]interface{}{
			"table":   tableInfo,
			"columns": columns.Rows,
		}
		if params := extractViewParameters(createQuery); len(params) > 0 {
			out["view_parameters"] = params
		}
		return textOrError(serializeAny(out))
	}
}

func handleShowCreateTable(cm *ConnectionManager) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		db, connCfg, err := resolveDB(cm, req)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		tableName := strings.TrimSpace(req.GetString("table_name", ""))
		schema, table := splitQualifiedNameRaw(tableName)
		if table == "" {
			return mcp.NewToolResultError("table_name parameter is required"), nil
		}
		if isTableBlocked(schema, table, connCfg.BlockedTablesMap) {
			return mcp.NewToolResultError(fmt.Sprintf("table '%s' is not accessible", tableName)), nil
		}

		result, err := db.ExecuteQuery(ctx, "SHOW CREATE TABLE "+qualifiedIdent(schema, table), QueryOptions{})
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to show create table: %s", err.Error())), nil
		}
		if result.Count == 0 || len(result.Columns) == 0 {
			return mcp.NewToolResultError(fmt.Sprintf("table '%s' not found", tableName)), nil
		}

		ddl := rowString(result.Rows[0], result.Columns[0])
		return mcp.NewToolResultText(filterCreateStatement(ddl, connCfg.SensitiveColumnsMap)), nil
	}
}

func handleQuery(cm *ConnectionManager) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		db, connCfg, err := resolveDB(cm, req)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		args := req.GetArguments()
		sqlQuery, ok := args["sql"].(string)
		if !ok || strings.TrimSpace(sqlQuery) == "" {
			return mcp.NewToolResultError("sql parameter is required"), nil
		}

		params, err := parseQueryParams(args["params"])
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		if err := ValidateQuery(sqlQuery, connCfg.BlockedTablesMap); err != nil {
			AuditLog(sqlQuery, false, 0, err)
			return mcp.NewToolResultError(fmt.Sprintf("Query blocked: %s", err.Error())), nil
		}

		finalQuery := addRowLimitIfMissing(sqlQuery, connCfg.MaxRowsResolved)

		result, err := db.ExecuteQuery(ctx, finalQuery, QueryOptions{MaxRows: connCfg.MaxRowsResolved, Params: params})
		if err != nil {
			AuditLog(sqlQuery, false, 0, err)
			return mcp.NewToolResultError(fmt.Sprintf("Query failed: %s", err.Error())), nil
		}

		safeResult := MaskSensitiveColumns(result, connCfg.SensitiveColumnsMap)
		AuditLog(sqlQuery, true, safeResult.Count, nil)

		output, err := serializeResult(safeResult)
		if err != nil {
			return mcp.NewToolResultError("failed to serialize results"), nil
		}
		return mcp.NewToolResultText(output), nil
	}
}

func handleExecView(cm *ConnectionManager) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		db, connCfg, err := resolveDB(cm, req)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		view := strings.TrimSpace(req.GetString("view", ""))
		if view == "" {
			return mcp.NewToolResultError("view parameter is required"), nil
		}
		params := req.GetString("params", "")

		result, execSQL, err := validateAndExecView(ctx, db, connCfg, view, params, connCfg.MaxRowsResolved)
		if err != nil {
			AuditLog(execSQL, false, 0, err)
			return mcp.NewToolResultError(err.Error()), nil
		}

		safeResult := MaskSensitiveColumns(result, connCfg.SensitiveColumnsMap)
		AuditLog(execSQL, true, safeResult.Count, nil)

		output, err := serializeResult(safeResult)
		if err != nil {
			return mcp.NewToolResultError("failed to serialize results"), nil
		}
		return mcp.NewToolResultText(output), nil
	}
}

func handleExplain(cm *ConnectionManager) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		db, connCfg, err := resolveDB(cm, req)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		sqlQuery := strings.TrimSpace(req.GetString("sql", ""))
		if sqlQuery == "" {
			return mcp.NewToolResultError("sql parameter is required"), nil
		}
		if err := ValidateQuery(sqlQuery, connCfg.BlockedTablesMap); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("Query blocked: %s", err.Error())), nil
		}
		first := strings.ToUpper(firstToken(maskSQL(sqlQuery, true)))
		if first != "SELECT" && first != "WITH" {
			return mcp.NewToolResultError("explain_query only accepts SELECT/WITH queries"), nil
		}

		prefixes := map[string]string{
			"indexes":  "EXPLAIN indexes = 1 ",
			"plan":     "EXPLAIN PLAN ",
			"pipeline": "EXPLAIN PIPELINE ",
			"syntax":   "EXPLAIN SYNTAX ",
			"estimate": "EXPLAIN ESTIMATE ",
			"ast":      "EXPLAIN AST ",
		}
		kind := strings.ToLower(strings.TrimSpace(req.GetString("type", "indexes")))
		prefix, ok := prefixes[kind]
		if !ok {
			return mcp.NewToolResultError("type must be one of: indexes, plan, pipeline, syntax, estimate, ast"), nil
		}

		explainSQL := prefix + strings.TrimRight(sqlQuery, "; \t\r\n")
		result, err := db.ExecuteQuery(ctx, explainSQL, QueryOptions{})
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("EXPLAIN failed: %s", err.Error())), nil
		}
		log.Printf("[AUDIT] explain type=%s query=%q", kind, sqlQuery)

		// Single-column EXPLAIN output is a list of text lines; return it as a readable block.
		if len(result.Columns) == 1 {
			lines := make([]string, 0, result.Count)
			for _, row := range result.Rows {
				lines = append(lines, rowString(row, result.Columns[0]))
			}
			return mcp.NewToolResultText(strings.Join(lines, "\n")), nil
		}
		return textOrError(serializeResult(result))
	}
}

func handleBenchmark(cm *ConnectionManager) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		db, connCfg, err := resolveDB(cm, req)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		args := req.GetArguments()

		queryA, _ := args["query_a"].(string)
		if strings.TrimSpace(queryA) == "" {
			return mcp.NewToolResultError("query_a parameter is required"), nil
		}

		labelA, _ := args["label_a"].(string)
		if labelA == "" {
			labelA = "Query A"
		}

		iterations := req.GetInt("iterations", 1)
		if iterations < 1 {
			iterations = 1
		}
		if iterations > 20 {
			iterations = 20
		}

		if err := ValidateQuery(queryA, connCfg.BlockedTablesMap); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("%s blocked: %s", labelA, err.Error())), nil
		}

		resultA, err := runBenchmark(ctx, db, queryA, iterations)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("%s failed: %s", labelA, err.Error())), nil
		}
		resultA["label"] = labelA

		results := []map[string]interface{}{resultA}

		queryB, _ := args["query_b"].(string)
		if strings.TrimSpace(queryB) != "" {
			labelB, _ := args["label_b"].(string)
			if labelB == "" {
				labelB = "Query B"
			}

			if err := ValidateQuery(queryB, connCfg.BlockedTablesMap); err != nil {
				return mcp.NewToolResultError(fmt.Sprintf("%s blocked: %s", labelB, err.Error())), nil
			}

			resultB, err := runBenchmark(ctx, db, queryB, iterations)
			if err != nil {
				return mcp.NewToolResultError(fmt.Sprintf("%s failed: %s", labelB, err.Error())), nil
			}
			resultB["label"] = labelB

			results = append(results, resultB)

			durA := resultA["duration_ms"].(float64)
			durB := resultB["duration_ms"].(float64)
			comparison := map[string]interface{}{
				"faster":       labelA,
				"diff_ms":      round2(durB - durA),
				"diff_percent": percentDiff(durA, durB),
			}
			if durB < durA {
				comparison["faster"] = labelB
				comparison["diff_ms"] = round2(durA - durB)
				comparison["diff_percent"] = percentDiff(durB, durA)
			}

			out := map[string]interface{}{
				"benchmarks": results,
				"comparison": comparison,
			}
			jsonBytes, _ := json.MarshalIndent(out, "", "  ")
			return mcp.NewToolResultText(string(jsonBytes)), nil
		}

		jsonBytes, _ := json.MarshalIndent(resultA, "", "  ")
		return mcp.NewToolResultText(string(jsonBytes)), nil
	}
}

func runBenchmark(ctx context.Context, db *Database, query string, iterations int) (map[string]interface{}, error) {
	var total, minDur, maxDur time.Duration
	var rowCount int
	var columns []string
	var rowsRead, bytesRead uint64

	for i := 0; i < iterations; i++ {
		var runRows, runBytes uint64
		opts := QueryOptions{Progress: func(p *clickhouse.Progress) {
			runRows += p.Rows
			runBytes += p.Bytes
		}}

		start := time.Now()
		count, cols, err := db.CountRows(ctx, query, opts)
		duration := time.Since(start)
		if err != nil {
			return nil, err
		}

		total += duration
		if i == 0 || duration < minDur {
			minDur = duration
		}
		if duration > maxDur {
			maxDur = duration
		}
		rowCount, columns = count, cols
		rowsRead, bytesRead = runRows, runBytes
	}

	avg := total / time.Duration(iterations)
	out := map[string]interface{}{
		"row_count":   rowCount,
		"columns":     columns,
		"duration_ms": float64(avg.Microseconds()) / 1000.0,
		"duration":    avg.String(),
	}
	if iterations > 1 {
		out["iterations"] = iterations
		out["min_ms"] = float64(minDur.Microseconds()) / 1000.0
		out["max_ms"] = float64(maxDur.Microseconds()) / 1000.0
	}
	// Progress packets are only sent over the native protocol.
	if rowsRead > 0 || bytesRead > 0 {
		out["rows_read"] = rowsRead
		out["bytes_read"] = bytesRead
		out["bytes_read_readable"] = formatFileSize(int64(bytesRead))
	}
	return out, nil
}

func handleTableStats(cm *ConnectionManager) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		db, connCfg, err := resolveDB(cm, req)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		if err := checkSystemTable("parts", connCfg); err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		partsSummary := `
			SELECT database, table,
			       sum(rows) AS rows,
			       count() AS parts,
			       uniqExact(partition) AS partitions,
			       formatReadableSize(sum(data_compressed_bytes)) AS compressed,
			       formatReadableSize(sum(data_uncompressed_bytes)) AS uncompressed,
			       round(sum(data_uncompressed_bytes) / nullIf(sum(data_compressed_bytes), 0), 2) AS compression_ratio,
			       sum(data_compressed_bytes) AS compressed_bytes,
			       max(modification_time) AS last_modified
			FROM system.parts
			WHERE active AND %s
			GROUP BY database, table
			ORDER BY compressed_bytes DESC`

		tableName := strings.TrimSpace(req.GetString("table_name", ""))
		if tableName == "" {
			database := strings.TrimSpace(req.GetString("database", ""))
			where := "database NOT IN (" + quoteStringList(systemDatabases) + ")"
			if database != "" {
				where = "database = " + quoteString(database)
			}
			limit := req.GetInt("limit", 20)
			if limit < 1 {
				limit = 20
			}

			result, err := db.ExecuteQuery(ctx, fmt.Sprintf(partsSummary, where), QueryOptions{})
			if err != nil {
				return mcp.NewToolResultError(fmt.Sprintf("failed to read table stats: %s", err.Error())), nil
			}
			rows := make([]map[string]interface{}, 0, limit)
			for _, row := range result.Rows {
				if isTableBlocked(rowString(row, "database"), rowString(row, "table"), connCfg.BlockedTablesMap) {
					continue
				}
				if len(rows) >= limit {
					break
				}
				rows = append(rows, row)
			}
			return textOrError(serializeResult(&QueryResult{Columns: result.Columns, Rows: rows, Count: len(rows)}))
		}

		schema, table := splitQualifiedNameRaw(tableName)
		if table == "" {
			return mcp.NewToolResultError("invalid table_name"), nil
		}
		if isTableBlocked(schema, table, connCfg.BlockedTablesMap) {
			return mcp.NewToolResultError(fmt.Sprintf("table '%s' is not accessible", tableName)), nil
		}

		where := fmt.Sprintf("database = %s AND table = %s", databaseExpr(schema), quoteString(table))
		// Parts access is often restricted; per-column sizes from system.columns are still useful without it.
		summary, partsErr := db.ExecuteQuery(ctx, fmt.Sprintf(partsSummary, where), QueryOptions{})

		columns, err := db.ExecuteQuery(ctx, `
			SELECT name AS column_name, type, compression_codec,
			       formatReadableSize(data_compressed_bytes) AS compressed,
			       formatReadableSize(data_uncompressed_bytes) AS uncompressed,
			       round(data_uncompressed_bytes / nullIf(data_compressed_bytes, 0), 2) AS compression_ratio
			FROM system.columns
			WHERE `+where+`
			ORDER BY data_compressed_bytes DESC
		`, QueryOptions{})
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to read column stats: %s", err.Error())), nil
		}
		columns = filterSensitiveColumnMetadata(columns, connCfg.SensitiveColumnsMap)

		out := map[string]interface{}{
			"columns": columns.Rows,
		}
		switch {
		case partsErr != nil:
			out["note"] = "Part-level stats unavailable: " + partsErr.Error()
		case summary.Count > 0:
			out["table"] = summary.Rows[0]
		default:
			out["note"] = "No active parts found. The table may be empty, a view, or use a non-MergeTree engine."
		}
		return textOrError(serializeAny(out))
	}
}

func handleRunningQueries(cm *ConnectionManager) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		db, connCfg, err := resolveDB(cm, req)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		if err := checkSystemTable("processes", connCfg); err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		result, err := db.ExecuteQuery(ctx, `
			SELECT query_id, user, round(elapsed, 2) AS elapsed_sec,
			       read_rows, formatReadableSize(read_bytes) AS read,
			       formatReadableSize(memory_usage) AS memory,
			       substring(query, 1, 500) AS query
			FROM system.processes
			WHERE query_id != queryID()
			ORDER BY elapsed DESC
		`, QueryOptions{MaxRows: 200})
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to read running queries: %s", err.Error())), nil
		}
		return textOrError(serializeResult(result))
	}
}

func handleQueryHistory(cm *ConnectionManager) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		db, connCfg, err := resolveDB(cm, req)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		if err := checkSystemTable("query_log", connCfg); err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		minutes := req.GetInt("minutes", 60)
		if minutes < 1 {
			minutes = 60
		}
		minDuration := req.GetInt("min_duration_ms", 0)
		if minDuration < 0 {
			minDuration = 0
		}
		limit := req.GetInt("limit", 20)
		if limit < 1 || limit > 200 {
			limit = 20
		}

		orderBy := map[string]string{
			"duration":   "query_duration_ms DESC",
			"memory":     "memory_usage DESC",
			"read_bytes": "read_bytes DESC",
			"recent":     "event_time DESC",
		}[strings.ToLower(req.GetString("sort", "duration"))]
		if orderBy == "" {
			orderBy = "query_duration_ms DESC"
		}

		typeFilter := "type IN ('QueryFinish', 'ExceptionBeforeStart', 'ExceptionWhileProcessing')"
		if req.GetBool("failed_only", false) {
			typeFilter = "type IN ('ExceptionBeforeStart', 'ExceptionWhileProcessing')"
		}

		query := fmt.Sprintf(`
			SELECT event_time, query_duration_ms, query_kind, user,
			       read_rows, formatReadableSize(read_bytes) AS read,
			       result_rows, formatReadableSize(memory_usage) AS memory,
			       substring(query, 1, 1000) AS query,
			       if(exception = '', NULL, substring(exception, 1, 500)) AS exception
			FROM system.query_log
			WHERE event_date >= toDate(now() - INTERVAL %d MINUTE)
			  AND event_time >= now() - INTERVAL %d MINUTE
			  AND %s
			  AND query_duration_ms >= %d
			  AND is_initial_query
			ORDER BY %s
			LIMIT %d`, minutes, minutes, typeFilter, minDuration, orderBy, limit)

		result, err := db.ExecuteQuery(ctx, query, QueryOptions{MaxRows: limit})
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to read query log: %s", err.Error())), nil
		}
		return textOrError(serializeResult(result))
	}
}

func serializeResult(result *QueryResult) (string, error) {
	if OutputFormat == "toon" {
		return result.ToTOON(), nil
	}
	jsonBytes, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return "", err
	}
	return string(jsonBytes), nil
}

func textOrError(output string, err error) (*mcp.CallToolResult, error) {
	if err != nil {
		return mcp.NewToolResultError("failed to serialize results: " + err.Error()), nil
	}
	return mcp.NewToolResultText(output), nil
}

// addRowLimitIfMissing appends LIMIT to SELECT/WITH queries that have no top-level row limit.
// LIMIT n BY col does not cap rows and is ignored. If the query ends with a SETTINGS clause,
// LIMIT is inserted before it. scanRows enforces the same cap client-side regardless.
func addRowLimitIfMissing(query string, limit int) string {
	if limit <= 0 {
		return query
	}
	trimmed := strings.TrimSpace(query)
	masked := maskSQL(trimmed, true)

	first := strings.ToUpper(firstToken(masked))
	if first != "SELECT" && first != "WITH" {
		return query
	}

	top := topLevelOnly(masked)
	if hasTopLevelLimit(top) {
		return query
	}

	// Drop trailing semicolons and comments so the LIMIT isn't swallowed by a trailing "-- comment".
	end := len(masked)
	for end > 0 {
		c := masked[end-1]
		if c == ';' || c == ' ' || c == '\t' || c == '\r' || c == '\n' {
			end--
			continue
		}
		break
	}
	body := trimmed[:end]

	limitClause := fmt.Sprintf("LIMIT %d", limit)
	if loc := topLevelSettingsPattern.FindStringIndex(top[:end]); loc != nil {
		return strings.TrimRight(body[:loc[0]], " \t\r\n") + " " + limitClause + " " + body[loc[0]:]
	}
	return body + " " + limitClause
}

func hasTopLevelLimit(top string) bool {
	if fetchOrTopPattern.MatchString(top) {
		return true
	}
	for _, loc := range limitKeywordPattern.FindAllStringIndex(top, -1) {
		if !limitByPattern.MatchString(top[loc[1]:]) {
			return true
		}
	}
	return false
}

// parseQueryParams accepts {name: value} as an object or a JSON string and converts values
// to the string form clickhouse-go sends for server-side {name:Type} binding.
func parseQueryParams(raw interface{}) (map[string]string, error) {
	var obj map[string]interface{}
	switch v := raw.(type) {
	case nil:
		return nil, nil
	case map[string]interface{}:
		obj = v
	case string:
		if strings.TrimSpace(v) == "" {
			return nil, nil
		}
		if err := json.Unmarshal([]byte(v), &obj); err != nil {
			return nil, fmt.Errorf("params must be a JSON object like {\"id\": 1}: %s", err.Error())
		}
	default:
		return nil, fmt.Errorf("params must be a JSON object like {\"id\": 1}")
	}

	params := make(map[string]string, len(obj))
	for key, val := range obj {
		switch v := val.(type) {
		case string:
			params[key] = v
		case nil:
			// Top-level NULL is sent as the \N marker; the NULL keyword would be read as text.
			params[key] = `\N`
		case float64:
			if v == float64(int64(v)) {
				params[key] = fmt.Sprintf("%d", int64(v))
			} else {
				params[key] = fmt.Sprintf("%v", v)
			}
		case bool:
			if v {
				params[key] = "1"
			} else {
				params[key] = "0"
			}
		default:
			// Arrays/maps use ClickHouse literal syntax, which matches JSON for simple values.
			data, err := json.Marshal(v)
			if err != nil {
				return nil, fmt.Errorf("param %q: %s", key, err.Error())
			}
			params[key] = strings.ReplaceAll(string(data), `"`, `'`)
		}
	}
	return params, nil
}

func checkSystemTable(table string, connCfg *ConnectionConfig) error {
	if isTableBlocked("system", table, connCfg.BlockedTablesMap) {
		return fmt.Errorf("access to table 'system.%s' is not permitted", table)
	}
	return nil
}

// quoteString returns a ClickHouse string literal.
func quoteString(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `'`, `\'`)
	return "'" + s + "'"
}

func quoteStringList(items []string) string {
	quoted := make([]string, len(items))
	for i, item := range items {
		quoted[i] = quoteString(item)
	}
	return strings.Join(quoted, ", ")
}

// quoteIdent returns a backtick-quoted ClickHouse identifier.
func quoteIdent(identifier string) string {
	identifier = strings.ReplaceAll(identifier, `\`, `\\`)
	return "`" + strings.ReplaceAll(identifier, "`", "\\`") + "`"
}

func qualifiedIdent(schema, name string) string {
	if schema == "" {
		return quoteIdent(name)
	}
	return quoteIdent(schema) + "." + quoteIdent(name)
}

// databaseExpr returns the database literal, or currentDatabase() when unqualified.
func databaseExpr(schema string) string {
	if schema == "" {
		return "currentDatabase()"
	}
	return quoteString(schema)
}

func rowString(row map[string]interface{}, key string) string {
	val, ok := row[key]
	if !ok || val == nil {
		return ""
	}
	return fmt.Sprintf("%v", val)
}

func filterSensitiveColumnMetadata(result *QueryResult, sensitiveColumns map[string]bool) *QueryResult {
	if result == nil || len(sensitiveColumns) == 0 {
		return result
	}
	safeRows := make([]map[string]interface{}, 0, len(result.Rows))
	for _, row := range result.Rows {
		colName := strings.ToLower(rowString(row, "column_name"))
		if colName != "" && sensitiveColumns[colName] {
			log.Printf("[SECURITY] omitted sensitive column metadata: %s", colName)
			continue
		}
		safeRows = append(safeRows, row)
	}
	return &QueryResult{Columns: result.Columns, Rows: safeRows, Count: len(safeRows)}
}

// filterCreateStatement removes column definition lines for sensitive columns from DDL.
func filterCreateStatement(ddl string, sensitiveColumns map[string]bool) string {
	if len(sensitiveColumns) == 0 {
		return ddl
	}
	lines := strings.Split(ddl, "\n")
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		var ident string
		if strings.HasPrefix(trimmed, "`") {
			if end := strings.Index(trimmed[1:], "`"); end >= 0 {
				ident = trimmed[1 : end+1]
			}
		} else if fields := strings.Fields(trimmed); len(fields) > 0 {
			ident = fields[0]
		}
		if ident != "" && sensitiveColumns[strings.ToLower(ident)] {
			log.Printf("[SECURITY] omitted sensitive column from DDL: %s", ident)
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

func percentDiff(faster, slower float64) string {
	if faster <= 0 {
		return "n/a"
	}
	return fmt.Sprintf("%.1f%%", ((slower-faster)/faster)*100)
}

func round2(f float64) float64 {
	return float64(int64(f*100+0.5)) / 100
}
