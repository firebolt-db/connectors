package main

import (
	"context"
	stdsql "database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	m "github.com/estuary/connectors/go/protocols/materialize"
	boilerplate "github.com/estuary/connectors/materialize-boilerplate"
	sql "github.com/estuary/connectors/materialize-sql"
	pf "github.com/estuary/flow/go/protocols/flow"
	pm "github.com/estuary/flow/go/protocols/materialize"
	"github.com/jmoiron/sqlx"
	log "github.com/sirupsen/logrus"
	sf "github.com/snowflakedb/gosnowflake"
	"go.gazette.dev/core/consumer/protocol"
	"golang.org/x/sync/errgroup"
)

type tableConfig struct {
	Table  string `json:"table" jsonschema_extras:"x-collection-name=true"`
	Schema string `json:"schema,omitempty" jsonschema:"title=Alternative Schema,description=Alternative schema for this table (optional)" jsonschema_extras:"x-schema-name=true"`
	Delta  bool   `json:"delta_updates,omitempty" jsonschema:"title=Delta Updates,description=Use Private Key authentication to enable Snowpipe for Delta Update bindings" jsonschema_extras:"x-delta-updates=true"`

	// If the endpoint schema is the same as the resource schema, the resource path will be only the
	// table name. This is to provide compatibility for materializations that were created prior to
	// the resource-level schema setting existing, which always had a resource path of only the
	// table name.
	endpointSchema string
}

func newTableConfig(ep *sql.Endpoint) sql.Resource {
	return &tableConfig{
		// Default to the explicit endpoint configuration schema. This may be over-written by a
		// present `schema` property within `raw` for the resource.
		Schema:         ep.Config.(*config).Schema,
		endpointSchema: ep.Config.(*config).Schema,
	}
}

func (c tableConfig) Validate() error {
	if c.Table == "" {
		return fmt.Errorf("expected table")
	}
	return nil
}

func schemasEqual(s1 string, s2 string) bool {
	// Both are unquoted: Do a case insensitive match, since Snowflake uppercases everything that
	// isn't quoted. For example, "Public" is the same as "public", which is the same as "PUBLIC"
	// etc.
	if isSimpleIdentifier(s1) && isSimpleIdentifier(s2) {
		return strings.EqualFold(s1, s2)
	}

	return s1 == s2
}

func (c tableConfig) Path() sql.TablePath {
	// This is here for backward compatibility purposes. There was a time when binding resources could not
	// have schema configuration. If we change this for all bindings to be a two-part resource path, that will
	// lead to a re-backfilling of the bindings which did not previously have a schema as part of their resource path
	if c.Schema == "" || schemasEqual(c.Schema, c.endpointSchema) {
		return []string{c.Table}
	}
	return []string{c.Schema, c.Table}
}

func (c tableConfig) DeltaUpdates() bool {
	return c.Delta
}

// newSnowflakeDriver creates a new Driver for Snowflake.
func newSnowflakeDriver() *sql.Driver {
	return &sql.Driver{
		DocumentationURL: "https://go.estuary.dev/materialize-snowflake",
		EndpointSpecType: new(config),
		ResourceSpecType: new(tableConfig),
		StartTunnel:      func(ctx context.Context, conf any) error { return nil },
		NewEndpoint: func(ctx context.Context, raw json.RawMessage, tenant string) (*sql.Endpoint, error) {
			var parsed = new(config)
			if err := pf.UnmarshalStrict(raw, parsed); err != nil {
				return nil, fmt.Errorf("parsing Snowflake configuration: %w", err)
			}

			log.WithFields(log.Fields{
				"host":     parsed.Host,
				"database": parsed.Database,
				"schema":   parsed.Schema,
				"tenant":   tenant,
			}).Info("opening Snowflake")

			dsn, err := parsed.toURI(tenant)
			if err != nil {
				return nil, fmt.Errorf("building snowflake dsn: %w", err)
			}

			db, err := stdsql.Open("snowflake", dsn)
			if err != nil {
				return nil, fmt.Errorf("newSnowflakeDriver stdsql.Open: %w", err)
			}
			defer db.Close()

			timestampTypeMapping, err := getTimestampTypeMapping(ctx, db)
			if err != nil {
				return nil, fmt.Errorf("querying TIMESTAMP_TYPE_MAPPING: %w", err)
			}

			var dialect = snowflakeDialect(parsed.Schema, timestampTypeMapping)
			var templates = renderTemplates(dialect)

			return &sql.Endpoint{
				Config:  parsed,
				Dialect: dialect,
				// Snowflake does not use the checkpoint table, instead we use the recovery log
				// as the authoritative checkpoint and idempotent apply pattern
				MetaCheckpoints:     nil,
				NewClient:           newClient,
				CreateTableTemplate: templates.createTargetTable,
				NewResource:         newTableConfig,
				NewTransactor:       newTransactor,
				Tenant:              tenant,
				ConcurrentApply:     true,
			}, nil
		},
		PreReqs: preReqs,
	}
}

