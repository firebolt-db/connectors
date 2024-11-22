package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"slices"
	"strings"
	"time"

	cerrors "github.com/estuary/connectors/go/connector-errors"
	networkTunnel "github.com/estuary/connectors/go/network-tunnel"
	schemagen "github.com/estuary/connectors/go/schema-gen"
	boilerplate "github.com/estuary/connectors/source-boilerplate"
	"github.com/estuary/connectors/sqlcapture"
	pf "github.com/estuary/flow/go/protocols/flow"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/sirupsen/logrus"
)

type sshForwarding struct {
	SSHEndpoint string `json:"sshEndpoint" jsonschema:"title=SSH Endpoint,description=Endpoint of the remote SSH server that supports tunneling (in the form of ssh://user@hostname[:port])" jsonschema_extras:"pattern=^ssh://.+@.+$"`
	PrivateKey  string `json:"privateKey" jsonschema:"title=SSH Private Key,description=Private key to connect to the remote SSH server." jsonschema_extras:"secret=true,multiline=true"`
}

type tunnelConfig struct {
	SSHForwarding *sshForwarding `json:"sshForwarding,omitempty" jsonschema:"title=SSH Forwarding"`
}

var postgresDriver = &sqlcapture.Driver{
	ConfigSchema:     configSchema(),
	DocumentationURL: "https://go.estuary.dev/source-postgresql",
	Connect:          connectPostgres,
}

// The standard library `time.RFC3339Nano` is wrong for historical reasons, this
// format string is better because it always uses 9-digit fractional seconds, and
// thus it can be sorted lexicographically as bytes.
const sortableRFC3339Nano = "2006-01-02T15:04:05.000000000Z07:00"

func main() {
	boilerplate.RunMain(postgresDriver)
}

func connectPostgres(ctx context.Context, name string, cfg json.RawMessage) (sqlcapture.Database, error) {
	var config Config
	if err := pf.UnmarshalStrict(cfg, &config); err != nil {
		return nil, fmt.Errorf("error parsing config json: %w", err)
	}
	config.SetDefaults()

	// If SSH Endpoint is configured, then try to start a tunnel before establishing connections
	if config.NetworkTunnel != nil && config.NetworkTunnel.SSHForwarding != nil && config.NetworkTunnel.SSHForwarding.SSHEndpoint != "" {
		host, port, err := net.SplitHostPort(config.Address)
		if err != nil {
			return nil, fmt.Errorf("splitting address to host and port: %w", err)
		}

		var sshConfig = &networkTunnel.SshConfig{
			SshEndpoint: config.NetworkTunnel.SSHForwarding.SSHEndpoint,
			PrivateKey:  []byte(config.NetworkTunnel.SSHForwarding.PrivateKey),
			ForwardHost: host,
			ForwardPort: port,
			LocalPort:   "5432",
		}
		var tunnel = sshConfig.CreateTunnel()

		// FIXME/question: do we need to shut down the tunnel manually if it is a child process?
		// at the moment tunnel.Stop is not being called anywhere, but if the connector shuts down, the child process also shuts down.
		if err := tunnel.Start(); err != nil {
			return nil, err
		}
	}

	var db = &postgresDatabase{config: &config}
	if err := db.connect(ctx); err != nil {
		return nil, err
	}
	return db, nil
}

// Config tells the connector how to connect to and interact with the source database.
type Config struct {
	Address     string         `json:"address" jsonschema:"title=Server Address,description=The host or host:port at which the database can be reached." jsonschema_extras:"order=0"`
	User        string         `json:"user" jsonschema:"default=flow_capture,description=The database user to authenticate as." jsonschema_extras:"order=1"`
	Password    string         `json:"password" jsonschema:"description=Password for the specified database user." jsonschema_extras:"secret=true,order=2"`
	Database    string         `json:"database" jsonschema:"default=postgres,description=Logical database name to capture from." jsonschema_extras:"order=3"`
	HistoryMode bool           `json:"historyMode" jsonschema:"default=false,description=Capture change events without reducing them to a final state." jsonschema_extras:"order=4"`
	Advanced    advancedConfig `json:"advanced,omitempty" jsonschema:"title=Advanced Options,description=Options for advanced users. You should not typically need to modify these." jsonschema_extra:"advanced=true"`

	NetworkTunnel *tunnelConfig `json:"networkTunnel,omitempty" jsonschema:"title=Network Tunnel,description=Connect to your system through an SSH server that acts as a bastion host for your network."`
}

