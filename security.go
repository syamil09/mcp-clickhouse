package main

import (
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"
	"unicode"
)

// Statements that may start a query. Everything else (INSERT, SYSTEM, SET, USE, KILL, ...) is rejected
// by the prefix check before keyword scanning.
var allowedStatementPrefixes = []string{"SELECT", "WITH", "SHOW", "DESCRIBE", "DESC", "EXPLAIN", "EXISTS"}

var dangerousPattern = regexp.MustCompile(
	`(?i)\b(INSERT|UPDATE|DELETE|DROP|ALTER|CREATE|TRUNCATE|RENAME|OPTIMIZE|ATTACH|DETACH|KILL|GRANT|REVOKE|EXCHANGE|UNDROP|BACKUP|RESTORE|INTO|OUTFILE)\b`,
)

// Table functions that read files, the network or other servers.
var externalFunctionPattern = regexp.MustCompile(
	`(?i)\b(file|fileCluster|url|urlCluster|s3|s3Cluster|gcs|hdfs|hdfsCluster|azureBlobStorage|azureBlobStorageCluster|mysql|postgresql|mongodb|redis|sqlite|jdbc|odbc|executable|executablePool|input|remote|remoteSecure|deltaLake|deltaLakeCluster|iceberg|icebergS3|icebergAzure|icebergHDFS|icebergCluster|hudi|hudiCluster)\s*\(`,
)

// Table functions that can read arbitrary tables by name or regex, bypassing the table blocklist.
var blocklistBypassPattern = regexp.MustCompile(`(?i)\b(merge|cluster|clusterAllReplicas)\s*\(`)

// Settings this server enforces; queries may not override them with a SETTINGS clause.
var protectedSettingsPattern = regexp.MustCompile(
	`(?i)\b(readonly|max_result_rows|result_overflow_mode|max_execution_time)\s*=`,
)

// A FORMAT clause would replace the native result format the driver depends on.
var formatClausePattern = regexp.MustCompile(`(?i)\bFORMAT\s+[A-Za-z]\w*\s*(SETTINGS\b[\s\S]*)?;?\s*$`)

var accessControlShowPattern = regexp.MustCompile(
	`(?i)^\s*SHOW\s+(CREATE\s+)?(USERS?|ROLES?|CURRENT\s+ROLES|ENABLED\s+ROLES|GRANTS|ACCESS|QUOTAS?|PROFILES?|SETTINGS\s+PROFILES?|ROW\s+POLICY|ROW\s+POLICIES|POLICY|POLICIES|PRIVILEGES)\b`,
)

var showCreatePrefixPattern = regexp.MustCompile(`(?i)^\s*SHOW\s+CREATE\b`)

var paramDangerousPattern = regexp.MustCompile(
	`(?i)\b(SELECT|WITH|FROM|JOIN|UNION|INSERT|UPDATE|DELETE|DROP|ALTER|CREATE|TRUNCATE|RENAME|OPTIMIZE|ATTACH|DETACH|KILL|GRANT|REVOKE|SYSTEM|SETTINGS|FORMAT|INTO)\b`,
)