func getTimestampTypeMapping(ctx context.Context, db *stdsql.DB) (timestampTypeMapping, error) {
	xdb := sqlx.NewDb(db, "snowflake").Unsafe()

	type paramRow struct {
		Value string `db:"value"`
		Level string `db:"level"`
	}

	got := paramRow{}
	if err := xdb.GetContext(ctx, &got, "SHOW PARAMETERS LIKE 'TIMESTAMP_TYPE_MAPPING';"); err != nil {
		return "", err
	}

	m := timestampTypeMapping(got.Value)
	if !m.valid() {
		return "", fmt.Errorf("invalid timestamp type mapping: %s", got.Value)
	}

	log.WithFields(log.Fields{"value": got.Value, "level": got.Level}).Debug("queried TIMESTAMP_TYPE_MAPPING")

	if m == timestampNTZ && got.Level == "" {
		// Default to LTZ if the TIMESTAMP_TYPE_MAPPING parameter is using the
		// default and has not been explicitly set to use TIMESTAMP_NTZ.
		m = timestampLTZ
	}

	return m, nil
}

type transactor struct {
	cfg        *config
	ep         *sql.Endpoint
	db         *stdsql.DB
	pipeClient *PipeClient

	// Variables exclusively used by Load.
	load struct {
		conn *stdsql.Conn
	}
	// Variables exclusively used by Store.
	store struct {
		conn  *stdsql.Conn
		fence *sql.Fence
	}
	templates templates
	bindings  []*binding
	be        *boilerplate.BindingEvents
	cp        checkpoint

	// this shard's range spec and version, used to key pipes so they don't collide
	_range  *pf.RangeSpec
	version string
}

func (d *transactor) UnmarshalState(state json.RawMessage) error {
	if err := json.Unmarshal(state, &d.cp); err != nil {
		return err
	}

	return nil
}

