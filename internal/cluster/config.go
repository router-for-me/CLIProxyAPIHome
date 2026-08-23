package cluster

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	DefaultConfigPath                 = "cluster.yaml"
	defaultDatabaseSlowQueryThreshold = 200 * time.Millisecond
)

type Config struct {
	PGSQL  PGSQLConfig  `yaml:"pgsql"`
	SQLite SQLiteConfig `yaml:"sqlite"`
	Node   NodeConfig   `yaml:"node"`
}

type PGSQLConfig struct {
	PostgresURI        string        `yaml:"postgres_uri"`
	Host               string        `yaml:"host"`
	Port               int           `yaml:"port"`
	User               string        `yaml:"user"`
	Password           string        `yaml:"password"`
	Passowrd           string        `yaml:"passowrd"`
	Database           string        `yaml:"database"`
	SSLMode            string        `yaml:"sslmode"`
	SlowQueryThreshold time.Duration `yaml:"slow-query-threshold"`
}

type SQLiteConfig struct {
	Path               string        `yaml:"path"`
	SlowQueryThreshold time.Duration `yaml:"slow-query-threshold"`
}

type DatabaseBackend string

const (
	DatabaseBackendPostgres DatabaseBackend = "postgres"
	DatabaseBackendSQLite   DatabaseBackend = "sqlite"
)

type NodeConfig struct {
	ExternalIP        string        `yaml:"external-ip"`
	ExternalPort      int           `yaml:"external-port"`
	Port              int           `yaml:"port"`
	HeartbeatInterval time.Duration `yaml:"heartbeat-interval"`
	HeartbeatTimeout  time.Duration `yaml:"heartbeat-timeout"`
	EventPollInterval time.Duration `yaml:"event-poll-interval"`
}

type rawConfig struct {
	PGSQL  PGSQLConfig  `yaml:"pgsql"`
	SQLite SQLiteConfig `yaml:"sqlite"`
	Node   rawNode      `yaml:"node"`
}

type rawNode struct {
	ExternalIP        string       `yaml:"external-ip"`
	ExternalPort      int          `yaml:"external-port"`
	Port              int          `yaml:"port"`
	HeartbeatInterval yamlDuration `yaml:"heartbeat-interval"`
	HeartbeatTimeout  yamlDuration `yaml:"heartbeat-timeout"`
	EventPollInterval yamlDuration `yaml:"event-poll-interval"`
}

type yamlDuration time.Duration

// UnmarshalYAML decodes a yaml.
func (d *yamlDuration) UnmarshalYAML(value *yaml.Node) error {
	if value == nil || strings.TrimSpace(value.Value) == "" {
		*d = 0
		return nil
	}

	parsedDuration, errParseDuration := time.ParseDuration(value.Value)
	if errParseDuration == nil {
		*d = yamlDuration(parsedDuration)
		return nil
	}

	var parsedInt int64
	if errDecode := value.Decode(&parsedInt); errDecode == nil {
		*d = yamlDuration(time.Duration(parsedInt))
		return nil
	}

	return fmt.Errorf("invalid duration %q", value.Value)
}

// LoadConfigOptional loads a config optional.
func LoadConfigOptional(path string) (*Config, bool, error) {
	// Normalize source data before building the derived payload.
	configPath := strings.TrimSpace(path)
	if configPath == "" {
		configPath = DefaultConfigPath
	}

	content, errRead := os.ReadFile(configPath)
	if errors.Is(errRead, os.ErrNotExist) {
		return nil, false, nil
	}
	if errRead != nil {
		return nil, true, errRead
	}

	raw := rawConfig{}
	if errUnmarshal := yaml.Unmarshal(content, &raw); errUnmarshal != nil {
		return nil, true, errUnmarshal
	}

	cfg := &Config{
		PGSQL: raw.PGSQL,
		SQLite: SQLiteConfig{
			Path:               strings.TrimSpace(raw.SQLite.Path),
			SlowQueryThreshold: raw.SQLite.SlowQueryThreshold,
		},
		Node: NodeConfig{
			ExternalIP:        strings.TrimSpace(raw.Node.ExternalIP),
			ExternalPort:      raw.Node.ExternalPort,
			Port:              raw.Node.Port,
			HeartbeatInterval: time.Duration(raw.Node.HeartbeatInterval),
			HeartbeatTimeout:  time.Duration(raw.Node.HeartbeatTimeout),
			EventPollInterval: time.Duration(raw.Node.EventPollInterval),
		},
	}
	cfg.applyDefaults()
	if errValidate := cfg.Validate(); errValidate != nil {
		return nil, true, errValidate
	}

	return cfg, true, nil
}