type advancedConfig struct {
	PublicationName       string   `json:"publicationName,omitempty" jsonschema:"default=flow_publication,description=The name of the PostgreSQL publication to replicate from."`
	SlotName              string   `json:"slotName,omitempty" jsonschema:"default=flow_slot,description=The name of the PostgreSQL replication slot to replicate from."`
	WatermarksTable       string   `json:"watermarksTable,omitempty" jsonschema:"default=public.flow_watermarks,description=The name of the table used for watermark writes during backfills. Must be fully-qualified in '<schema>.<table>' form."`
	SkipBackfills         string   `json:"skip_backfills,omitempty" jsonschema:"title=Skip Backfills,description=A comma-separated list of fully-qualified table names which should not be backfilled."`
	BackfillChunkSize     int      `json:"backfill_chunk_size,omitempty" jsonschema:"title=Backfill Chunk Size,default=50000,description=The number of rows which should be fetched from the database in a single backfill query."`
	SSLMode               string   `json:"sslmode,omitempty" jsonschema:"title=SSL Mode,description=Overrides SSL connection behavior by setting the 'sslmode' parameter.,enum=disable,enum=allow,enum=prefer,enum=require,enum=verify-ca,enum=verify-full"`
	DiscoverSchemas       []string `json:"discover_schemas,omitempty" jsonschema:"title=Discovery Schema Selection,description=If this is specified only tables in the selected schema(s) will be automatically discovered. Omit all entries to discover tables from all schemas."`
	DiscoverOnlyPublished bool     `json:"discover_only_published,omitempty" jsonschema:"title=Discover Only Published Tables,description=When set the capture will only discover tables which have already been added to the publication. This can be useful if you intend to manage which tables are captured by adding or removing them from the publication."`
	MinimumBackfillXID    string   `json:"min_backfill_xid,omitempty" jsonschema:"title=Minimum Backfill XID,description=Only backfill rows with XMIN values greater (in a 32-bit modular comparison) than the specified XID. Helpful for reducing re-backfill data volume in certain edge cases." jsonschema_extras:"pattern=^[0-9]+$"`
}

// Validate checks that the configuration possesses all required properties.
func (c *Config) Validate() error {
	var requiredProperties = [][]string{
		{"address", c.Address},
		{"user", c.User},
		{"password", c.Password},
	}
	for _, req := range requiredProperties {
		if req[1] == "" {
			return fmt.Errorf("missing '%s'", req[0])
		}
	}

	// Connection poolers like pgbouncer or the Supabase one are not supported. They will silently
	// ignore the `replication=database` option, meaning that captures can never be successful, and
	// they can introduce other weird failure modes which aren't worth trying to fix given that core
	// incompatibility.
	//
	// It might be useful to add a generic "are we connected via a pooler" heuristic if we can figure
	// one out. Until then all we can do is check for specific address patterns that match common ones
	// we see people trying to use. Even once we have a generic check this is probably still useful,
	// since it lets us name the specific thing they're using and give instructions for precisely how
	// to get the correct address instead.
	if strings.HasSuffix(c.Address, ".pooler.supabase.com:6543") || strings.HasSuffix(c.Address, ".pooler.supabase.com") {
		return cerrors.NewUserError(nil, fmt.Sprintf("address must be a direct connection: address %q is using the Supabase connection pooler, consult go.estuary.dev/supabase-direct-address for details", c.Address))
	}

	if c.Advanced.WatermarksTable != "" && !strings.Contains(c.Advanced.WatermarksTable, ".") {
		return fmt.Errorf("invalid 'watermarksTable' configuration: table name %q must be fully-qualified as \"<schema>.<table>\"", c.Advanced.WatermarksTable)
	}
	if c.Advanced.SkipBackfills != "" {
		for _, skipStreamID := range strings.Split(c.Advanced.SkipBackfills, ",") {
			if !strings.Contains(skipStreamID, ".") {
				return fmt.Errorf("invalid 'skipBackfills' configuration: table name %q must be fully-qualified as \"<schema>.<table>\"", skipStreamID)
			}
		}
	}
	if c.Advanced.SSLMode != "" {
		if !slices.Contains([]string{"disable", "allow", "prefer", "require", "verify-ca", "verify-full"}, c.Advanced.SSLMode) {
			return fmt.Errorf("invalid 'sslmode' configuration: unknown setting %q", c.Advanced.SSLMode)
		}
	}
	for _, s := range c.Advanced.DiscoverSchemas {
		if len(s) == 0 {
			return fmt.Errorf("discovery schema selection must not contain any entries that are blank: To discover tables from all schemas, remove all entries from the discovery schema selection list. To discover tables only from specific schemas, provide their names as non-blank entries")
		}
	}

	return nil
}

