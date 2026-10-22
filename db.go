package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"net"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

type Database struct {
	pool          *sql.DB
	queryTimeout  time.Duration
	ReadonlyLevel int
	ServerVersion string
}

func NewDatabase(opts *clickhouse.Options, queryTimeout time.Duration) (*Database, error) {
	pool := clickhouse.OpenDB(opts)

	ctx, cancel := timeoutContext(context.Background(), 15*time.Second)
	defer cancel()

	if err := pool.PingContext(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("cannot reach ClickHouse: %w", err)
	}

	db := &Database{pool: pool, queryTimeout: queryTimeout}

	// A user whose profile already has readonly=1 rejects every per-query setting,
	// so detect the level once and only send settings the server will accept.
	var readonly, serverVersion string
	if err := pool.QueryRowContext(ctx, "SELECT toString(getSetting('readonly')), version()").Scan(&readonly, &serverVersion); err != nil {
		pool.Close()
		return nil, fmt.Errorf("failed to read server settings: %w", err)
	}
	db.ReadonlyLevel, _ = strconv.Atoi(readonly)
	db.ServerVersion = serverVersion

	return db, nil
}

// QueryResult is the tabular result returned to MCP clients.
type QueryResult struct {
	Columns   []string                 `json:"columns"`
	Rows      []map[string]interface{} `json:"rows"`
	Count     int                      `json:"count"`
	Truncated bool                     `json:"truncated,omitempty"`
}

// QueryOptions controls server-side settings and client-side limits for a single query.
type QueryOptions struct {
	// MaxRows caps the rows read from the result set. 0 means no cap.
	MaxRows int
	// Params binds {name:Type} placeholders server-side.
	Params map[string]string
	// Progress receives incremental progress packets (native protocol only).
	Progress func(*clickhouse.Progress)
}

func (db *Database) Close() error {
	return db.pool.Close()
}

// queryContext attaches ClickHouse settings, parameters and the query timeout to ctx.
func (db *Database) queryContext(ctx context.Context, opts QueryOptions) (context.Context, context.CancelFunc) {
	var chOpts []clickhouse.QueryOption

	if db.ReadonlyLevel != 1 {
		settings := clickhouse.Settings{
			"max_execution_time": int(db.queryTimeout.Seconds()),
		}
		if db.ReadonlyLevel == 0 {
			settings["readonly"] = 2
		}
		if opts.MaxRows > 0 {
			settings["max_result_rows"] = opts.MaxRows
			settings["result_overflow_mode"] = "break"
		}
		chOpts = append(chOpts, clickhouse.WithSettings(settings))
	}
	if len(opts.Params) > 0 {
		chOpts = append(chOpts, clickhouse.WithParameters(clickhouse.Parameters(opts.Params)))
	}
	if opts.Progress != nil {
		chOpts = append(chOpts, clickhouse.WithProgress(opts.Progress))
	}

	ctx, cancel := timeoutContext(ctx, db.queryTimeout+10*time.Second)
	return clickhouse.Context(ctx, chOpts...), cancel
}

// timeoutContext cancels after d without giving the context a deadline. clickhouse-go turns any
// deadline into a max_execution_time setting, which servers in readonly=1 mode reject.
func timeoutContext(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.WithoutCancel(parent))
	stopParent := context.AfterFunc(parent, cancel)
	timer := time.AfterFunc(d, cancel)
	return ctx, func() {
		timer.Stop()
		stopParent()
		cancel()
	}
}

func (db *Database) ExecuteQuery(ctx context.Context, query string, opts QueryOptions) (*QueryResult, error) {
	ctx, cancel := db.queryContext(ctx, opts)
	defer cancel()

	rows, err := db.pool.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("query failed: %w", err)
	}

	result, err := scanRows(rows, opts.MaxRows)
	if result != nil && result.Truncated {
		// Abort the remaining stream instead of draining it.
		cancel()
	}
	rows.Close()
	return result, err
}

// CountRows executes a query and counts rows without keeping them in memory.
func (db *Database) CountRows(ctx context.Context, query string, opts QueryOptions) (int, []string, error) {
	ctx, cancel := db.queryContext(ctx, opts)
	defer cancel()

	rows, err := db.pool.QueryContext(ctx, query)
	if err != nil {
		return 0, nil, fmt.Errorf("query failed: %w", err)
	}
	defer rows.Close()

	columns, err := rows.Columns()
	if err != nil {
		return 0, nil, fmt.Errorf("failed to get columns: %w", err)
	}

	values := make([]interface{}, len(columns))
	valuePtrs := make([]interface{}, len(columns))
	for i := range values {
		valuePtrs[i] = &values[i]
	}

	count := 0
	for rows.Next() {
		if err := rows.Scan(valuePtrs...); err != nil {
			return 0, nil, fmt.Errorf("failed to scan row: %w", err)
		}
		count++
	}
	if err := rows.Err(); err != nil {
		return 0, nil, fmt.Errorf("row iteration failed: %w", err)
	}
	return count, columns, nil
}

