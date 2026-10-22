package main

import (
	"fmt"
	"io"
	"log"
	"os"

	"github.com/mark3labs/mcp-go/server"
)

const version = "1.0.0"

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "--version":
			fmt.Println("mcp-clickhouse " + version)
			return
		case "--check":
			os.Exit(runCheck())
		}
	}

	LoadConfig()

	// Support env var fallback for single-connection mode.
	if defConn, ok := LoadedConfig.Connections[DefaultConnectionName]; ok && !defConn.HasTarget() {
		if dsn := os.Getenv("CLICKHOUSE_DSN"); dsn != "" {
			log.Println("[CONNECT] Using CLICKHOUSE_DSN env var for default connection")
			defConn.DSN = dsn
		}
	}

	hasServer := false
	for _, conn := range LoadedConfig.Connections {
		if conn.HasTarget() {
			hasServer = true
			break
		}
	}
	if !hasServer {
		log.Fatal("No database credentials found. Set credentials in .mcp-clickhouse-config.json or CLICKHOUSE_DSN env var")
	}

	cm := NewConnectionManager(LoadedConfig.Connections, DefaultConnectionName)
	defer cm.Close()

	// Eagerly connect the default connection so bad config fails at startup.
	if DefaultConnectionName != "" {
		_, _, err := cm.GetConnection("")
		if err != nil {
			log.Fatalf("Failed to connect default connection '%s': %v", DefaultConnectionName, err)
		}
		log.Println("Connected to ClickHouse successfully")
	}

	s := server.NewMCPServer("mcp-clickhouse", version,
		server.WithInstructions(`You are connected to one or more ClickHouse servers via mcp-clickhouse.
You can only READ data. No inserts, mutations, DDL or system commands are allowed by this MCP server.
Always be conservative with row counts. ClickHouse tables are often huge: filter on the partition key
and sorting key, aggregate on the server, and start with small limits.
Some tables and columns may be restricted for security reasons. Respect those boundaries.

Multi-connection support:
- Use list_connections to see all available connections
- Pass the "connection" parameter to any DB tool to target a specific connection
- If you omit "connection", the default connection is used

Available tools:
- list_connections: See all configured connections and which is default
- list_databases: See databases on the server
- list_tables: See tables with engine, row count and size (optionally for one database)
- describe_table: Columns, types, codecs, partition/sorting/primary keys, and view parameters
- show_create_table: Full CREATE statement of a table, view or dictionary
- query_database: Execute SELECT/WITH/SHOW/DESCRIBE/EXPLAIN/EXISTS queries.
  Use {name:Type} placeholders with the "params" object for values, e.g.
  sql: "SELECT count() FROM events WHERE user_id = {uid:UInt64}", params: {"uid": 42}
- exec_view: Query a (parameterized) view, e.g. exec_view(view: "reports.daily_sales", params: "day='2026-01-01'")
- explain_query: See the execution plan and whether indexes/partitions are used, without running the query
- benchmark_query: Compare query performance (time, rows/bytes read) without returning data
- table_stats: Storage size, compression ratio and parts per table, plus per-column sizes
- running_queries: Queries currently running on the server
- query_history: Slow, memory-heavy or failed recent queries from system.query_log
- export_query: Export query or view results to a JSON, NDJSON or CSV file

Guidelines:
- Explore with list_tables and describe_table before writing queries
- Use explain_query (type "indexes") to check that a heavy query uses the primary key before running it
- For performance comparison, use benchmark_query
- For exporting data to files, use export_query`),
	)

	registerTools(s, cm)

	log.Printf("mcp-clickhouse v%s starting (stdio transport)...", version)
	if err := server.ServeStdio(s); err != nil {
		log.Fatalf("Server error: %v", err)
	}
}

// runCheck connects every configured connection and prints one result line per connection.
// Used by the team setup script to verify credentials. Returns the process exit code.
func runCheck() int {
	log.SetOutput(io.Discard)
	LoadConfig()

	cm := NewConnectionManager(LoadedConfig.Connections, DefaultConnectionName)
	defer cm.Close()

	failed := 0
	for _, name := range cm.ListConnections() {
		if !LoadedConfig.Connections[name].HasTarget() {
			fmt.Printf("FAIL %s: no server configured\n", name)
			failed++
			continue
		}
		db, _, err := cm.GetConnection(name)
		if err != nil {
			fmt.Printf("FAIL %s: %v\n", name, err)
			failed++
			continue
		}
		fmt.Printf("OK   %s: ClickHouse %s (readonly=%d)\n", name, db.ServerVersion, db.ReadonlyLevel)
	}
	if failed > 0 {
		return 1
	}
	return 0
}
