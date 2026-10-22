package main

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

var ExportMaxRows = 10000
var ExportDirOverride string

var (
	fromClauseRe    = regexp.MustCompile("(?i)\\bFROM\\s+(?:[`\"]?([A-Za-z_][\\w]*)[`\"]?\\.)?[`\"]?([A-Za-z_][\\w]*)[`\"]?")
	validViewNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(\.[A-Za-z_][A-Za-z0-9_]*)?$`)
	viewParamRe     = regexp.MustCompile(`\{\s*([A-Za-z_]\w*)\s*:\s*([^{}]+?)\s*\}`)
	safeFilenameRe  = regexp.MustCompile(`[^\w\-]`)
)

var viewEngines = map[string]bool{"View": true, "MaterializedView": true, "LiveView": true, "WindowView": true}

// validateAndExecView runs SELECT * FROM a (possibly parameterized) view after checking the view
// exists, isn't blocked and that required parameters were supplied.
func validateAndExecView(ctx context.Context, db *Database, connCfg *ConnectionConfig, viewName, params string, maxRows int) (*QueryResult, string, error) {
	if !validViewNameRe.MatchString(viewName) {
		return nil, "", fmt.Errorf("invalid view name; use 'view' or 'database.view'")
	}
	if err := ValidateViewParams(params); err != nil {
		return nil, "", err
	}
	if !balancedParens(maskSQL(params, true)) {
		return nil, "", fmt.Errorf("view parameters have unbalanced parentheses")
	}

	schema, name := splitQualifiedNameRaw(viewName)
	if isTableBlocked(schema, name, connCfg.BlockedTablesMap) {
		return nil, "", fmt.Errorf("view '%s' is not accessible", viewName)
	}

	info, err := db.ExecuteQuery(ctx, fmt.Sprintf(
		"SELECT database, name, engine, create_table_query FROM system.tables WHERE database = %s AND name = %s",
		databaseExpr(schema), quoteString(name)), QueryOptions{})
	if err != nil {
		return nil, "", fmt.Errorf("failed to inspect view: %w", err)
	}
	if info.Count == 0 {
		return nil, "", fmt.Errorf("view '%s' not found or not accessible", viewName)
	}

	row := info.Rows[0]
	engine := rowString(row, "engine")
	if !viewEngines[engine] {
		return nil, "", fmt.Errorf("'%s' is a %s table, not a view; use query_database instead", viewName, engine)
	}

	required := extractViewParameters(rowString(row, "create_table_query"))
	params = strings.TrimSpace(params)
	if len(required) > 0 && params == "" {
		names := make([]string, 0, len(required))
		for _, p := range required {
			names = append(names, fmt.Sprintf("%s (%s)", p["name"], p["type"]))
		}
		return nil, "", fmt.Errorf("view '%s' requires parameters: %s. Example params: \"%s=...\"",
			viewName, strings.Join(names, ", "), required[0]["name"])
	}

	target := qualifiedIdent(rowString(row, "database"), rowString(row, "name"))
	execSQL := "SELECT * FROM " + target
	if params != "" {
		execSQL += "(" + params + ")"
	}

	// Re-validate the assembled statement so parameters can't smuggle in blocked tables or functions.
	if err := ValidateQuery(execSQL, connCfg.BlockedTablesMap); err != nil {
		return nil, execSQL, fmt.Errorf("view call blocked: %w", err)
	}

	execSQL = addRowLimitIfMissing(execSQL, maxRows)
	log.Printf("[AUDIT] view validated, executing: %s", execSQL)

	result, err := db.ExecuteQuery(ctx, execSQL, QueryOptions{MaxRows: maxRows})
	if err != nil {
		return nil, execSQL, fmt.Errorf("view execution failed: %w", err)
	}
	return result, execSQL, nil
}

// extractViewParameters returns the {name:Type} placeholders declared in a view definition.
func extractViewParameters(createQuery string) []map[string]interface{} {
	if createQuery == "" {
		return nil
	}
	seen := make(map[string]bool)
	var params []map[string]interface{}
	for _, m := range viewParamRe.FindAllStringSubmatch(createQuery, -1) {
		if seen[m[1]] {
			continue
		}
		seen[m[1]] = true
		params = append(params, map[string]interface{}{"name": m[1], "type": m[2]})
	}
	return params
}

func balancedParens(s string) bool {
	depth := 0
	for _, c := range s {
		switch c {
		case '(':
			depth++
		case ')':
			depth--
			if depth < 0 {
				return false
			}
		}
	}
	return depth == 0
}

func handleExport(cm *ConnectionManager) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		db, connCfg, err := resolveDB(cm, req)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		args := req.GetArguments()

		sqlQuery, _ := args["sql"].(string)
		view, _ := args["view"].(string)
		params, _ := args["params"].(string)
		jsonColumnsRaw, _ := args["json_columns"].(string)
		sourceColumn, _ := args["source_column"].(string)
		customFilename, _ := args["filename"].(string)
		format, _ := args["format"].(string)

		sqlQuery = strings.TrimSpace(sqlQuery)
		view = strings.TrimSpace(view)
		sourceColumn = strings.TrimSpace(sourceColumn)
		format = strings.ToLower(strings.TrimSpace(format))
		if format == "" {
			format = "json"
		}

		if format != "json" && format != "ndjson" && format != "csv" {
			return mcp.NewToolResultError("format must be one of: json, ndjson, csv"), nil
		}
		if sqlQuery == "" && view == "" {
			return mcp.NewToolResultError("either 'sql' or 'view' parameter is required"), nil
		}
		if sqlQuery != "" && view != "" {
			return mcp.NewToolResultError("provide either 'sql' or 'view', not both"), nil
		}
		if format == "csv" && sourceColumn != "" {
			return mcp.NewToolResultError("source_column is only supported for json and ndjson exports"), nil
		}

		queryParams, err := parseQueryParams(args["query_params"])
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		var jsonColumns map[string]bool
		if strings.TrimSpace(jsonColumnsRaw) != "" {
			jsonColumns = make(map[string]bool)
			for _, col := range strings.Split(jsonColumnsRaw, ",") {
				col = strings.TrimSpace(col)
				if col != "" {
					jsonColumns[strings.ToLower(col)] = true
				}
			}
		}

		var result *QueryResult
		var sourceName string
		var auditLabel string

		if sqlQuery != "" {
			if err := ValidateQuery(sqlQuery, connCfg.BlockedTablesMap); err != nil {
				AuditLog("EXPORT: "+sqlQuery, false, 0, err)
				return mcp.NewToolResultError(fmt.Sprintf("Query blocked: %s", err.Error())), nil
			}

			finalQuery := addRowLimitIfMissing(sqlQuery, ExportMaxRows)

			result, err = db.ExecuteQuery(ctx, finalQuery, QueryOptions{MaxRows: ExportMaxRows, Params: queryParams})
			if err != nil {
				AuditLog("EXPORT: "+sqlQuery, false, 0, err)
				return mcp.NewToolResultError(fmt.Sprintf("Query failed: %s", err.Error())), nil
			}
			sourceName = extractTableName(sqlQuery)
			auditLabel = "EXPORT: " + sqlQuery
		} else {
			result, auditLabel, err = validateAndExecView(ctx, db, connCfg, view, params, ExportMaxRows)
			auditLabel = "EXPORT: " + auditLabel
			if err != nil {
				AuditLog(auditLabel, false, 0, err)
				return mcp.NewToolResultError(err.Error()), nil
			}
			_, sourceName = splitQualifiedName(view)
		}

		result = MaskSensitiveColumns(result, connCfg.SensitiveColumnsMap)

		if sourceColumn != "" {
			found := false
			for _, col := range result.Columns {
				if strings.EqualFold(col, sourceColumn) {
					sourceColumn = col
					found = true
					break
				}
			}
			if !found {
				return mcp.NewToolResultError(fmt.Sprintf("source_column '%s' not found in result columns: %v", sourceColumn, result.Columns)), nil
			}
		}

		var data []byte
		var exportCount, skippedCount int

		switch format {
		case "csv":
			data, err = buildCSV(result)
			exportCount = result.Count
		default:
			var items []interface{}
			if sourceColumn != "" {
				items, exportCount, skippedCount = buildSourceColumnOutput(result, sourceColumn)
			} else {
				items, exportCount = buildNormalOutput(result, jsonColumns)
			}
			if format == "ndjson" {
				data, err = buildNDJSON(items)
			} else {
				data, err = json.MarshalIndent(items, "", "  ")
			}
		}
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to encode %s: %s", format, err.Error())), nil
		}

		filename := buildFilename(customFilename, sourceName, format)
		outDir := getExportDir()
		if err := os.MkdirAll(outDir, 0755); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to create output directory: %s", err.Error())), nil
		}

		filePath := filepath.Join(outDir, filename)
		absOut, _ := filepath.Abs(outDir)
		absFile, _ := filepath.Abs(filePath)
		if !strings.HasPrefix(absFile, absOut+string(os.PathSeparator)) && absFile != absOut {
			return mcp.NewToolResultError("filename must not contain path traversal characters"), nil
		}

		// Never overwrite: the tool is annotated as non-destructive.
		if _, err := os.Stat(filePath); err == nil {
			return mcp.NewToolResultError(fmt.Sprintf("file '%s' already exists; choose a different filename", filename)), nil
		}
		if err := os.WriteFile(filePath, data, 0644); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to write file: %s", err.Error())), nil
		}

		fileSize := formatFileSize(int64(len(data)))

		AuditLog(auditLabel, true, exportCount, nil)

		summary := fmt.Sprintf("Exported successfully!\nFile: %s\nFormat: %s\nRows exported: %d\nFile size: %s", absFile, format, exportCount, fileSize)
		if skippedCount > 0 {
			summary += fmt.Sprintf("\nSkipped (invalid JSON): %d", skippedCount)
		}
		if exportCount == 0 {
			summary += "\nWarning: query returned 0 rows"
		}
		if result.Truncated || (ExportMaxRows > 0 && result.Count >= ExportMaxRows) {
			summary += fmt.Sprintf("\nNote: result was limited to %d rows (export_max_rows setting)", ExportMaxRows)
		}

		log.Printf("[EXPORT] %s -> %s (%d rows, %s)", sourceName, absFile, exportCount, fileSize)
		return mcp.NewToolResultText(summary), nil
	}
}

func buildNormalOutput(result *QueryResult, explicitJSONColumns map[string]bool) ([]interface{}, int) {
	rows := make([]interface{}, 0, len(result.Rows))

	for _, row := range result.Rows {
		outRow := make(map[string]interface{}, len(row))
		for key, val := range row {
			if shouldParseAsJSON(key, val, explicitJSONColumns) {
				if parsed, ok := tryParseJSON(val); ok {
					outRow[key] = parsed
					continue
				}
			}
			outRow[key] = val
		}
		rows = append(rows, outRow)
	}
	return rows, len(rows)
}

func buildSourceColumnOutput(result *QueryResult, sourceColumn string) ([]interface{}, int, int) {
	output := make([]interface{}, 0)
	skipped := 0

	for _, row := range result.Rows {
		val, exists := row[sourceColumn]
		if !exists || val == nil {
			skipped++
			continue
		}

		// Native ClickHouse Map/Tuple/Array/JSON values arrive already decoded.
		switch v := val.(type) {
		case []interface{}:
			output = append(output, v...)
			continue
		case map[string]interface{}:
			output = append(output, v)
			continue
		}

		str, ok := val.(string)
		if !ok {
			skipped++
			continue
		}

		str = strings.TrimSpace(str)
		if str == "" {
			skipped++
			continue
		}

		var arr []interface{}
		if err := json.Unmarshal([]byte(str), &arr); err == nil {
			output = append(output, arr...)
			continue
		}

		var obj map[string]interface{}
		if err := json.Unmarshal([]byte(str), &obj); err == nil {
			output = append(output, obj)
			continue
		}

		skipped++
	}

	return output, len(output), skipped
}

func buildNDJSON(items []interface{}) ([]byte, error) {
	var buf bytes.Buffer
	for _, item := range items {
		line, err := json.Marshal(item)
		if err != nil {
			return nil, err
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	return buf.Bytes(), nil
}

func buildCSV(result *QueryResult) ([]byte, error) {
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	if err := w.Write(result.Columns); err != nil {
		return nil, err
	}
	record := make([]string, len(result.Columns))
	for _, row := range result.Rows {
		for i, col := range result.Columns {
			record[i] = csvCell(row[col])
		}
		if err := w.Write(record); err != nil {
			return nil, err
		}
	}
	w.Flush()
	return buf.Bytes(), w.Error()
}

func csvCell(v interface{}) string {
	switch val := v.(type) {
	case nil:
		return ""
	case string:
		return val
	case []interface{}, map[string]interface{}:
		data, err := json.Marshal(val)
		if err != nil {
			return fmt.Sprintf("%v", val)
		}
		return string(data)
	default:
		return fmt.Sprintf("%v", val)
	}
}

func shouldParseAsJSON(colName string, val interface{}, explicitColumns map[string]bool) bool {
	if explicitColumns != nil {
		return explicitColumns[strings.ToLower(colName)]
	}
	str, ok := val.(string)
	if !ok {
		return false
	}
	str = strings.TrimSpace(str)
	return len(str) > 0 && (str[0] == '{' || str[0] == '[')
}

func tryParseJSON(val interface{}) (interface{}, bool) {
	str, ok := val.(string)
	if !ok {
		return nil, false
	}
	str = strings.TrimSpace(str)
	if str == "" {
		return nil, false
	}

	var parsed interface{}
	if err := json.Unmarshal([]byte(str), &parsed); err != nil {
		return nil, false
	}
	return parsed, true
}

func extractTableName(sql string) string {
	matches := fromClauseRe.FindStringSubmatch(sql)
	if len(matches) >= 3 && matches[2] != "" {
		return cleanIdentifier(matches[2])
	}
	return "export"
}

func buildFilename(custom, sourceName, format string) string {
	timestamp := time.Now().Format("20060102_150405")
	ext := "." + format

	if custom != "" {
		custom = filepath.Base(strings.TrimSpace(custom))
		if !strings.HasSuffix(strings.ToLower(custom), ext) {
			custom += ext
		}
		return custom
	}

	if sourceName == "" {
		sourceName = "export"
	}

	safeName := safeFilenameRe.ReplaceAllString(sourceName, "_")
	return fmt.Sprintf("%s_%s%s", safeName, timestamp, ext)
}

func getExportDir() string {
	if ExportDirOverride != "" {
		return ExportDirOverride
	}
	if exePath, err := os.Executable(); err == nil {
		return filepath.Join(filepath.Dir(exePath), "exportDatabaseSql")
	}
	return "exportDatabaseSql"
}

func formatFileSize(bytes int64) string {
	const (
		KB = 1024
		MB = KB * 1024
		GB = MB * 1024
	)
	switch {
	case bytes >= GB:
		return fmt.Sprintf("%.2f GB", float64(bytes)/float64(GB))
	case bytes >= MB:
		return fmt.Sprintf("%.2f MB", float64(bytes)/float64(MB))
	case bytes >= KB:
		return fmt.Sprintf("%.2f KB", float64(bytes)/float64(KB))
	default:
		return fmt.Sprintf("%d bytes", bytes)
	}
}
