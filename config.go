package main

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
)

const configFileName = ".mcp-clickhouse-config.json"

// ConnectionConfig holds per-connection ClickHouse credentials and security settings.
type ConnectionConfig struct {
	Server            string   `json:"server"`
	Port              int      `json:"port"`
	Database          string   `json:"database"`
	User              string   `json:"user"`
	Password          string   `json:"password"`
	Protocol          string   `json:"protocol"`
	Secure            bool     `json:"secure"`
	SkipVerify        bool     `json:"skip_verify"`
	DSN               string   `json:"dsn"`
	ConnectionTimeout int      `json:"connection_timeout"`
	QueryTimeout      int      `json:"query_timeout"`
	BlockedTables     []string `json:"blocked_tables"`
	SensitiveColumns  []string `json:"sensitive_columns"`
	MaxRows           int      `json:"max_rows"`
	IsDefault         bool     `json:"default"`

	// Resolved at load time (not serialized).
	BlockedTablesMap    map[string]bool `json:"-"`
	SensitiveColumnsMap map[string]bool `json:"-"`
	MaxRowsResolved     int             `json:"-"`
}

// ProtocolName returns the normalized protocol: "native" or "http".
func (c *ConnectionConfig) ProtocolName() string {
	if strings.EqualFold(strings.TrimSpace(c.Protocol), "http") {
		return "http"
	}
	return "native"
}

// ResolvedPort returns the configured port or the ClickHouse default for the protocol/TLS combination.
func (c *ConnectionConfig) ResolvedPort() int {
	if c.Port != 0 {
		return c.Port
	}
	switch {
	case c.ProtocolName() == "http" && c.Secure:
		return 8443
	case c.ProtocolName() == "http":
		return 8123
	case c.Secure:
		return 9440
	default:
		return 9000
	}
}

// QueryTimeoutDuration returns the per-query timeout enforced both client-side and via max_execution_time.
func (c *ConnectionConfig) QueryTimeoutDuration() time.Duration {
	if c.QueryTimeout <= 0 {
		return 60 * time.Second
	}
	return time.Duration(c.QueryTimeout) * time.Second
}

// HasTarget reports whether the connection has enough information to connect.
func (c *ConnectionConfig) HasTarget() bool {
	return c.Server != "" || c.DSN != ""
}

// BuildOptions creates clickhouse-go options from config fields or the DSN.
func (c *ConnectionConfig) BuildOptions() (*clickhouse.Options, error) {
	var opts *clickhouse.Options

	if c.DSN != "" {
		parsed, err := clickhouse.ParseDSN(c.DSN)
		if err != nil {
			return nil, fmt.Errorf("invalid dsn: %w", err)
		}
		opts = parsed
	} else {
		if c.Server == "" {
			return nil, nil
		}

		database := c.Database
		if database == "" {
			database = "default"
		}
		user := c.User
		if user == "" {
			user = "default"
		}

		opts = &clickhouse.Options{
			Addr: []string{net.JoinHostPort(c.Server, strconv.Itoa(c.ResolvedPort()))},
			Auth: clickhouse.Auth{
				Database: database,
				Username: user,
				Password: c.Password,
			},
		}
		if c.ProtocolName() == "http" {
			opts.Protocol = clickhouse.HTTP
		} else {
			opts.Protocol = clickhouse.Native
			opts.Compression = &clickhouse.Compression{Method: clickhouse.CompressionLZ4}
		}
		if c.Secure {
			opts.TLS = &tls.Config{InsecureSkipVerify: c.SkipVerify}
			// Over HTTPS, clickhouse-go switches to client-certificate auth when the password is
			// empty. Force normal user auth so password-less users still work.
			if opts.Protocol == clickhouse.HTTP && c.Password == "" {
				opts.HttpHeaders = map[string]string{"X-ClickHouse-SSL-Certificate-Auth": "off"}
			}
		}
	}

	timeout := c.ConnectionTimeout
	if timeout == 0 {
		timeout = 30
	}
	opts.DialTimeout = time.Duration(timeout) * time.Second
	opts.MaxOpenConns = 5
	opts.MaxIdleConns = 2
	opts.ConnMaxLifetime = 30 * time.Minute
	opts.ClientInfo = clickhouse.ClientInfo{
		Products: []struct {
			Name    string
			Version string
		}{{Name: "mcp-clickhouse", Version: version}},
	}

	return opts, nil
}

func (c *ConnectionConfig) resolveDefaults() {
	c.BlockedTablesMap = toMap(c.BlockedTables)
	c.SensitiveColumnsMap = toMap(c.SensitiveColumns)
	c.MaxRowsResolved = c.MaxRows
	if c.MaxRowsResolved == 0 {
		c.MaxRowsResolved = 100
	}

	if len(c.SensitiveColumnsMap) == 0 {
		c.SensitiveColumnsMap = map[string]bool{
			"password": true, "password_hash": true,
			"ssn": true, "credit_card": true,
			"salary": true, "token": true,
			"secret": true, "api_key": true,
		}
	}
}