// SetDefaults fills in the default values for unset optional parameters.
func (c *Config) SetDefaults() {
	// Note these are 1:1 with 'omitempty' in Config field tags,
	// which cause these fields to be emitted as non-required.
	if c.Advanced.SlotName == "" {
		c.Advanced.SlotName = "flow_slot"
	}
	if c.Advanced.PublicationName == "" {
		c.Advanced.PublicationName = "flow_publication"
	}
	if c.Advanced.WatermarksTable == "" {
		c.Advanced.WatermarksTable = "public.flow_watermarks"
	}
	if c.Advanced.BackfillChunkSize <= 0 {
		c.Advanced.BackfillChunkSize = 50000
	}

	// The address config property should accept a host or host:port
	// value, and if the port is unspecified it should be the PostgreSQL
	// default 5432.
	if !strings.Contains(c.Address, ":") {
		c.Address += ":5432"
	}
}

// ToURI converts the Config to a DSN string.
func (c *Config) ToURI() string {
	var address = c.Address
	// If SSH Tunnel is configured, we are going to create a tunnel from localhost:5432
	// to address through the bastion server, so we use the tunnel's address
	if c.NetworkTunnel != nil && c.NetworkTunnel.SSHForwarding != nil && c.NetworkTunnel.SSHForwarding.SSHEndpoint != "" {
		address = "localhost:5432"
	}
	var uri = url.URL{
		Scheme: "postgres",
		Host:   address,
		User:   url.UserPassword(c.User, c.Password),
	}
	if c.Database != "" {
		uri.Path = "/" + c.Database
	}
	var params = make(url.Values)
	if c.Advanced.SSLMode != "" {
		params.Set("sslmode", c.Advanced.SSLMode)
	}
	if len(params) > 0 {
		uri.RawQuery = params.Encode()
	}
	return uri.String()
}

func configSchema() json.RawMessage {
	var schema = schemagen.GenerateSchema("PostgreSQL Connection", &Config{})
	var configSchema, err = schema.MarshalJSON()
	if err != nil {
		panic(err)
	}
	return json.RawMessage(configSchema)
}

type postgresDatabase struct {
	config          *Config
	conn            *pgx.Conn
	explained       map[sqlcapture.StreamID]struct{} // Tracks tables which have had an `EXPLAIN` run on them during this connector invocation
	includeTxIDs    map[sqlcapture.StreamID]bool     // Tracks which tables should have XID properties in their replication metadata
	tablesPublished map[sqlcapture.StreamID]bool     // Tracks which tables are part of the configured publication
}

func (db *postgresDatabase) HistoryMode() bool {
	return db.config.HistoryMode
}

func (db *postgresDatabase) connect(ctx context.Context) error {
	logrus.WithFields(logrus.Fields{
		"address":  db.config.Address,
		"user":     db.config.User,
		"database": db.config.Database,
		"slot":     db.config.Advanced.SlotName,
	}).Info("initializing connector")

	// Normal database connection used for table scanning
	var config, err = pgx.ParseConfig(db.config.ToURI())
	if err != nil {
		return fmt.Errorf("error parsing database uri: %w", err)
	}
	if config.ConnectTimeout == 0 {
		config.ConnectTimeout = 10 * time.Second
	}
	conn, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		var pgErr *pgconn.PgError

		if errors.As(err, &pgErr) {
			switch pgErr.Code {
			case "28P01":
				return cerrors.NewUserError(err, "incorrect username or password")
			case "3D000":
				return cerrors.NewUserError(err, fmt.Sprintf("database %q does not exist", db.config.Database))
			case "42501":
				return cerrors.NewUserError(err, fmt.Sprintf("user %q does not have CONNECT privilege to database %q", db.config.User, db.config.Database))
			}
		}

		return fmt.Errorf("unable to connect to database: %w", err)
	}
	if err := registerDatatypeTweaks(ctx, conn, conn.TypeMap()); err != nil {
		return err
	}
	db.conn = conn
	return nil
}

func (db *postgresDatabase) Close(ctx context.Context) error {
	if err := db.conn.Close(ctx); err != nil {
		return fmt.Errorf("error closing database connection: %w", err)
	}
	return nil
}

func (db *postgresDatabase) EmptySourceMetadata() sqlcapture.SourceMetadata {
	return &postgresSource{}
}

func (db *postgresDatabase) FallbackCollectionKey() []string {
	return []string{"/_meta/source/loc/0", "/_meta/source/loc/1", "/_meta/source/loc/2"}
}

func (db *postgresDatabase) ShouldBackfill(streamID string) bool {
	if db.config.Advanced.SkipBackfills != "" {
		// This repeated splitting is a little inefficient, but this check is done at
		// most once per table during connector startup and isn't really worth caching.
		for _, skipStreamID := range strings.Split(db.config.Advanced.SkipBackfills, ",") {
			if streamID == strings.ToLower(skipStreamID) {
				return false
			}
		}
	}
	return true
}

func (db *postgresDatabase) RequestTxIDs(schema, table string) {
	if db.includeTxIDs == nil {
		db.includeTxIDs = make(map[sqlcapture.StreamID]bool)
	}
	db.includeTxIDs[sqlcapture.JoinStreamID(schema, table)] = true
}