func scanRows(rows *sql.Rows, maxRows int) (*QueryResult, error) {
	columns, err := rows.Columns()
	if err != nil {
		return nil, fmt.Errorf("failed to get columns: %w", err)
	}

	dbTypes := make([]string, len(columns))
	if colTypes, err := rows.ColumnTypes(); err == nil {
		for i, ct := range colTypes {
			dbTypes[i] = ct.DatabaseTypeName()
		}
	}

	result := QueryResult{Columns: columns, Rows: []map[string]interface{}{}}

	for rows.Next() {
		if maxRows > 0 && len(result.Rows) >= maxRows {
			result.Truncated = true
			break
		}
		values := make([]interface{}, len(columns))
		valuePtrs := make([]interface{}, len(columns))
		for i := range values {
			valuePtrs[i] = &values[i]
		}
		if err := rows.Scan(valuePtrs...); err != nil {
			return nil, fmt.Errorf("failed to scan row: %w", err)
		}
		row := make(map[string]interface{}, len(columns))
		for i, col := range columns {
			row[col] = formatValue(values[i], dbTypes[i])
		}
		result.Rows = append(result.Rows, row)
	}

	if !result.Truncated {
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("row iteration failed: %w", err)
		}
	}

	result.Count = len(result.Rows)
	return &result, nil
}

// formatValue converts a driver value into a JSON/TOON-friendly value.
// Date columns are rendered without a time component.
func formatValue(v interface{}, dbType string) interface{} {
	if isDateType(dbType) {
		if t, ok := derefTime(v); ok {
			return t.Format("2006-01-02")
		}
	}
	// The std driver hands Decimals over as strings; keep them numeric in the output.
	if strings.HasPrefix(unwrapType(dbType), "Decimal") {
		if s, ok := v.(string); ok {
			return json.Number(s)
		}
	}
	return normalizeValue(v)
}

func isDateType(dbType string) bool {
	base := unwrapType(dbType)
	return base == "Date" || base == "Date32"
}

// unwrapType strips Nullable(...) and LowCardinality(...) wrappers from a ClickHouse type name.
func unwrapType(t string) string {
	for {
		switch {
		case strings.HasPrefix(t, "Nullable(") && strings.HasSuffix(t, ")"):
			t = t[len("Nullable(") : len(t)-1]
		case strings.HasPrefix(t, "LowCardinality(") && strings.HasSuffix(t, ")"):
			t = t[len("LowCardinality(") : len(t)-1]
		default:
			return t
		}
	}
}

func derefTime(v interface{}) (time.Time, bool) {
	switch t := v.(type) {
	case time.Time:
		return t, true
	case *time.Time:
		if t != nil {
			return *t, true
		}
	}
	return time.Time{}, false
}

// normalizeValue recursively converts ClickHouse driver types (unsigned ints, decimals,
// big ints, UUIDs, IPs, Nullable pointers, Maps with non-string keys, Tuples, Arrays)
// into values encoding/json can always marshal.
func normalizeValue(v interface{}) interface{} {
	switch val := v.(type) {
	case nil:
		return nil
	case string, bool, json.Number,
		int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64:
		return val
	case float32:
		return normalizeFloat(float64(val))
	case float64:
		return normalizeFloat(val)
	case []byte:
		return string(val)
	case time.Time:
		return val.Format(time.RFC3339Nano)
	case decimal.Decimal:
		return json.Number(val.String())
	case *decimal.Decimal:
		if val == nil {
			return nil
		}
		return json.Number(val.String())
	case big.Int:
		return json.Number(val.String())
	case *big.Int:
		if val == nil {
			return nil
		}
		return json.Number(val.String())
	case uuid.UUID:
		return val.String()
	case net.IP:
		return val.String()
	}

	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Ptr, reflect.Interface:
		if rv.IsNil() {
			return nil
		}
		return normalizeValue(rv.Elem().Interface())
	case reflect.Slice, reflect.Array:
		if rv.Kind() == reflect.Slice && rv.IsNil() {
			return []interface{}{}
		}
		if rv.Type().Elem().Kind() == reflect.Uint8 && rv.Kind() == reflect.Slice {
			return string(rv.Bytes())
		}
		out := make([]interface{}, rv.Len())
		for i := 0; i < rv.Len(); i++ {
			out[i] = normalizeValue(rv.Index(i).Interface())
		}
		return out
	case reflect.Map:
		out := make(map[string]interface{}, rv.Len())
		iter := rv.MapRange()
		for iter.Next() {
			out[fmt.Sprint(normalizeValue(iter.Key().Interface()))] = normalizeValue(iter.Value().Interface())
		}
		return out
	case reflect.String:
		return rv.String()
	case reflect.Bool:
		return rv.Bool()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return rv.Int()
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return rv.Uint()
	case reflect.Float32, reflect.Float64:
		return normalizeFloat(rv.Float())
	case reflect.Struct:
		if s, ok := v.(fmt.Stringer); ok {
			return s.String()
		}
		if data, err := json.Marshal(v); err == nil {
			var decoded interface{}
			if err := json.Unmarshal(data, &decoded); err == nil {
				return decoded
			}
		}
	}

	if s, ok := v.(fmt.Stringer); ok {
		return s.String()
	}
	return fmt.Sprint(v)
}

// normalizeFloat keeps NaN/Inf (valid in ClickHouse, invalid in JSON) as strings.
func normalizeFloat(f float64) interface{} {
	switch {
	case math.IsNaN(f):
		return "nan"
	case math.IsInf(f, 1):
		return "inf"
	case math.IsInf(f, -1):
		return "-inf"
	}
	return f
}