// applyDefaults applies a defaults.
func (c *Config) applyDefaults() {
	c.PGSQL.PostgresURI = strings.TrimSpace(c.PGSQL.PostgresURI)
	if c.PGSQL.SlowQueryThreshold == 0 {
		c.PGSQL.SlowQueryThreshold = defaultDatabaseSlowQueryThreshold
	}
	if c.PGSQL.PostgresURI == "" {
		if c.PGSQL.Port == 0 {
			c.PGSQL.Port = 5432
		}
		if c.PGSQL.SSLMode == "" {
			c.PGSQL.SSLMode = "disable"
		}
		if c.PGSQL.Password == "" && c.PGSQL.Passowrd != "" {
			c.PGSQL.Password = c.PGSQL.Passowrd
		}
	}
	if c.SQLite.SlowQueryThreshold == 0 {
		c.SQLite.SlowQueryThreshold = defaultDatabaseSlowQueryThreshold
	}
	if c.Node.HeartbeatInterval == 0 {
		c.Node.HeartbeatInterval = 5 * time.Second
	}
	if c.Node.HeartbeatTimeout == 0 {
		c.Node.HeartbeatTimeout = DefaultHeartbeatTimeout()
	}
	if c.Node.EventPollInterval == 0 {
		c.Node.EventPollInterval = 3 * time.Second
	}
}

// Validate validates validate.
func (c *Config) Validate() error {
	pgsqlConfigured := c.PGSQL.Configured()
	sqliteConfigured := c.SQLite.Configured()
	if pgsqlConfigured && sqliteConfigured {
		return fmt.Errorf("exactly one database backend must be configured")
	}
	if !pgsqlConfigured && !sqliteConfigured {
		return c.PGSQL.Validate()
	}
	if pgsqlConfigured {
		if c.PGSQL.SlowQueryThreshold < 0 {
			return fmt.Errorf("pgsql.slow-query-threshold must not be negative")
		}
		if errValidatePGSQL := c.PGSQL.Validate(); errValidatePGSQL != nil {
			return errValidatePGSQL
		}
	}
	if sqliteConfigured && c.SQLite.SlowQueryThreshold < 0 {
		return fmt.Errorf("sqlite.slow-query-threshold must not be negative")
	}
	if c.Node.Port <= 0 {
		return fmt.Errorf("node.port must be greater than 0")
	}
	if c.Node.ExternalPort < 0 {
		return fmt.Errorf("node.external-port must be greater than 0 when set")
	}
	if c.Node.HeartbeatInterval <= 0 {
		return fmt.Errorf("node.heartbeat-interval must be greater than 0")
	}
	if c.Node.HeartbeatTimeout <= 0 {
		return fmt.Errorf("node.heartbeat-timeout must be greater than 0")
	}
	if c.Node.EventPollInterval <= 0 {
		return fmt.Errorf("node.event-poll-interval must be greater than 0")
	}
	return nil
}

// DatabaseBackend returns the selected database backend.
func (c *Config) DatabaseBackend() DatabaseBackend {
	if c != nil && c.SQLite.Configured() {
		return DatabaseBackendSQLite
	}
	return DatabaseBackendPostgres
}

// Configured reports whether PostgreSQL has cluster database settings.
func (c PGSQLConfig) Configured() bool {
	return strings.TrimSpace(c.PostgresURI) != "" ||
		strings.TrimSpace(c.Host) != "" ||
		strings.TrimSpace(c.User) != "" ||
		c.Password != "" ||
		c.Passowrd != "" ||
		strings.TrimSpace(c.Database) != ""
}

// Validate validates validate.
func (c PGSQLConfig) Validate() error {
	if strings.TrimSpace(c.PostgresURI) != "" {
		if strings.TrimSpace(c.Host) != "" ||
			c.Port != 0 ||
			strings.TrimSpace(c.User) != "" ||
			c.Password != "" ||
			c.Passowrd != "" ||
			strings.TrimSpace(c.Database) != "" ||
			strings.TrimSpace(c.SSLMode) != "" {
			return fmt.Errorf("pgsql.postgres_uri cannot be combined with individual connection fields")
		}
		return nil
	}
	if strings.TrimSpace(c.Host) == "" {
		return fmt.Errorf("pgsql.host is required")
	}
	if isUnixSocketHost(c.Host) {
		return fmt.Errorf("pgsql.host must be a TCP host, not a Unix socket path")
	}
	if c.Port <= 0 {
		return fmt.Errorf("pgsql.port must be greater than 0")
	}
	if strings.TrimSpace(c.User) == "" {
		return fmt.Errorf("pgsql.user is required")
	}
	if strings.TrimSpace(c.Database) == "" {
		return fmt.Errorf("pgsql.database is required")
	}
	return nil
}

// Configured reports whether SQLite has cluster database settings.
func (c SQLiteConfig) Configured() bool {
	return strings.TrimSpace(c.Path) != ""
}

// isUnixSocketHost reports whether unix socket host.
func isUnixSocketHost(host string) bool {
	trimmedHost := strings.TrimSpace(host)
	return strings.Contains(trimmedHost, "/") || strings.HasPrefix(trimmedHost, ".")
}
