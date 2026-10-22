package main

import (
	"encoding/json"
	"math"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

func TestValidateQueryAllows(t *testing.T) {
	blocked := toMap([]string{"secrets", "auth.users"})
	queries := []string{
		"SELECT 1",
		"select count() from events",
		"WITH t AS (SELECT 1 AS x) SELECT x FROM t",
		"SELECT * FROM system.tables LIMIT 5",
		"SELECT update_time, deleted FROM events",
		"SELECT 'DROP TABLE x; INSERT' AS s",
		"SELECT `delete` FROM events",
		"SELECT 1 -- DROP TABLE x",
		"SELECT 1 /* ALTER */",
		"SELECT 'it\\'s' AS s",
		"SELECT format('{} {}', 'a', 'b')",
		"SELECT formatDateTime(now(), '%Y')",
		"SELECT format FROM events",
		"SHOW TABLES",
		"SHOW DATABASES",
		"DESCRIBE TABLE events",
		"EXPLAIN SELECT 1",
		"EXISTS TABLE events",
		"SELECT * FROM events SETTINGS max_threads = 4",
		"SELECT * FROM numbers(10)",
		"SELECT * FROM events WHERE id IN (1, 2, 3)",
		"SELECT 1;",
		"SELECT * FROM auth.sessions",
		"SELECT getSetting('max_result_rows') AS max_result_rows, getSetting('readonly') AS readonly",
	}
	for _, q := range queries {
		if err := ValidateQuery(q, blocked); err != nil {
			t.Errorf("expected allowed: %q, got %v", q, err)
		}
	}
}

func TestValidateQueryBlocks(t *testing.T) {
	blocked := toMap([]string{"secrets", "auth.users"})
	queries := map[string]string{
		"INSERT INTO t VALUES (1)":                            "only SELECT",
		"ALTER TABLE t DELETE WHERE 1":                        "only SELECT",
		"SYSTEM FLUSH LOGS":                                   "only SELECT",
		"SET readonly = 0":                                    "only SELECT",
		"KILL QUERY WHERE 1":                                  "only SELECT",
		"OPTIMIZE TABLE t FINAL":                              "only SELECT",
		"SELECT 1; DROP TABLE t":                              "multiple",
		"SELECT * FROM t INTO OUTFILE 'x.csv'":                "INTO",
		"SELECT * FROM file('/etc/passwd')":                   "file",
		"SELECT * FROM url('http://x', CSV)":                  "url",
		"SELECT * FROM s3('https://bucket/x')":                "s3",
		"SELECT * FROM remote('host', db.t)":                  "remote",
		"SELECT * FROM postgresql('h:5432','db','t','u','p')": "postgresql",
		"SELECT * FROM merge('db', '^secrets$')":              "merge",
		"SELECT * FROM clusterAllReplicas(default, secrets)":  "clusterAllReplicas",
		"SELECT * FROM secrets":                               "secrets",
		"SELECT * FROM `secrets`":                             "secrets",
		"SELECT * FROM db.secrets":                            "secrets",
		"SELECT * FROM auth.users":                            "auth.users",
		"SELECT * FROM events JOIN secrets ON 1":              "secrets",
		"SELECT * FROM events AS e, secrets AS s":             "secrets",
		"SELECT * FROM numbers(10), secrets":                  "secrets",
		"SELECT * FROM events WHERE id IN secrets":            "secrets",
		"SELECT * FROM (SELECT * FROM secrets)":               "secrets",
		"DESCRIBE TABLE secrets":                              "secrets",
		"DESC secrets":                                        "secrets",
		"SHOW CREATE TABLE secrets":                           "secrets",
		"SELECT 1 SETTINGS readonly = 0":                      "readonly",
		"SELECT 1 SETTINGS max_result_rows = 1000000":         "max_result_rows",
		"SELECT * FROM events FORMAT JSON":                    "FORMAT",
		"SELECT * FROM events FORMAT CSV SETTINGS a = 1":      "FORMAT",
		"SHOW GRANTS":                                         "SHOW",
		"SHOW CREATE USER default":                            "SHOW",
		"":                                                    "empty",
	}
	for q, want := range queries {
		err := ValidateQuery(q, blocked)
		if err == nil {
			t.Errorf("expected blocked: %q", q)
			continue
		}
		if !strings.Contains(err.Error(), want) {
			t.Errorf("query %q: error %q does not mention %q", q, err.Error(), want)
		}
	}
}

func TestBlocklistBypassFunctionsAllowedWithoutBlocklist(t *testing.T) {
	if err := ValidateQuery("SELECT * FROM clusterAllReplicas(default, system.parts)", nil); err != nil {
		t.Errorf("expected clusterAllReplicas allowed without blocklist, got %v", err)
	}
}

func TestMaskSQLPreservesLength(t *testing.T) {
	inputs := []string{
		"SELECT 'a\\'b', \"x\", `y` -- c\n/* d */ # e\nFROM t",
		"SELECT $tag$DROP$tag$, 'unterminated",
		"SELECT 'ünïcode' FROM t",
	}
	for _, in := range inputs {
		for _, idents := range []bool{true, false} {
			if out := maskSQL(in, idents); len(out) != len(in) {
				t.Errorf("maskSQL(%q, %v) changed length %d -> %d", in, idents, len(in), len(out))
			}
		}
	}
	masked := maskSQL("SELECT 'DROP' -- DELETE\n", true)
	if strings.Contains(masked, "DROP") || strings.Contains(masked, "DELETE") {
		t.Errorf("literal/comment not masked: %q", masked)
	}
}

func TestAddRowLimitIfMissing(t *testing.T) {
	cases := map[string]string{
		"SELECT * FROM t":                                      "SELECT * FROM t LIMIT 100",
		"SELECT * FROM t;":                                     "SELECT * FROM t LIMIT 100",
		"SELECT * FROM t LIMIT 5":                              "SELECT * FROM t LIMIT 5",
		"SELECT * FROM t LIMIT 5, 10":                          "SELECT * FROM t LIMIT 5, 10",
		"SELECT * FROM t LIMIT 2 BY user_id":                   "SELECT * FROM t LIMIT 2 BY user_id LIMIT 100",
		"SELECT * FROM t LIMIT 2 BY u LIMIT 7":                 "SELECT * FROM t LIMIT 2 BY u LIMIT 7",
		"SELECT * FROM (SELECT * FROM t LIMIT 5)":              "SELECT * FROM (SELECT * FROM t LIMIT 5) LIMIT 100",
		"SELECT * FROM t SETTINGS max_threads = 2":             "SELECT * FROM t LIMIT 100 SETTINGS max_threads = 2",
		"SELECT * FROM t -- trailing comment":                  "SELECT * FROM t LIMIT 100",
		"SELECT 'LIMIT 5' FROM t":                              "SELECT 'LIMIT 5' FROM t LIMIT 100",
		"SELECT * FROM t OFFSET 1 ROW FETCH FIRST 3 ROWS ONLY": "SELECT * FROM t OFFSET 1 ROW FETCH FIRST 3 ROWS ONLY",
		"SELECT TOP 5 * FROM t":                                "SELECT TOP 5 * FROM t",
		"SHOW TABLES":                                          "SHOW TABLES",
		"DESCRIBE t":                                           "DESCRIBE t",
	}
	for in, want := range cases {
		if got := addRowLimitIfMissing(in, 100); got != want {
			t.Errorf("addRowLimitIfMissing(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNormalizeValueIsJSONSafe(t *testing.T) {
	s := "x"
	var nilStr *string
	u64 := uint64(math.MaxUint64)
	values := []interface{}{
		uint8(1), uint64(18446744073709551615), &u64,
		decimal.RequireFromString("12.3400"),
		big.NewInt(0).Lsh(big.NewInt(1), 100),
		uuid.MustParse("123e4567-e89b-12d3-a456-426614174000"),
		net.ParseIP("10.0.0.1"),
		&s, nilStr,
		map[uint64]string{1: "a"},
		map[string][]int32{"k": {1, 2}},
		[]interface{}{uint16(3), "z"},
		[2]float64{1.5, 2.5},
		math.NaN(), math.Inf(1),
		time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	}
	for _, v := range values {
		out := normalizeValue(v)
		if _, err := json.Marshal(out); err != nil {
			t.Errorf("normalizeValue(%T) not marshalable: %v", v, err)
		}
	}
	if got := normalizeValue(decimal.RequireFromString("12.34")); got != json.Number("12.34") {
		t.Errorf("decimal normalized to %v (%T)", got, got)
	}
	if got := formatValue(time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC), "Nullable(Date)"); got != "2026-01-02" {
		t.Errorf("Date formatted as %v", got)
	}
}

func TestToonNumericTypes(t *testing.T) {
	r := &QueryResult{
		Columns: []string{"c", "d"},
		Rows:    []map[string]interface{}{{"c": uint64(42), "d": json.Number("1.50")}},
		Count:   1,
	}
	out := r.ToTOON()
	if !strings.Contains(out, "42,1.50") {
		t.Errorf("unsigned/decimal values should render unquoted, got:\n%s", out)
	}
}

func TestParseQueryParams(t *testing.T) {
	params, err := parseQueryParams(`{"id": 42, "name": "bob", "ok": true, "ratio": 0.5, "missing": null, "ids": [1, 2]}`)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"id": "42", "name": "bob", "ok": "1", "ratio": "0.5", "missing": `\N`, "ids": "[1,2]"}
	for k, v := range want {
		if params[k] != v {
			t.Errorf("param %s = %q, want %q", k, params[k], v)
		}
	}
	if _, err := parseQueryParams("[1,2]"); err == nil {
		t.Error("expected error for non-object params")
	}
}

func TestValidateViewParams(t *testing.T) {
	if err := ValidateViewParams("day='2026-01-01', region=3, d=toDate('2026-01-01')"); err != nil {
		t.Errorf("expected valid params, got %v", err)
	}
	for _, bad := range []string{"x=1; DROP TABLE t", "x=(SELECT 1)", "x=1 FROM secrets", "x=file('/etc/passwd')"} {
		if err := ValidateViewParams(bad); err == nil {
			t.Errorf("expected blocked params: %q", bad)
		}
	}
	if balancedParens("x=1) AS t, secrets AS (") {
		t.Error("expected unbalanced parens to be rejected")
	}
}

func TestExtractViewParameters(t *testing.T) {
	ddl := "CREATE VIEW db.v AS SELECT * FROM t WHERE d = {day:Date} AND r = {region: UInt32} AND x = {day:Date}"
	params := extractViewParameters(ddl)
	if len(params) != 2 || params[0]["name"] != "day" || params[1]["type"] != "UInt32" {
		t.Errorf("unexpected params: %v", params)
	}
}

func TestFilterCreateStatement(t *testing.T) {
	ddl := "CREATE TABLE db.u\n(\n    `id` UInt64,\n    `password` String,\n    `email` String\n)\nENGINE = MergeTree"
	out := filterCreateStatement(ddl, toMap([]string{"password"}))
	if strings.Contains(out, "password") || !strings.Contains(out, "email") {
		t.Errorf("unexpected DDL filter output:\n%s", out)
	}
}