func newTransactor(
	ctx context.Context,
	ep *sql.Endpoint,
	fence sql.Fence,
	bindings []sql.Table,
	open pm.Request_Open,
	is *boilerplate.InfoSchema,
	be *boilerplate.BindingEvents,
) (_ m.Transactor, _ *boilerplate.MaterializeOptions, err error) {
	var cfg = ep.Config.(*config)

	dsn, err := cfg.toURI(ep.Tenant)
	if err != nil {
		return nil, nil, fmt.Errorf("building snowflake dsn: %w", err)
	}

	db, err := stdsql.Open("snowflake", dsn)
	if err != nil {
		return nil, nil, fmt.Errorf("newTransactor stdsql.Open: %w", err)
	}

	var pipeClient *PipeClient
	if cfg.Credentials.AuthType == JWT {
		var accountName string
		if err := db.QueryRowContext(ctx, "SELECT CURRENT_ACCOUNT()").Scan(&accountName); err != nil {
			return nil, nil, fmt.Errorf("fetching current account name: %w", err)
		}
		pipeClient, err = NewPipeClient(cfg, accountName, ep.Tenant)
		if err != nil {
			return nil, nil, fmt.Errorf("NewPipeClient: %w", err)
		}
	}

	var d = &transactor{
		cfg:        cfg,
		ep:         ep,
		templates:  renderTemplates(ep.Dialect),
		db:         db,
		pipeClient: pipeClient,
		_range:     open.Range,
		version:    open.Version,
		be:         be,
	}

	d.store.fence = &fence

	// Establish connections.
	if db, err := stdsql.Open("snowflake", dsn); err != nil {
		return nil, nil, fmt.Errorf("load stdsql.Open: %w", err)
	} else if d.load.conn, err = db.Conn(ctx); err != nil {
		return nil, nil, fmt.Errorf("load db.Conn: %w", err)
	}

	if db, err := stdsql.Open("snowflake", dsn); err != nil {
		return nil, nil, fmt.Errorf("store stdsql.Open: %w", err)
	} else if d.store.conn, err = db.Conn(ctx); err != nil {
		return nil, nil, fmt.Errorf("store db.Conn: %w", err)
	}

	// Create stage for file-based transfers.
	if _, err = d.load.conn.ExecContext(ctx, createStageSQL); err != nil {
		return nil, nil, fmt.Errorf("creating transfer stage : %w", err)
	}

	for _, binding := range bindings {
		if err = d.addBinding(binding); err != nil {
			return nil, nil, fmt.Errorf("%v: %w", binding, err)
		}
	}

	opts := &boilerplate.MaterializeOptions{
		ExtendedLogging: true,
		AckSchedule: &boilerplate.AckScheduleOption{
			Config: cfg.Schedule,
			Jitter: []byte(cfg.Host + cfg.Warehouse),
		},
		DBTJobTrigger: &cfg.DBTJobTrigger,
	}

	return d, opts, nil
}

type binding struct {
	target sql.Table

	pipeName string
	// Variables exclusively used by Load.
	load struct {
		loadQuery string
		stage     *stagedFile
	}
	// Variables accessed by Prepare, Store, and Commit.
	store struct {
		stage       *stagedFile
		mergeInto   string
		copyInto    string
		mustMerge   bool
		mergeBounds *sql.MergeBoundsBuilder
	}
}

func (d *transactor) addBinding(target sql.Table) error {
	var b = new(binding)
	b.target = target

	b.load.stage = newStagedFile(os.TempDir())
	b.store.stage = newStagedFile(os.TempDir())
	b.store.mergeBounds = sql.NewMergeBoundsBuilder(target.Keys, d.ep.Dialect.Literal)

	if b.target.DeltaUpdates && d.cfg.Credentials.AuthType == JWT {
		var pipeName string
		var err error
		if pipeName, err = renderTableShardVersionTemplate(b.target, d._range.KeyBegin, d.version, d.templates.pipeName); err != nil {
			return fmt.Errorf("pipeName template: %w", err)
		} else {
			pipeName = strings.ToUpper(strings.Trim(pipeName, "`"))
			b.pipeName = fmt.Sprintf("%s.%s.%s", d.cfg.Database, d.cfg.Schema, pipeName)
		}
	}

	d.bindings = append(d.bindings, b)
	return nil
}

const MaxConcurrentQueries = 5