func ValidateQuery(sql string, blockedTables map[string]bool) error {
	normalized := strings.TrimSpace(sql)
	if normalized == "" {
		return fmt.Errorf("query is empty")
	}

	// masked hides literal/comment/quoted-identifier contents so keywords inside them don't trip checks.
	masked := maskSQL(normalized, true)
	// identMasked keeps identifiers readable for table blocklist checks.
	identMasked := maskSQL(normalized, false)

	firstWord := strings.ToUpper(firstToken(masked))
	allowed := false
	for _, prefix := range allowedStatementPrefixes {
		if firstWord == prefix {
			allowed = true
			break
		}
	}
	if !allowed {
		return fmt.Errorf("only SELECT/WITH/SHOW/DESCRIBE/EXPLAIN/EXISTS queries are allowed, got: %.20s...", normalized)
	}

	if hasMultipleStatements(masked) {
		return fmt.Errorf("multiple SQL statements are not allowed")
	}

	if accessControlShowPattern.MatchString(masked) {
		return fmt.Errorf("SHOW statements for users, roles, grants, quotas and policies are not allowed")
	}

	// SHOW CREATE TABLE is a read; don't let its CREATE keyword trip the keyword scan.
	keywordScan := masked
	if loc := showCreatePrefixPattern.FindStringIndex(keywordScan); loc != nil {
		keywordScan = keywordScan[loc[1]:]
	}
	if match := dangerousPattern.FindString(keywordScan); match != "" {
		return fmt.Errorf("query contains forbidden keyword: %s", strings.ToUpper(strings.TrimSpace(match)))
	}

	if match := externalFunctionPattern.FindStringSubmatch(masked); match != nil {
		return fmt.Errorf("table function '%s' is not allowed (reads files, network or other servers)", match[1])
	}

	if len(blockedTables) > 0 {
		if match := blocklistBypassPattern.FindStringSubmatch(masked); match != nil {
			return fmt.Errorf("table function '%s' is not allowed while a table blocklist is configured", match[1])
		}
	}

	if match := protectedSettingsPattern.FindString(masked); match != "" {
		return fmt.Errorf("query may not override server-enforced setting: %s", match)
	}

	if formatClausePattern.MatchString(topLevelOnly(masked)) {
		return fmt.Errorf("FORMAT clause is not allowed; results are formatted by the MCP server")
	}

	if len(blockedTables) > 0 {
		if err := checkTableAccess(identMasked, blockedTables); err != nil {
			return err
		}
	}

	return nil
}

