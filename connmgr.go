package main

import (
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"

	"github.com/mark3labs/mcp-go/mcp"
)

// ConnectionManager manages multiple named ClickHouse connections with lazy initialization.
type ConnectionManager struct {
	mu          sync.RWMutex
	configs     map[string]*ConnectionConfig
	pools       map[string]*Database
	defaultName string
}

func NewConnectionManager(configs map[string]*ConnectionConfig, defaultName string) *ConnectionManager {
	return &ConnectionManager{
		configs:     configs,
		pools:       make(map[string]*Database),
		defaultName: defaultName,
	}
}

func (cm *ConnectionManager) GetConnection(name string) (*Database, *ConnectionConfig, error) {
	if name == "" {
		name = cm.defaultName
	}
	if name == "" {
		return nil, nil, fmt.Errorf("no default connection configured. Specify 'connection' parameter. Available: %s",
			strings.Join(cm.ListConnections(), ", "))
	}

	cfg, ok := cm.configs[name]
	if !ok {
		return nil, nil, fmt.Errorf("unknown connection '%s'. Available connections: %s",
			name, strings.Join(cm.ListConnections(), ", "))
	}

	cm.mu.RLock()
	db, exists := cm.pools[name]
	cm.mu.RUnlock()
	if exists {
		return db, cfg, nil
	}

	cm.mu.Lock()
	defer cm.mu.Unlock()

	if db, exists = cm.pools[name]; exists {
		return db, cfg, nil
	}

	opts, err := cfg.BuildOptions()
	if err != nil {
		return nil, nil, fmt.Errorf("connection '%s': %w", name, err)
	}
	if opts == nil {
		return nil, nil, fmt.Errorf("connection '%s' has no server configured", name)
	}

	log.Printf("[CONNECT] Initializing connection '%s' (addr=%s protocol=%s database=%s user=%s)",
		name, strings.Join(opts.Addr, ","), opts.Protocol, opts.Auth.Database, opts.Auth.Username)

	db, err = NewDatabase(opts, cfg.QueryTimeoutDuration())
	if err != nil {
		return nil, nil, fmt.Errorf("connection '%s' failed: %w", name, err)
	}

	cm.pools[name] = db
	log.Printf("[CONNECT] Connection '%s' established (server version %s, readonly=%d)",
		name, db.ServerVersion, db.ReadonlyLevel)
	return db, cfg, nil
}

// Connected returns the open database for a connection without connecting, or nil.
func (cm *ConnectionManager) Connected(name string) *Database {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	return cm.pools[name]
}

func (cm *ConnectionManager) ListConnections() []string {
	names := make([]string, 0, len(cm.configs))
	for name := range cm.configs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (cm *ConnectionManager) DefaultName() string {
	return cm.defaultName
}

func (cm *ConnectionManager) Configs() map[string]*ConnectionConfig {
	return cm.configs
}

func (cm *ConnectionManager) Close() {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	for name, db := range cm.pools {
		log.Printf("[CONNECT] Closing connection '%s'", name)
		db.Close()
	}
}

func resolveDB(cm *ConnectionManager, req mcp.CallToolRequest) (*Database, *ConnectionConfig, error) {
	args := req.GetArguments()
	connName, _ := args["connection"].(string)
	return cm.GetConnection(strings.TrimSpace(connName))
}