func (d *transactor) Load(it *m.LoadIterator, loaded func(int, json.RawMessage) error) (returnErr error) {
	var ctx = it.Context()

	for it.Next() {
		var b = d.bindings[it.Binding]

		if err := b.load.stage.start(ctx, d.db); err != nil {
			return err
		} else if converted, err := b.target.ConvertKey(it.Key); err != nil {
			return fmt.Errorf("converting Load key: %w", err)
		} else if err = b.load.stage.encodeRow(converted); err != nil {
			return fmt.Errorf("encoding Load key to scratch file: %w", err)
		}
	}
	if it.Err() != nil {
		return it.Err()
	}

	var subqueries = make(map[int]string)
	var filesToCleanup []string
	for i, b := range d.bindings {
		if !b.load.stage.started {
			// Pass.
		} else if dir, err := b.load.stage.flush(); err != nil {
			return fmt.Errorf("load.stage(): %w", err)
		} else if subqueries[i], err = renderTableAndFileTemplate(b.target, dir, d.templates.loadQuery); err != nil {
			return fmt.Errorf("loadQuery template: %w", err)
		} else {
			filesToCleanup = append(filesToCleanup, dir)
		}
	}
	defer func() {
		// If there was an error during processing, report that as the final
		// error and log any error from deleting the temporary files.
		if deleteErr := d.deleteFiles(ctx, filesToCleanup); deleteErr != nil && returnErr == nil {
			returnErr = deleteErr
		} else if deleteErr != nil {
			log.WithError(deleteErr).Error("failed to delete temporary files")
		}
	}()

	if len(subqueries) == 0 {
		return nil // Nothing to load.
	}

	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(MaxConcurrentQueries)
	// Used to ensure we have no data-interleaving when calling |loaded|
	var mutex sync.Mutex

	// In order for the concurrent requests below to be actually run concurrently we need
	// a separate connection for each
	for _, queryLoop := range subqueries {
		var query = queryLoop
		group.Go(func() error {
			// Issue a join of the target table and (now staged) load keys,
			// and send results to the |loaded| callback.
			rows, err := d.db.QueryContext(sf.WithStreamDownloader(groupCtx), query)
			if err != nil {
				return fmt.Errorf("querying Load documents: %w", err)
			}
			defer rows.Close()

			var binding int
			var document stdsql.RawBytes

			mutex.Lock()
			defer mutex.Unlock()
			log.WithFields(log.Fields{
				"query": query,
			}).Debug("sending Loaded documents from query")
			for rows.Next() {
				if err = rows.Scan(&binding, &document); err != nil {
					return fmt.Errorf("scanning Load document: %w", err)
				} else if err = loaded(binding, json.RawMessage(document)); err != nil {
					return fmt.Errorf("sending loaded document for table %q: %w", d.bindings[binding].target.Identifier, err)
				}
			}

			if err = rows.Err(); err != nil {
				return fmt.Errorf("querying Loads: %w", err)
			}

			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return err
	}

	return returnErr
}

func (d *transactor) pipeExists(ctx context.Context, pipeName string) (bool, error) {
	// Check to see if a pipe for this version already exists
	var query = fmt.Sprintf("SHOW PIPES LIKE '%s';", pipeName)
	rows, err := d.db.QueryContext(ctx, query)
	if err != nil {
		return false, fmt.Errorf("finding pipe %q: %w", pipeName, err)
	}
	return rows.Next(), nil
}

type checkpointItem struct {
	Table     string
	Query     string
	StagedDir string
	PipeName  string
	PipeFiles []fileRecord
	Version   string
}

type checkpoint = map[string]*checkpointItem

func (d *transactor) Store(it *m.StoreIterator) (m.StartCommitFunc, error) {
	var ctx = it.Context()

	for it.Next() {
		var b = d.bindings[it.Binding]

		if it.Exists {
			b.store.mustMerge = true
		}

		var flowDocument = it.RawJSON
		if d.cfg.HardDelete && it.Delete {
			if it.Exists {
				flowDocument = json.RawMessage(`"delete"`)
			} else {
				// Ignore items which do not exist and are already deleted
				continue
			}
		}

		if err := b.store.stage.start(ctx, d.db); err != nil {
			return nil, err
		}
		converted, err := b.target.ConvertAll(it.Key, it.Values, flowDocument)
		if err != nil {
			return nil, fmt.Errorf("converting Store: %w", err)
		}
		if err = b.store.stage.encodeRow(converted); err != nil {
			return nil, fmt.Errorf("encoding Store to scratch file: %w", err)
		}
		b.store.mergeBounds.NextKey(converted[:len(b.target.Keys)])
	}

	// Upload the staged files and build a list of merge and copy into queries that need to be run
	// to effectively commit the files into destination tables. These queries are stored
	// in the checkpoint so that if the connector is restarted in middle of a commit
	// it can run the same queries on the next startup. This is the pattern for
	// recovery log being authoritative and the connector idempotently applies a commit
	// These are keyed on the binding table name so that in case of a recovery being necessary
	// we don't run queries belonging to bindings that have been removed
	for _, b := range d.bindings {
		if !b.store.stage.started {
			continue
		}

		dir, err := b.store.stage.flush()
		if err != nil {
			return nil, err
		}

		if b.store.mustMerge {
			mergeIntoQuery, err := renderMergeQueryTemplate(d.templates.mergeInto, b.target, dir, b.store.mergeBounds.Build())
			if err != nil {
				return nil, fmt.Errorf("mergeInto template: %w", err)
			}
			d.cp[b.target.StateKey] = &checkpointItem{
				Table:     b.target.Identifier,
				Query:     mergeIntoQuery,
				StagedDir: dir,
				Version:   d.version,
			}
			// Reset for next round.
			b.store.mustMerge = false
		} else if b.pipeName != "" {
			var pipeNameParts = strings.Split(b.pipeName, ".")
			var pipeNameLastPart = pipeNameParts[len(pipeNameParts)-1]

			// Check to see if a pipe for this version already exists
			exists, err := d.pipeExists(ctx, pipeNameLastPart)
			if err != nil {
				return nil, err
			}

			// Only create the pipe if it doesn't exist. Since the pipe name is versioned by the spec
			// it means if the spec has been updated, we will end up creating a new pipe
			if !exists {
				log.WithField("name", b.pipeName).Info("store: creating pipe")
				if createPipe, err := renderTableShardVersionTemplate(b.target, d._range.KeyBegin, d.version, d.templates.createPipe); err != nil {
					return nil, fmt.Errorf("createPipe template: %w", err)
				} else if _, err := d.db.ExecContext(ctx, createPipe); err != nil {
					return nil, fmt.Errorf("creating pipe for table %q: %w", b.target.Path, err)
				}
			}

			// Our understanding is that CREATE PIPE is _eventually consistent_, and so we
			// wait until we can make sure the pipe exists before continuing
			for !exists {
				exists, err = d.pipeExists(ctx, pipeNameLastPart)
				if err != nil {
					return nil, err
				}
				if !exists {
					time.Sleep(5 * time.Second)
				}
			}

			d.cp[b.target.StateKey] = &checkpointItem{
				Table:     b.target.Identifier,
				StagedDir: dir,
				PipeFiles: b.store.stage.uploaded,
				PipeName:  b.pipeName,
				Version:   d.version,
			}

		} else {
			if copyIntoQuery, err := renderTableAndFileTemplate(b.target, dir, d.templates.copyInto); err != nil {
				return nil, fmt.Errorf("copyInto template: %w", err)
			} else {
				d.cp[b.target.StateKey] = &checkpointItem{
					Table:     b.target.Identifier,
					Query:     copyIntoQuery,
					StagedDir: dir,
				}
			}
		}
	}

	return func(ctx context.Context, runtimeCheckpoint *protocol.Checkpoint) (*pf.ConnectorState, m.OpFuture) {
		var checkpointJSON, err = json.Marshal(d.cp)
		if err != nil {
			return nil, m.FinishedOperation(fmt.Errorf("creating checkpoint json: %w", err))
		}

		return &pf.ConnectorState{UpdatedJson: checkpointJSON, MergePatch: true}, nil
	}, nil
}

type pipeRecord struct {
	files     []fileRecord
	dir       string
	tableName string
}

// When a file has been successfully loaded, we remove it from the pipe record
func (pipe *pipeRecord) fileLoaded(file string) bool {
	for i, f := range pipe.files {
		if f.Path == file {
			pipe.files = append(pipe.files[:i], pipe.files[i+1:]...)
			return true
		}
	}

	return false
}

type copyHistoryRow struct {
	fileName          string
	status            string
	firstErrorMessage string
}

func (d *transactor) copyHistory(ctx context.Context, tableName string, fileNames []string) ([]copyHistoryRow, error) {
	query, err := renderCopyHistoryTemplate(tableName, fileNames, d.templates.copyHistory)
	if err != nil {
		return nil, fmt.Errorf("snowpipe: rendering copy history: %w", err)
	}

	rows, err := d.store.conn.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("snowpipe: fetching copy history: %w", err)
	}
	defer rows.Close()

	var items []copyHistoryRow

	var (
		fileName                  string
		status                    string
		firstErrorMessage         string
		firstErrorMessageNullable stdsql.NullString
	)

	for rows.Next() {
		if err := rows.Scan(&fileName, &status, &firstErrorMessageNullable); err != nil {
			return nil, fmt.Errorf("scanning copy history row: %w", err)
		}

		if firstErrorMessageNullable.Valid {
			firstErrorMessage = firstErrorMessageNullable.String
		}
		log.WithFields(log.Fields{
			"fileName":          fileName,
			"status":            status,
			"firstErrorMessage": firstErrorMessage,
			"tableName":         tableName,
		}).Info("snowpipe: copy history row")

		items = append(items, copyHistoryRow{
			fileName:          fileName,
			status:            status,
			firstErrorMessage: firstErrorMessage,
		})
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("snowpipe: reading copy history: %w", err)
	}

	return items, nil
}

// Acknowledge merges data from temporary table to main table
func (d *transactor) Acknowledge(ctx context.Context) (*pf.ConnectorState, error) {
	// Run store queries concurrently, as each independently operates on a separate table.
	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(MaxConcurrentQueries)

	var pipes = make(map[string]*pipeRecord)
	for stateKey, item := range d.cp {
		path := d.pathForStateKey(stateKey)
		// we skip queries that belong to tables which do not have a binding anymore
		// since these tables might be deleted already
		if len(path) == 0 {
			continue
		}

		if len(item.Query) > 0 {
			item := item
			group.Go(func() error {
				d.be.StartedResourceCommit(path)
				if _, err := d.db.ExecContext(groupCtx, item.Query); err != nil {
					return fmt.Errorf("query %q failed: %w", item.Query, err)
				}

				d.be.FinishedResourceCommit(path)
				if err := d.deleteFiles(ctx, []string{item.StagedDir}); err != nil {
					return fmt.Errorf("cleaning up files: %w", err)
				}

				return nil
			})
		} else if len(item.PipeFiles) > 0 {
			log.WithField("table", item.Table).Info("store: starting pipe requests")
			var fileRequests = make([]FileRequest, len(item.PipeFiles))
			for i, f := range item.PipeFiles {
				fileRequests[i] = FileRequest(f)
			}

			if resp, err := d.pipeClient.InsertFiles(item.PipeName, fileRequests); err != nil {
				var insertErr InsertFilesError
				if errors.As(err, &insertErr) && insertErr.Code == "390404" && strings.Contains(insertErr.Message, "Pipe not found") {
					// This error can happen if either the pipe was not found, or we are not authorized to use it
					// It is possible to not be authorized to use the pipe even though we have created it. When creating the pipe
					// we specify the role by which to create the pipe, but when using Snowpipe, we can't specify a role. Rather, the
					// default role of the user must have access to use the pipe. So here we make an additional check to make sure
					// the user has access to the pipe directly, or it has a default role which has access, otherwise
					// we advise the user to set a default role.
					var pipeNameParts = strings.Split(item.PipeName, ".")
					var pipeNameLastPart = pipeNameParts[len(pipeNameParts)-1]

					if exists, err := d.pipeExists(ctx, pipeNameLastPart); err != nil {
						return nil, fmt.Errorf("checking pipe existence %q: %w", item.PipeName, err)
					} else if exists {
						return nil, fmt.Errorf("pipe exists %q but Snowpipe cannot access it. This is most likely because the user does not have access to pipes through its default role. Try setting the default role of the user:\nALTER USER %s SET DEFAULT_ROLE=%s", pipeNameLastPart, d.cfg.Credentials.User, d.cfg.Role)
					}

					// Pipe was not found for this checkpoint item. We take this to mean that this item has already
					// been processed and the pipe has been cleaned up by us in a prior Acknowledge
					log.WithField("pipeName", item.PipeName).Info("pipe does not exist, skipping this checkpoint item")
					continue
				}
				return nil, fmt.Errorf("snowpipe insertFiles: %w", err)
			} else {
				log.WithField("response", resp).Debug("insertFiles successful")
			}

			pipes[item.PipeName] = &pipeRecord{
				files:     item.PipeFiles,
				dir:       item.StagedDir,
				tableName: item.Table,
			}
		}
	}

	if err := group.Wait(); err != nil {
		return nil, fmt.Errorf("executing concurrent store query: %w", err)
	}

	// Keep asking for a report on the files that have been submitted for processing
	// until they have all been successful, or an error has been thrown

	// If we see no results from the REST API for `maxTries` iterations, then we
	// fallback to asking the `COPY_HISTORY` table. We allow up to 5 minutes for results to show up in the REST API.
	if len(pipes) > 0 {
		// 5 minutes divided by three seconds for each pipe
		var maxTries = (5 * 60) / (3 * len(pipes))
		var retryDelay = 3 * time.Second
		for tries := 0; tries < maxTries; tries++ {
			for pipeName, pipe := range pipes {
				// if the pipe has no files to begin with, just skip it
				// we might have processed all files of this pipe previously
				if len(pipe.files) == 0 {
					continue
				}

				// We first try to check the status of pipes using the REST API's insertReport
				// The REST API does not wake up the warehouse, hence our preference
				// however this API is not the most ergonomic and sometimes does not yield
				// results as expected
				report, err := d.pipeClient.InsertReport(pipeName)
				if err != nil {
					return nil, fmt.Errorf("snowpipe: insertReports: %w", err)
				}

				// If the files have already been loaded, when we submit a request to
				// load those files again, our request will not show up in reports.
				// Moreover, insertReport only retains events for 10 minutes.
				// One way to find out whether the files were successfully loaded is to check
				// the COPY_HISTORY to make sure they are there. The COPY_HISTORY is much more
				// reliable. If they are not there, then something is wrong.
				if len(report.Files) == 0 {
					// We try `maxTries` times since it may take some time for the REST API
					// to reflect the new pipe requests
					if tries < maxTries-1 {
						time.Sleep(retryDelay)
						continue
					}

					log.WithFields(log.Fields{
						"tries": tries,
						"pipe":  pipeName,
					}).Info("snowpipe: no files in report, fetching copy history from warehouse")

					var fileNames = make([]string, len(pipe.files))
					for i, f := range pipe.files {
						fileNames[i] = f.Path
					}

					rows, err := d.copyHistory(ctx, pipe.tableName, fileNames)
					if err != nil {
						return nil, err
					}

					// If there are items still in progress, we continue retrying until those items
					// resolve to another status
					var hasItemsInProgress = false

					for _, row := range rows {
						if row.status == "Loaded" {
							pipe.fileLoaded(row.fileName)
						} else if row.status == "Load in progress" {
							hasItemsInProgress = true
						} else {
							return nil, fmt.Errorf("unexpected status %q for files in pipe %q: %s", row.status, pipeName, row.firstErrorMessage)
						}
					}

					// If items are still in progress, we continue trying to fetch their results
					if hasItemsInProgress {
						tries--
						time.Sleep(retryDelay)
						continue
					}

					if len(pipe.files) > 0 {
						return nil, fmt.Errorf("snowpipe: could not find reports of successful processing for all files of pipe %v", pipe)
					}

					// All files have been processed for this pipe, we can skip to the next pipe
					if err := d.deleteFiles(ctx, []string{pipe.dir}); err != nil {
						return nil, fmt.Errorf("cleaning up files: %w", err)
					}
					continue
				}

				// So long as we are able to get some results from the REST API, we do not
				// want to ask COPY_HISTORY. So we reset the counter if we see some results from the REST API
				tries = 0

				for _, reportFile := range report.Files {
					if reportFile.Status == "LOADED" {
						pipe.fileLoaded(reportFile.Path)
					} else if reportFile.Status == "LOAD_FAILED" || reportFile.Status == "PARTIALLY_LOADED" {
						return nil, fmt.Errorf("failed to load files in pipe %q: %s, %s", pipeName, reportFile.FirstError, reportFile.SystemError)
					} else if reportFile.Status == "LOAD_IN_PROGRESS" {
						continue
					}
				}

				if len(pipe.files) == 0 {
					if err := d.deleteFiles(ctx, []string{pipe.dir}); err != nil {
						return nil, fmt.Errorf("cleaning up files: %w", err)
					}
					continue
				} else {
					time.Sleep(retryDelay)
				}
			}
		}
	}

	// After having applied the checkpoint, we try to clean up the checkpoint in the ack response
	// so that a restart of the connector does not need to run the same queries again
	// Note that this is an best-effort "attempt" and there is no guarantee that this checkpoint update
	// can actually be committed
	// Important to note that in this case we do not reset the checkpoint for all bindings, but only the ones
	// that have been committed in this transaction. The reason is that it may be the case that a binding
	// which has been disabled right after a failed attempt to run its queries, must be able to recover by enabling
	// the binding and running the queries that are pending for its last transaction.
	var checkpointClear = make(checkpoint)
	for _, b := range d.bindings {
		checkpointClear[b.target.StateKey] = nil
		delete(d.cp, b.target.StateKey)
	}
	// Also clean up checkpoint items which are for previous versions of active bindings
	for stateKey, item := range d.cp {
		var key = strings.Split(stateKey, ".")[0]
		for _, b := range d.bindings {
			var key2 = strings.Split(b.target.StateKey, ".")[0]
			if key == key2 && item.PipeName != b.pipeName {
				checkpointClear[stateKey] = nil
				delete(d.cp, stateKey)

				log.WithField("pipeName", item.PipeName).WithField("stateKey", stateKey).Info("dropping pipe and clearing checkpoint")
				if _, err := d.store.conn.ExecContext(ctx, fmt.Sprintf("DROP PIPE IF EXISTS %s", d.ep.Dialect.Identifier(item.PipeName))); err != nil {
					return nil, fmt.Errorf("dropping pipe %s failed: %w", item.PipeName, err)
				}
			}
		}
	}
	checkpointJSON, err := json.Marshal(checkpointClear)
	if err != nil {
		return nil, fmt.Errorf("creating checkpoint clearing json: %w", err)
	}

	return &pf.ConnectorState{UpdatedJson: json.RawMessage(checkpointJSON), MergePatch: true}, nil
}

func (d *transactor) pathForStateKey(stateKey string) []string {
	for _, b := range d.bindings {
		if b.target.StateKey == stateKey {
			return b.target.Path
		}
	}
	return nil
}

func (d *transactor) deleteFiles(ctx context.Context, files []string) error {
	for _, f := range files {
		if _, err := d.store.conn.ExecContext(ctx, fmt.Sprintf("REMOVE %s", f)); err != nil {
			return err
		}
	}

	return nil
}

func (d *transactor) Destroy() {
	d.load.conn.Close()
	d.store.conn.Close()
	d.db.Close()
}

func main() {
	// gosnowflake also uses logrus for logging and the logs it produces may be confusing when
	// intermixed with our connector logs. We disable the gosnowflake logger here and log as needed
	// when handling errors from the sql driver.
	sf.GetLogger().SetOutput(io.Discard)
	boilerplate.RunMain(newSnowflakeDriver())
}
