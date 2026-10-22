package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ToTOON converts a QueryResult to TOON (Token-Oriented Object Notation) format.
func (r *QueryResult) ToTOON() string {
	if r == nil || len(r.Columns) == 0 {
		return "count: 0\ncolumns[0]:"
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("count: %d", r.Count))
	if r.Truncated {
		sb.WriteString("\ntruncated: true")
	}
	sb.WriteString(fmt.Sprintf("\ncolumns[%d]: %s", len(r.Columns), strings.Join(r.Columns, ",")))

	if len(r.Rows) == 0 {
		sb.WriteString(fmt.Sprintf("\nrows[0]{%s}:", strings.Join(r.Columns, ",")))
	} else {
		sb.WriteString(fmt.Sprintf("\nrows[%d]{%s}:", len(r.Rows), strings.Join(r.Columns, ",")))
		for _, row := range r.Rows {
			sb.WriteString("\n  ")
			for i, col := range r.Columns {
				if i > 0 {
					sb.WriteString(",")
				}
				sb.WriteString(toonValue(row[col]))
			}
		}
	}

	return sb.String()
}

func toonValue(v interface{}) string {
	if v == nil {
		return "null"
	}

	switch val := v.(type) {
	case bool:
		if val {
			return "true"
		}
		return "false"
	case int:
		return fmt.Sprintf("%d", val)
	case int8:
		return fmt.Sprintf("%d", val)
	case int16:
		return fmt.Sprintf("%d", val)
	case int32:
		return fmt.Sprintf("%d", val)
	case int64:
		return fmt.Sprintf("%d", val)
	case uint:
		return fmt.Sprintf("%d", val)
	case uint8:
		return fmt.Sprintf("%d", val)
	case uint16:
		return fmt.Sprintf("%d", val)
	case uint32:
		return fmt.Sprintf("%d", val)
	case uint64:
		return fmt.Sprintf("%d", val)
	case json.Number:
		return val.String()
	case float32:
		return formatFloat(float64(val))
	case float64:
		return formatFloat(val)
	case string:
		return toonString(val)
	case []interface{}, map[string]interface{}:
		// Arrays, Maps and Tuples are rendered inline as JSON.
		data, err := json.Marshal(val)
		if err != nil {
			return toonString(fmt.Sprintf("%v", val))
		}
		return toonString(string(data))
	default:
		return toonString(fmt.Sprintf("%v", val))
	}
}

func formatFloat(f float64) string {
	if f == float64(int64(f)) {
		return fmt.Sprintf("%d", int64(f))
	}
	s := fmt.Sprintf("%g", f)
	if strings.ContainsAny(s, "eE") {
		s = strings.TrimRight(fmt.Sprintf("%f", f), "0")
		s = strings.TrimRight(s, ".")
	}
	return s
}

func toonString(s string) string {
	if s == "" {
		return `""`
	}

	if s == "true" || s == "false" || s == "null" {
		return `"` + s + `"`
	}

	needsQuote := false
	for _, r := range s {
		switch r {
		case ',', ':', '"', '\\', '[', ']', '{', '}', '\n', '\r', '\t':
			needsQuote = true
		}
		if needsQuote {
			break
		}
	}

	if !needsQuote && (s[0] == ' ' || s[len(s)-1] == ' ') {
		needsQuote = true
	}
	if !needsQuote && looksNumeric(s) {
		needsQuote = true
	}

	if !needsQuote {
		return s
	}

	escaped := strings.ReplaceAll(s, `\`, `\\`)
	escaped = strings.ReplaceAll(escaped, `"`, `\"`)
	escaped = strings.ReplaceAll(escaped, "\n", `\n`)
	escaped = strings.ReplaceAll(escaped, "\r", `\r`)
	escaped = strings.ReplaceAll(escaped, "\t", `\t`)

	return `"` + escaped + `"`
}

func looksNumeric(s string) bool {
	if len(s) == 0 {
		return false
	}
	start := 0
	if s[0] == '-' {
		start = 1
		if len(s) == 1 {
			return false
		}
	}
	if start < len(s) && s[start] == '0' && start+1 < len(s) && s[start+1] >= '0' && s[start+1] <= '9' {
		return true
	}
	hasDigit := false
	hasDot := false
	for i := start; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
			hasDigit = true
		case c == '.' && !hasDot:
			hasDot = true
		case c == 'e' || c == 'E':
			return hasDigit
		default:
			return false
		}
	}
	return hasDigit
}

func serializeAny(v interface{}) (string, error) {
	if OutputFormat == "toon" {
		data, err := json.Marshal(v)
		if err != nil {
			return "", err
		}
		var normalized interface{}
		if err := json.Unmarshal(data, &normalized); err != nil {
			return "", err
		}
		return toTOONAny(normalized, 0), nil
	}
	jsonBytes, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "", err
	}
	return string(jsonBytes), nil
}

func toTOONAny(v interface{}, indent int) string {
	if v == nil {
		return "null"
	}
	prefix := strings.Repeat("  ", indent)

	switch val := v.(type) {
	case map[string]interface{}:
		if len(val) == 0 {
			return "{}"
		}
		var sb strings.Builder
		keys := make([]string, 0, len(val))
		for k := range val {
			keys = append(keys, k)
		}
		sortStrings(keys)
		for _, k := range keys {
			child := val[k]
			if arr, ok := child.([]interface{}); ok && len(arr) > 0 {
				if cols := uniformObjectKeys(arr); cols != nil {
					sb.WriteString(fmt.Sprintf("%s%s[%d]{%s}:\n", prefix, k, len(arr), strings.Join(cols, ",")))
					for _, item := range arr {
						row := item.(map[string]interface{})
						sb.WriteString(prefix + "  ")
						for ci, c := range cols {
							if ci > 0 {
								sb.WriteString(",")
							}
							sb.WriteString(toonValue(row[c]))
						}
						sb.WriteString("\n")
					}
					continue
				}
			}
			sb.WriteString(fmt.Sprintf("%s%s: %s\n", prefix, k, toTOONAny(child, indent+1)))
		}
		return sb.String()

	case []interface{}:
		if len(val) == 0 {
			return "[]"
		}
		if cols := uniformObjectKeys(val); cols != nil {
			var sb strings.Builder
			sb.WriteString(fmt.Sprintf("[%d]{%s}:\n", len(val), strings.Join(cols, ",")))
			for _, item := range val {
				row := item.(map[string]interface{})
				sb.WriteString(prefix + "  ")
				for ci, c := range cols {
					if ci > 0 {
						sb.WriteString(",")
					}
					sb.WriteString(toonValue(row[c]))
				}
				sb.WriteString("\n")
			}
			return sb.String()
		}
		var sb strings.Builder
		for _, item := range val {
			sb.WriteString(fmt.Sprintf("%s- %s\n", prefix, toTOONAny(item, indent+1)))
		}
		return sb.String()

	default:
		return toonValue(v)
	}
}

func uniformObjectKeys(arr []interface{}) []string {
	if len(arr) == 0 {
		return nil
	}
	first, ok := arr[0].(map[string]interface{})
	if !ok {
		return nil
	}
	keys := make([]string, 0, len(first))
	for k := range first {
		keys = append(keys, k)
	}
	sortStrings(keys)
	for _, item := range arr[1:] {
		m, ok := item.(map[string]interface{})
		if !ok || len(m) != len(keys) {
			return nil
		}
		for _, k := range keys {
			if _, exists := m[k]; !exists {
				return nil
			}
		}
	}
	return keys
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