type Config struct {
	Connections map[string]*ConnectionConfig `json:"connections"`

	// Single-connection format.
	Server            string   `json:"server"`
	Port              int      `json:"port"`
	Database          string   `json:"database"`
	User              string   `json:"user"`
	Password          string   `json:"password"`
	Protocol          string   `json:"protocol"`
	Secure            bool     `json:"secure"`
	SkipVerify        bool     `json:"skip_verify"`
	DSN               string   `json:"dsn"`
	ConnectionTimeout int      `json:"connection_timeout"`
	QueryTimeout      int      `json:"query_timeout"`
	BlockedTables     []string `json:"blocked_tables"`
	SensitiveColumns  []string `json:"sensitive_columns"`
	MaxRows           int      `json:"max_rows"`

	OutputFormat  string `json:"output_format"`
	ExportMaxRows int    `json:"export_max_rows"`
	ExportDir     string `json:"export_dir"`
}

var OutputFormat string
var LoadedConfig Config
var DefaultConnectionName string

func LoadConfig() {
	cfg := loadConfigFile()
	LoadedConfig = cfg

	if len(cfg.Connections) == 0 {
		conn := &ConnectionConfig{
			Server:            cfg.Server,
			Port:              cfg.Port,
			Database:          cfg.Database,
			User:              cfg.User,
			Password:          cfg.Password,
			Protocol:          cfg.Protocol,
			Secure:            cfg.Secure,
			SkipVerify:        cfg.SkipVerify,
			DSN:               cfg.DSN,
			ConnectionTimeout: cfg.ConnectionTimeout,
			QueryTimeout:      cfg.QueryTimeout,
			BlockedTables:     cfg.BlockedTables,
			SensitiveColumns:  cfg.SensitiveColumns,
			MaxRows:           cfg.MaxRows,
			IsDefault:         true,
		}
		cfg.Connections = map[string]*ConnectionConfig{"default": conn}
		LoadedConfig.Connections = cfg.Connections
	}

	DefaultConnectionName = ""
	for name, conn := range cfg.Connections {
		conn.resolveDefaults()
		if conn.IsDefault {
			DefaultConnectionName = name
		}
	}
	if DefaultConnectionName == "" && len(cfg.Connections) == 1 {
		for name := range cfg.Connections {
			DefaultConnectionName = name
		}
	}

	OutputFormat = strings.ToLower(cfg.OutputFormat)
	if OutputFormat != "toon" {
		OutputFormat = "json"
	}

	if cfg.ExportMaxRows > 0 {
		ExportMaxRows = cfg.ExportMaxRows
	}
	if cfg.ExportDir != "" {
		ExportDirOverride = cfg.ExportDir
	}

	log.Printf("[CONFIG] connections=%d default=%q output_format=%s export_max_rows=%d",
		len(cfg.Connections), DefaultConnectionName, OutputFormat, ExportMaxRows)
	for name, conn := range cfg.Connections {
		marker := ""
		if name == DefaultConnectionName {
			marker = " (default)"
		}
		log.Printf("[CONFIG]   connection %q%s: server=%s protocol=%s secure=%t database=%s blocked_tables=%d sensitive_columns=%d max_rows=%d",
			name, marker, conn.Server, conn.ProtocolName(), conn.Secure, conn.Database,
			len(conn.BlockedTablesMap), len(conn.SensitiveColumnsMap), conn.MaxRowsResolved)
	}
}

func loadConfigFile() Config {
	var cfg Config

	configPath := os.Getenv("CLICKHOUSE_CONFIG_FILE")

	// A relative CLICKHOUSE_CONFIG_FILE (e.g. set in a project's .mcp.json) is looked up from the
	// working directory upward, so it still resolves when Claude Code starts in a subfolder.
	if configPath != "" && !filepath.IsAbs(configPath) {
		if _, err := os.Stat(configPath); err != nil {
			if found := findUpward(configPath); found != "" {
				configPath = found
			}
		}
	}

	if configPath == "" {
		if exePath, err := os.Executable(); err == nil {
			candidate := filepath.Join(filepath.Dir(exePath), configFileName)
			if _, err := os.Stat(candidate); err == nil {
				configPath = candidate
			}
		}
	}

	if configPath == "" {
		configPath = findUpward(configFileName)
	}

	if configPath == "" {
		configPath = configFileName
	}

	cwd, _ := os.Getwd()
	log.Printf("[CONFIG] cwd=%s configPath=%s", cwd, configPath)

	data, err := os.ReadFile(configPath)
	if err != nil {
		log.Printf("[CONFIG] no config file at %s, falling back to env vars", configPath)
		cfg.BlockedTables = splitCSV(os.Getenv("CLICKHOUSE_BLOCKED_TABLES"))
		cfg.SensitiveColumns = splitCSV(os.Getenv("CLICKHOUSE_SENSITIVE_COLUMNS"))
		return cfg
	}

	if err := json.Unmarshal(data, &cfg); err != nil {
		log.Fatalf("[CONFIG] invalid JSON in %s: %v", configPath, err)
	}

	log.Printf("[CONFIG] loaded from %s", configPath)
	return cfg
}

// findUpward returns the first existing dir/name walking from the working directory to the
// filesystem root, or "" if none exists.
func findUpward(name string) string {
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
	for {
		candidate := filepath.Join(dir, name)
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

func toMap(items []string) map[string]bool {
	result := make(map[string]bool)
	for _, item := range items {
		key := strings.ToLower(strings.TrimSpace(item))
		if key != "" {
			result[key] = true
		}
	}
	return result
}

func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	result := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			result = append(result, t)
		}
	}
	return result
}