func firstToken(s string) string {
	fields := strings.FieldsFunc(s, func(r rune) bool {
		return unicode.IsSpace(r) || r == '(' || r == ';'
	})
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

func hasMultipleStatements(maskedSQL string) bool {
	trimmed := strings.TrimSpace(maskedSQL)
	for strings.HasSuffix(trimmed, ";") {
		trimmed = strings.TrimSpace(strings.TrimSuffix(trimmed, ";"))
	}
	return strings.Contains(trimmed, ";")
}

// maskSQL returns a copy of sql with identical byte length where comments and the contents of
// string literals are blanked. When maskIdents is true, quoted identifiers ("x" and `x`) are
// replaced with underscores so they still read as a single word.
// Handles ClickHouse syntax: backslash escapes, doubled quotes, --, #, /* */ comments and $tag$ heredocs.
func maskSQL(sql string, maskIdents bool) string {
	out := []byte(sql)
	n := len(sql)

	blank := func(from, to int, ch byte) {
		for k := from; k < to && k < n; k++ {
			if out[k] != '\n' {
				out[k] = ch
			}
		}
	}

	for i := 0; i < n; i++ {
		c := sql[i]

		switch {
		case c == '-' && i+1 < n && sql[i+1] == '-', c == '#':
			start := i
			for i < n && sql[i] != '\n' {
				i++
			}
			blank(start, i, ' ')

		case c == '/' && i+1 < n && sql[i+1] == '*':
			start := i
			i += 2
			for i+1 < n && !(sql[i] == '*' && sql[i+1] == '/') {
				i++
			}
			end := i + 2
			if end > n {
				end = n
			}
			blank(start, end, ' ')
			i = end - 1

		case c == '\'' || c == '"' || c == '`':
			start := i
			i++
			for i < n {
				if sql[i] == '\\' {
					i += 2
					continue
				}
				if sql[i] == c {
					if i+1 < n && sql[i+1] == c {
						i += 2
						continue
					}
					break
				}
				i++
			}
			end := i
			if end > n {
				end = n
			}
			if c == '\'' {
				blank(start+1, end, ' ')
			} else if maskIdents {
				blank(start+1, end, '_')
			}

		case c == '$':
			if tag, ok := readHeredocTag(sql[i:]); ok {
				closeAt := strings.Index(sql[i+len(tag):], tag)
				if closeAt >= 0 {
					end := i + len(tag) + closeAt + len(tag)
					blank(i, end, ' ')
					out[i] = '\''
					out[end-1] = '\''
					i = end - 1
				}
			}
		}
	}
	return string(out)
}

func readHeredocTag(s string) (string, bool) {
	if len(s) < 2 || s[0] != '$' {
		return "", false
	}
	for i := 1; i < len(s); i++ {
		if s[i] == '$' {
			return s[:i+1], true
		}
		ch := s[i]
		if !(ch == '_' || ch >= '0' && ch <= '9' || ch >= 'A' && ch <= 'Z' || ch >= 'a' && ch <= 'z') {
			return "", false
		}
	}
	return "", false
}

// topLevelOnly blanks everything nested inside parentheses so clause detection only sees the outer query.
func topLevelOnly(masked string) string {
	out := []byte(masked)
	depth := 0
	for i := 0; i < len(out); i++ {
		switch out[i] {
		case '(':
			depth++
			out[i] = ' '
		case ')':
			if depth > 0 {
				depth--
			}
			out[i] = ' '
		default:
			if depth > 0 && out[i] != '\n' {
				out[i] = ' '
			}
		}
	}
	return string(out)
}

var tableTargetSkipWords = map[string]bool{
	"TABLE": true, "TEMPORARY": true, "VIEW": true, "DICTIONARY": true, "DATABASE": true,
	"CREATE": true, "IF": true, "NOT": true, "EXISTS": true,
}

// fromListEnd lists keywords that close a comma-separated FROM list.
var fromListEnd = map[string]bool{
	"WHERE": true, "PREWHERE": true, "GROUP": true, "ORDER": true, "LIMIT": true, "HAVING": true,
	"SETTINGS": true, "UNION": true, "EXCEPT": true, "INTERSECT": true, "WINDOW": true, "QUALIFY": true,
	"JOIN": true, "ON": true, "USING": true, "ARRAY": true, "SAMPLE": true, "FINAL": true, "FORMAT": true,
}

// tokenizeSQL splits SQL into words plus standalone ",", "(" and ")" tokens.
func tokenizeSQL(query string) []string {
	var tokens []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			tokens = append(tokens, cur.String())
			cur.Reset()
		}
	}
	for _, r := range query {
		switch {
		case unicode.IsSpace(r) || r == ';':
			flush()
		case r == ',' || r == '(' || r == ')':
			flush()
			tokens = append(tokens, string(r))
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return tokens
}

func checkTableAccess(query string, blockedTables map[string]bool) error {
	tokens := tokenizeSQL(query)
	if len(tokens) == 0 {
		return nil
	}

	// DESCRIBE x / DESC TABLE x / EXISTS TABLE x / SHOW CREATE TABLE x name the target directly.
	first := strings.ToUpper(tokens[0])
	if first == "DESCRIBE" || first == "DESC" || first == "EXISTS" ||
		(first == "SHOW" && len(tokens) > 1 && strings.EqualFold(tokens[1], "CREATE")) {
		for _, tok := range tokens[1:] {
			if tok == "(" || tableTargetSkipWords[strings.ToUpper(tok)] {
				continue
			}
			if err := checkTableWord(tok, blockedTables); err != nil {
				return err
			}
			break
		}
	}

	depth := 0
	captureNext := false
	// fromListDepth is the paren depth of an open FROM list (comma joins), or -1.
	fromListDepth := -1
	for _, tok := range tokens {
		switch tok {
		case "(":
			depth++
			continue
		case ")":
			depth--
			if fromListDepth > depth {
				fromListDepth = -1
			}
			continue
		case ",":
			if fromListDepth == depth {
				captureNext = true
			}
			continue
		}

		upper := strings.ToUpper(tok)
		// ClickHouse accepts a bare table name on the right side of IN: "id IN db.users".
		if upper == "FROM" || upper == "JOIN" || upper == "IN" {
			captureNext = true
			if upper == "FROM" {
				fromListDepth = depth
			}
			continue
		}
		if fromListDepth == depth && fromListEnd[upper] {
			fromListDepth = -1
		}
		if !captureNext {
			continue
		}
		if upper == "ONLY" || upper == "TABLE" {
			continue
		}
		if err := checkTableWord(tok, blockedTables); err != nil {
			return err
		}
		captureNext = false
	}
	return nil
}

func checkTableWord(word string, blockedTables map[string]bool) error {
	schema, table := splitQualifiedName(word)
	if table == "" {
		return nil
	}
	qualified := table
	if schema != "" {
		qualified = schema + "." + table
	}
	log.Printf("[ACCESS] table=%s", qualified)

	if isTableBlocked(schema, table, blockedTables) {
		return fmt.Errorf("access to table '%s' is not permitted", qualified)
	}
	return nil
}

// splitQualifiedName returns lower-cased (database, table) for blocklist comparisons.
func splitQualifiedName(name string) (string, string) {
	clean := cleanIdentifier(name)
	if clean == "" {
		return "", ""
	}
	parts := strings.Split(clean, ".")
	if len(parts) == 1 {
		return "", parts[0]
	}
	return parts[len(parts)-2], parts[len(parts)-1]
}

// splitQualifiedNameRaw returns (database, table) with original case, for catalog lookups.
// ClickHouse identifiers are case-sensitive.
func splitQualifiedNameRaw(name string) (string, string) {
	parts := strings.Split(strings.TrimSpace(name), ".")
	for i, part := range parts {
		parts[i] = strings.Trim(strings.TrimSpace(part), "\"'`[]")
	}
	if len(parts) == 1 {
		return "", parts[0]
	}
	return parts[len(parts)-2], parts[len(parts)-1]
}

func cleanIdentifier(name string) string {
	name = strings.TrimSpace(name)
	name = strings.Trim(name, "\"'`[]")
	name = strings.TrimSuffix(name, ";")
	parts := strings.Split(name, ".")
	for i, part := range parts {
		part = strings.TrimSpace(part)
		part = strings.Trim(part, "\"'`[]")
		parts[i] = strings.ToLower(part)
	}
	return strings.Join(parts, ".")
}

func isTableBlocked(schema, table string, blockedTables map[string]bool) bool {
	table = cleanIdentifier(table)
	schema = cleanIdentifier(schema)
	if table == "" {
		return false
	}
	if blockedTables[table] {
		return true
	}
	if schema != "" && blockedTables[schema+"."+table] {
		return true
	}
	return false
}

func MaskSensitiveColumns(result *QueryResult, sensitiveColumns map[string]bool) *QueryResult {
	maskedIndices := make(map[int]bool)
	safeColumns := []string{}

	for i, col := range result.Columns {
		if sensitiveColumns[strings.ToLower(col)] {
			maskedIndices[i] = true
			log.Printf("[SECURITY] masked sensitive column: %s", col)
		} else {
			safeColumns = append(safeColumns, col)
		}
	}

	if len(maskedIndices) == 0 {
		return result
	}

	safeRows := make([]map[string]interface{}, len(result.Rows))
	for i, row := range result.Rows {
		safeRow := make(map[string]interface{})
		for col, val := range row {
			if !sensitiveColumns[strings.ToLower(col)] {
				safeRow[col] = val
			}
		}
		safeRows[i] = safeRow
	}

	return &QueryResult{Columns: safeColumns, Rows: safeRows, Count: result.Count, Truncated: result.Truncated}
}

// ValidateViewParams checks the argument list passed to a parameterized view, e.g. "id=10, status='active'".
func ValidateViewParams(params string) error {
	if strings.TrimSpace(params) == "" {
		return nil
	}
	masked := maskSQL(params, true)
	if strings.Contains(masked, ";") {
		return fmt.Errorf("view parameters must not contain semicolons")
	}
	if match := paramDangerousPattern.FindString(masked); match != "" {
		return fmt.Errorf("view parameters contain forbidden SQL fragment: %s", strings.ToUpper(strings.TrimSpace(match)))
	}
	if match := externalFunctionPattern.FindStringSubmatch(masked); match != nil {
		return fmt.Errorf("view parameters contain forbidden table function: %s", match[1])
	}
	return nil
}

func AuditLog(query string, success bool, rowCount int, err error) {
	status, errMsg := "SUCCESS", ""
	if !success {
		status = "BLOCKED"
		errMsg = err.Error()
	}
	log.Printf("[AUDIT] time=%s status=%s rows=%d query=%q error=%q",
		time.Now().Format(time.RFC3339), status, rowCount, query, errMsg)
}
