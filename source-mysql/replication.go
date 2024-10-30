package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/estuary/connectors/sqlcapture"
	"github.com/go-mysql-org/go-mysql/mysql"
	"github.com/go-mysql-org/go-mysql/replication"
	"github.com/google/uuid"
	"github.com/mitchellh/mapstructure"
	"github.com/sirupsen/logrus"
	"vitess.io/vitess/go/vt/sqlparser"
)

var (
	// replicationBufferSize controls how many change events can be buffered in the
	// replicationStream before it stops receiving further events from MySQL.
	// In normal use it's a constant, it's just a variable so that tests are more
	// likely to exercise blocking sends and backpressure.
	replicationBufferSize = 256

	// binlogEventCacheCount controls how many binlog events will be buffered inside
	// the client library before we receive them.
	binlogEventCacheCount = 256

	// streamToFenceWatchdogTimeout is the length of time after which a stream-to-fence
	// operation will error out if no further events are received when there ought to be
	// some. This should never be hit in normal operation, and exists only so that certain
	// rare failure modes produce an error rather than blocking forever.
	streamToFenceWatchdogTimeout = 5 * time.Minute
)

func (db *mysqlDatabase) ReplicationStream(ctx context.Context, startCursor string) (sqlcapture.ReplicationStream, error) {
	var address = db.config.Address
	// If SSH Tunnel is configured, we are going to create a tunnel from localhost:5432
	// to address through the bastion server, so we use the tunnel's address
	if db.config.NetworkTunnel != nil && db.config.NetworkTunnel.SSHForwarding != nil && db.config.NetworkTunnel.SSHForwarding.SSHEndpoint != "" {
		address = "localhost:3306"
	}
	var host, port, err = splitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("invalid mysql address: %w", err)
	}

	var pos mysql.Position
	if startCursor != "" {
		pos, err = parseCursor(startCursor)
		if err != nil {
			return nil, fmt.Errorf("invalid resume cursor: %w", err)
		}
		logrus.WithField("pos", pos).Debug("resuming from binlog position")
	} else {
		pos, err = db.queryBinlogPosition()
		if err != nil {
			return nil, err
		}
		logrus.WithField("pos", pos).Debug("initialized binlog position")
	}

	var syncConfig = replication.BinlogSyncerConfig{
		ServerID: uint32(db.config.Advanced.NodeID),
		Flavor:   "mysql", // TODO(wgd): See what happens if we change this and run against MariaDB?
		Host:     host,
		Port:     uint16(port),
		User:     db.config.User,
		Password: db.config.Password,
		// TODO(wgd): Maybe add 'serverName' checking as described over in Connect()
		TLSConfig: &tls.Config{InsecureSkipVerify: true},
		// Request that timestamp values coming via replication be interpreted as UTC.
		TimestampStringLocation: time.UTC,

		// Limit connection retries so unreachability eventually bubbles up into task failure.
		MaxReconnectAttempts: 10,

		// Request heartbeat events from the server every 30s and time out waiting after 5m.
		// The heartbeats ensure that the TCP connection remains alive when no changes are
		// occurring and the read timeout ensures that we will detect silent link failures
		// and fail+restart the capture within a few minutes at worst.
		HeartbeatPeriod: 30 * time.Second,
		ReadTimeout:     5 * time.Minute,

		// Output replication log messages with Logrus the same as our own connector messages.
		Logger: logrus.StandardLogger(),

		// Allow the binlog syncer to buffer a few events internally for speed, but not too many.
		EventCacheCount: binlogEventCacheCount,
	}

	logrus.WithFields(logrus.Fields{"pos": pos}).Info("starting replication")
	var streamer *replication.BinlogStreamer
	var syncer = replication.NewBinlogSyncer(syncConfig)
	if streamer, err = syncer.StartSync(pos); err == nil {
		logrus.Debug("replication connected with TLS")
	} else {
		syncer.Close()
		syncConfig.TLSConfig = nil
		syncer = replication.NewBinlogSyncer(syncConfig)
		if streamer, err = syncer.StartSync(pos); err == nil {
			logrus.Info("replication connected without TLS")
		} else {
			return nil, fmt.Errorf("error starting binlog sync: %w", err)
		}
	}

	var stream = &mysqlReplicationStream{
		db:            db,
		syncer:        syncer,
		streamer:      streamer,
		startPosition: pos,
		fencePosition: pos,
	}
	stream.tables.active = make(map[string]struct{})
	stream.tables.discovery = make(map[string]sqlcapture.DiscoveryInfo)
	stream.tables.metadata = make(map[string]*mysqlTableMetadata)
	stream.tables.keyColumns = make(map[string][]string)
	stream.tables.nonTransactional = make(map[string]bool)
	return stream, nil
}

func parseCursor(cursor string) (mysql.Position, error) {
	seps := strings.Split(cursor, ":")
	if len(seps) != 2 {
		return mysql.Position{}, fmt.Errorf("input %q must have <logfile>:<offset> shape", cursor)
	}
	offset, err := strconv.ParseInt(seps[1], 10, 64)
	if err != nil {
		return mysql.Position{}, fmt.Errorf("invalid offset value %q: %w", seps[1], err)
	}
	return mysql.Position{
		Name: seps[0],
		Pos:  uint32(offset),
	}, nil
}

func splitHostPort(addr string) (string, int64, error) {
	seps := strings.Split(addr, ":")
	if len(seps) != 2 {
		return "", 0, fmt.Errorf("input %q must have <host>:<port> shape", addr)
	}
	port, err := strconv.ParseInt(seps[1], 10, 64)
	if err != nil {
		return "", 0, fmt.Errorf("invalid port number %q: %w", seps[1], err)
	}
	return seps[0], port, nil
}

type mysqlReplicationStream struct {
	db            *mysqlDatabase
	syncer        *replication.BinlogSyncer
	streamer      *replication.BinlogStreamer
	startPosition mysql.Position                // The start position when the stream was created. Never updated.
	fencePosition mysql.Position                // The latest fence position, updated at the end of each StreamToFence cycle.
	events        chan sqlcapture.DatabaseEvent // Output channel from replication worker goroutine

	cancel context.CancelFunc // Cancellation thunk for the replication worker goroutine
	errCh  chan error         // Error output channel for the replication worker goroutine

	gtidTimestamp time.Time // The OriginalCommitTimestamp value of the last GTID Event
	gtidString    string    // The GTID value of the last GTID event, formatted as a "<uuid>:<counter>" string.

	uncommittedChanges      int // A count of row changes since the last commit was processed.
	nonTransactionalChanges int // A count of row changes *on non-transactional tables* since the last commit was processed.

	// The active tables set and associated metadata, guarded by a
	// mutex so it can be modified from the main goroutine while it's
	// read from the replication goroutine.
	tables struct {
		sync.RWMutex
		active           map[sqlcapture.StreamID]struct{}
		discovery        map[sqlcapture.StreamID]sqlcapture.DiscoveryInfo
		metadata         map[sqlcapture.StreamID]*mysqlTableMetadata
		keyColumns       map[sqlcapture.StreamID][]string
		nonTransactional map[sqlcapture.StreamID]bool
		dirtyMetadata    []sqlcapture.StreamID
	}
}

type mysqlTableMetadata struct {
	Schema         mysqlTableSchema `json:"schema"`
	DefaultCharset string           `json:"charset,omitempty"`
}

type mysqlTableSchema struct {
	Columns     []string               `json:"columns"`
	ColumnTypes map[string]interface{} `json:"types"`
}

func (rs *mysqlReplicationStream) StartReplication(ctx context.Context, _ map[sqlcapture.StreamID]*sqlcapture.DiscoveryInfo) error {
	if rs.cancel != nil {
		return fmt.Errorf("internal error: replication stream already started")
	}

	var streamCtx, streamCancel = context.WithCancel(ctx)
	rs.events = make(chan sqlcapture.DatabaseEvent, replicationBufferSize)
	rs.errCh = make(chan error)
	rs.cancel = streamCancel

	go func() {
		var err = rs.run(streamCtx, rs.startPosition)
		if errors.Is(err, context.Canceled) {
			err = nil
		}
		rs.syncer.Close()
		close(rs.events)
		rs.errCh <- err
	}()
	return nil
}

func (rs *mysqlReplicationStream) run(ctx context.Context, startCursor mysql.Position) error {
	var cursor = startCursor

	for {
		// Process the next binlog event from the database.
		var event, err = rs.streamer.GetEvent(ctx)
		if err != nil {
			return fmt.Errorf("error getting next event: %w", err)
		}

		if event.Header.LogPos > 0 {
			cursor.Pos = event.Header.LogPos
		}

		// Events which are neither row changes nor commits need to be reported as an
		// implicit FlushEvent if and only if there are no uncommitted changes. This
		// avoids certain edge cases in the positional fence implementation.
		var implicitFlush = false

		switch data := event.Event.(type) {
		case *replication.RowsEvent:
			var schema, table = string(data.Table.Schema), string(data.Table.Table)
			var streamID = sqlcapture.JoinStreamID(schema, table)

			// Skip change events from tables which aren't being captured. Send a KeepaliveEvent
			// to indicate that we are actively receiving events, just not ones that need to be
			// decoded and processed further.
			if !rs.tableActive(streamID) {
				if err := rs.emitEvent(ctx, &sqlcapture.KeepaliveEvent{}); err != nil {
					return err
				}
				continue
			}

			var sourceCommon = sqlcapture.SourceCommon{
				Schema: schema,
				Table:  table,
			}
			if !rs.gtidTimestamp.IsZero() {
				sourceCommon.Millis = rs.gtidTimestamp.UnixMilli()
			}

			// Get column names and types from persistent metadata. If available, allow
			// override the persistent column name tracking using binlog row metadata.
			var metadata, ok = rs.tableMetadata(streamID)
			if !ok || metadata == nil {
				return fmt.Errorf("missing metadata for stream %q", streamID)
			}
			var columnTypes = metadata.Schema.ColumnTypes
			var columnNames = data.Table.ColumnNameString()
			if len(columnNames) == 0 {
				columnNames = metadata.Schema.Columns
			}

			keyColumns, ok := rs.keyColumns(streamID)
			if !ok {
				return fmt.Errorf("unknown key columns for stream %q", streamID)
			}

			var nonTransactionalTable = rs.isNonTransactional(streamID)

			switch event.Header.EventType {
			case replication.WRITE_ROWS_EVENTv1, replication.WRITE_ROWS_EVENTv2:
				for rowIdx, row := range data.Rows {
					var after, err = decodeRow(streamID, columnNames, row, data.SkippedColumns[rowIdx])
					if err != nil {
						return fmt.Errorf("error decoding row values: %w", err)
					}
					rowKey, err := sqlcapture.EncodeRowKey(keyColumns, after, columnTypes, encodeKeyFDB)
					if err != nil {
						return fmt.Errorf("error encoding row key for %q: %w", streamID, err)
					}
					if err := rs.db.translateRecordFields(false, columnTypes, after); err != nil {
						return fmt.Errorf("error translating 'after' of %q InsertOp: %w", streamID, err)
					}
					var sourceInfo = &mysqlSourceInfo{
						SourceCommon: sourceCommon,
						EventCursor:  fmt.Sprintf("%s:%d:%d", cursor.Name, cursor.Pos, rowIdx),
					}
					if rs.db.includeTxIDs[streamID] {
						sourceInfo.TxID = rs.gtidString
					}
					if err := rs.emitEvent(ctx, &sqlcapture.ChangeEvent{
						Operation: sqlcapture.InsertOp,
						RowKey:    rowKey,
						After:     after,
						Source:    sourceInfo,
					}); err != nil {
						return err
					}
					rs.uncommittedChanges++ // Keep a count of all uncommitted changes
					if nonTransactionalTable {
						rs.nonTransactionalChanges++ // Keep a count of uncommitted non-transactional changes
					}
				}
			case replication.UPDATE_ROWS_EVENTv1, replication.UPDATE_ROWS_EVENTv2:
				for rowIdx := range data.Rows {
					// Update events contain alternating (before, after) pairs of rows
					if rowIdx%2 == 1 {
						before, err := decodeRow(streamID, columnNames, data.Rows[rowIdx-1], data.SkippedColumns[rowIdx-1])
						if err != nil {
							return fmt.Errorf("error decoding row values: %w", err)
						}
						after, err := decodeRow(streamID, columnNames, data.Rows[rowIdx], data.SkippedColumns[rowIdx])
						if err != nil {
							return fmt.Errorf("error decoding row values: %w", err)
						}
						after = mergePreimage(after, before)
						rowKey, err := sqlcapture.EncodeRowKey(keyColumns, after, columnTypes, encodeKeyFDB)
						if err != nil {
							return fmt.Errorf("error encoding row key for %q: %w", streamID, err)
						}
						if err := rs.db.translateRecordFields(false, columnTypes, before); err != nil {
							return fmt.Errorf("error translating 'before' of %q UpdateOp: %w", streamID, err)
						}
						if err := rs.db.translateRecordFields(false, columnTypes, after); err != nil {
							return fmt.Errorf("error translating 'after' of %q UpdateOp: %w", streamID, err)
						}
						var sourceInfo = &mysqlSourceInfo{
							SourceCommon: sourceCommon,
							EventCursor:  fmt.Sprintf("%s:%d:%d", cursor.Name, cursor.Pos, rowIdx/2),
						}
						if rs.db.includeTxIDs[streamID] {
							sourceInfo.TxID = rs.gtidString
						}
						if err := rs.emitEvent(ctx, &sqlcapture.ChangeEvent{
							Operation: sqlcapture.UpdateOp,
							RowKey:    rowKey,
							Before:    before,
							After:     after,
							Source:    sourceInfo,
						}); err != nil {
							return err
						}
						rs.uncommittedChanges++ // Keep a count of all uncommitted changes
						if nonTransactionalTable {
							rs.nonTransactionalChanges++ // Keep a count of uncommitted non-transactional changes
						}
					}
				}
			case replication.DELETE_ROWS_EVENTv1, replication.DELETE_ROWS_EVENTv2:
				for rowIdx, row := range data.Rows {
					var before, err = decodeRow(streamID, columnNames, row, data.SkippedColumns[rowIdx])
					if err != nil {
						return fmt.Errorf("error decoding row values: %w", err)
					}
					rowKey, err := sqlcapture.EncodeRowKey(keyColumns, before, columnTypes, encodeKeyFDB)
					if err != nil {
						return fmt.Errorf("error encoding row key for %q: %w", streamID, err)
					}
					if err := rs.db.translateRecordFields(false, columnTypes, before); err != nil {
						return fmt.Errorf("error translating 'before' of %q DeleteOp: %w", streamID, err)
					}
					var sourceInfo = &mysqlSourceInfo{
						SourceCommon: sourceCommon,
						EventCursor:  fmt.Sprintf("%s:%d:%d", cursor.Name, cursor.Pos, rowIdx),
						TxID:         rs.gtidString,
					}
					if rs.db.includeTxIDs[streamID] {
						sourceInfo.TxID = rs.gtidString
					}
					if err := rs.emitEvent(ctx, &sqlcapture.ChangeEvent{
						Operation: sqlcapture.DeleteOp,
						RowKey:    rowKey,
						Before:    before,
						Source:    sourceInfo,
					}); err != nil {
						return err
					}
					rs.uncommittedChanges++ // Keep a count of all uncommitted changes
					if nonTransactionalTable {
						rs.nonTransactionalChanges++ // Keep a count of uncommitted non-transactional changes
					}
				}
			default:
				return fmt.Errorf("unknown row event type: %q", event.Header.EventType)
			}
		case *replication.XIDEvent:
			logrus.WithFields(logrus.Fields{
				"xid":    data.XID,
				"cursor": cursor,
			}).Trace("XID Event")
			if err := rs.emitEvent(ctx, &sqlcapture.FlushEvent{
				Cursor: fmt.Sprintf("%s:%d", cursor.Name, cursor.Pos),
			}); err != nil {
				return err
			}
			rs.uncommittedChanges = 0      // Reset count of all uncommitted changes
			rs.nonTransactionalChanges = 0 // Reset count of uncommitted non-transactional changes
		case *replication.TableMapEvent:
			logrus.WithField("data", data).Trace("Table Map Event")
		case *replication.GTIDEvent:
			implicitFlush = true // Implicit FlushEvent conversion permitted
			logrus.WithField("data", data).Trace("GTID Event")
			rs.gtidTimestamp = data.OriginalCommitTime()

			if len(data.SID) != 16 || (data.GNO == 0 && bytes.Equal(data.SID, []byte("\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00"))) {
				rs.gtidString = ""
			} else {
				var sourceUUID, err = uuid.FromBytes(data.SID)
				if err != nil {
					return fmt.Errorf("internal error: failed to parse GTID source %q: %w", data.SID, err)
				}
				rs.gtidString = fmt.Sprintf("%s:%d", sourceUUID.String(), data.GNO)
			}
		case *replication.PreviousGTIDsEvent:
			implicitFlush = true // Implicit FlushEvent conversion permitted
			logrus.WithField("gtids", data.GTIDSets).Trace("PreviousGTIDs Event")
		case *replication.QueryEvent:
			if string(data.Query) == "COMMIT" && rs.nonTransactionalChanges > 0 {
				// If there are uncommitted non-transactional changes, we should treat a
				// "COMMIT" query event as a transaction commit.
				if err := rs.emitEvent(ctx, &sqlcapture.FlushEvent{
					Cursor: fmt.Sprintf("%s:%d", cursor.Name, cursor.Pos),
				}); err != nil {
					return err
				}
				rs.uncommittedChanges = 0      // Reset count of all uncommitted changes
				rs.nonTransactionalChanges = 0 // Reset count of uncommitted non-transactional changes
			} else {
				implicitFlush = true // Implicit FlushEvent conversion permitted
				if err := rs.handleQuery(ctx, string(data.Schema), string(data.Query)); err != nil {
					return fmt.Errorf("error processing query event: %w", err)
				}
			}
		case *replication.RotateEvent:
			implicitFlush = true // Implicit FlushEvent conversion permitted
			cursor.Name = string(data.NextLogName)
			cursor.Pos = uint32(data.Position)
			logrus.WithFields(logrus.Fields{
				"name": cursor.Name,
				"pos":  cursor.Pos,
			}).Trace("Rotate Event")
		case *replication.FormatDescriptionEvent:
			implicitFlush = true // Implicit FlushEvent conversion permitted
			logrus.WithField("data", data).Trace("Format Description Event")
		case *replication.GenericEvent:
			implicitFlush = true // Implicit FlushEvent conversion permitted
			if event.Header.EventType == replication.HEARTBEAT_EVENT {
				logrus.Debug("received server heartbeat")
			} else {
				logrus.WithField("event", event.Header.EventType.String()).Debug("Generic Event")
			}
		case *replication.RowsQueryEvent:
			implicitFlush = true // Implicit FlushEvent conversion permitted
			logrus.WithField("query", string(data.Query)).Debug("ignoring Rows Query Event")
		default:
			return fmt.Errorf("unhandled event type: %q", event.Header.EventType)
		}

		// If the binlog event is eligible for implicit FlushEvent reporting and there
		// are no uncommitted changes currently pending (for which we would want to wait
		// until a real commit event to flush the output) then report a new FlushEvent
		// with the latest position.
		if implicitFlush && rs.uncommittedChanges == 0 {
			if err := rs.emitEvent(ctx, &sqlcapture.FlushEvent{
				Cursor: fmt.Sprintf("%s:%d", cursor.Name, cursor.Pos),
			}); err != nil {
				return err
			}
		}
	}
}

// decodeRow takes a list of column names, a parallel list of column values, and a list of indices
// of columns which should be omitted from the decoded row state, and returns a map from colum names
// to the corresponding values.
func decodeRow(streamID string, colNames []string, row []any, skips []int) (map[string]any, error) {
	// If we have more or fewer values than expected, something has gone wrong
	// with our metadata tracking and it's best to die immediately. The fix in
	// this case is almost always going to be deleting and recreating the
	// capture binding for a particular table.
	if len(row) != len(colNames) {
		if len(colNames) == 0 {
			return nil, fmt.Errorf("metadata error (go.estuary.dev/eiKbOh): unknown column names for stream %q", streamID)
		}
		return nil, fmt.Errorf("metadata error (go.estuary.dev/eiKbOh): change event on stream %q contains %d values, expected %d", streamID, len(row), len(colNames))
	}

	var fields = make(map[string]interface{})
	for idx, val := range row {
		fields[colNames[idx]] = val
	}
	for _, omitColIdx := range skips {
		delete(fields, colNames[omitColIdx])
	}
	return fields, nil
}

// mergePreimage fills out any unspecified properties of the 'fields' map with the
// corresponding values from the 'preimage' map.
func mergePreimage(fields map[string]any, preimage map[string]any) map[string]any {
	for key, val := range preimage {
		if _, ok := fields[key]; !ok {
			fields[key] = val
		}
	}
	return fields
}

// Query Events in the MySQL binlog are normalized enough that we can use
// prefix matching to detect many types of query that we just completely
// don't care about. This is good, because the Vitess SQL parser disagrees
// with the binlog Query Events for some statements like GRANT and CREATE USER.
// TODO(johnny): SET STATEMENT is not safe in the general case, and we want to re-visit
// by extracting and ignoring a SET STATEMENT stanza prior to parsing.
var silentIgnoreQueriesRe = regexp.MustCompile(`(?i)^(BEGIN|COMMIT|SAVEPOINT .*|# [^\n]*)$`)
var createDefinerRegex = `CREATE\s*(OR REPLACE){0,1}\s*(ALGORITHM\s*=\s*[^ ]+)*\s*DEFINER`
var ignoreQueriesRe = regexp.MustCompile(`(?i)^(BEGIN|COMMIT|GRANT|REVOKE|CREATE USER|` + createDefinerRegex + `|DROP USER|ALTER USER|DROP PROCEDURE|DROP FUNCTION|DROP TRIGGER|SET STATEMENT|CREATE EVENT|ALTER EVENT|DROP EVENT)`)

func (rs *mysqlReplicationStream) handleQuery(ctx context.Context, schema, query string) error {
	// There are basically three types of query events we might receive:
	//   * An INSERT/UPDATE/DELETE query is an error, we should never receive
	//     these if the server's `binlog_format` is set to ROW as it should be
	//     for CDC to work properly.
	//   * Various DDL queries like CREATE/ALTER/DROP/TRUNCATE/RENAME TABLE,
	//     which should in general be treated like errors *if they occur on
	//     a table we're capturing*, though we expect to eventually handle
	//     some subset of possible alterations like adding/renaming columns.
	//   * Some other queries like BEGIN and CREATE DATABASE and other things
	//     that we don't care about, either because they change things that
	//     don't impact our capture or because we get the relevant information
	//     by some other means.
	if silentIgnoreQueriesRe.MatchString(query) {
		logrus.WithField("query", query).Trace("silently ignoring query event")
		return nil
	}
	if ignoreQueriesRe.MatchString(query) {
		logrus.WithField("query", query).Info("ignoring query event")
		return nil
	}
	logrus.WithField("query", query).Info("handling query event")

	var stmt, err = sqlparser.Parse(query)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"query": query,
			"err":   err,
		}).Warn("failed to parse query event, ignoring it")
		return nil
	}
	logrus.WithField("stmt", fmt.Sprintf("%#v", stmt)).Debug("parsed query")

	switch stmt := stmt.(type) {
	case *sqlparser.CreateDatabase, *sqlparser.AlterDatabase, *sqlparser.CreateTable, *sqlparser.Savepoint, *sqlparser.Flush:
		logrus.WithField("query", query).Debug("ignoring benign query")
	case *sqlparser.CreateView, *sqlparser.AlterView, *sqlparser.DropView:
		// All view creation/deletion/alterations should be fine to ignore since we don't capture from views.
		logrus.WithField("query", query).Debug("ignoring benign query")
	case *sqlparser.DropDatabase:
		// Remember that In MySQL land "database" is a synonym for the usual SQL concept "schema"
		if rs.schemaActive(stmt.GetDatabaseName()) {
			return fmt.Errorf("cannot handle query %q: schema %q is actively being captured", query, stmt.GetDatabaseName())
		}
		logrus.WithField("query", query).Debug("ignorable dropped schema (not being captured from)")
	case *sqlparser.AlterTable:
		if streamID := resolveTableName(schema, stmt.Table); rs.tableActive(streamID) {
			logrus.WithFields(logrus.Fields{
				"query":         query,
				"partitionSpec": stmt.PartitionSpec,
				"alterOptions":  stmt.AlterOptions,
			}).Info("parsed components of ALTER TABLE statement")

			if stmt.PartitionSpec == nil || len(stmt.AlterOptions) != 0 {
				if err := rs.handleAlterTable(ctx, stmt, query, streamID); err != nil {
					return fmt.Errorf("cannot handle table alteration %q: %w", query, err)
				}
			}
		}
	case *sqlparser.DropTable:
		for _, table := range stmt.FromTables {
			if streamID := resolveTableName(schema, table); rs.tableActive(streamID) {
				// Indicate that change streaming for this table has failed.
				if err := rs.emitEvent(ctx, &sqlcapture.TableDropEvent{
					StreamID: streamID,
					Cause:    fmt.Sprintf("table %q was dropped by query %q", streamID, query),
				}); err != nil {
					return err
				} else if err := rs.deactivateTable(streamID); err != nil {
					return err
				}
			}
		}
	case *sqlparser.TruncateTable:
		if streamID := resolveTableName(schema, stmt.Table); rs.tableActive(streamID) {
			// Once we have a concept of collection-level truncation we will probably
			// want to either handle this like a dropped-and-recreated table or else
			// use another mechanism to produce the appropriate "the collection is
			// now truncated" signals here. But for now ignoring is still the best
			// we can do.
			logrus.WithField("table", streamID).Warn("ignoring TRUNCATE on active table")
		}
	case *sqlparser.RenameTable:
		for _, pair := range stmt.TablePairs {
			if streamID := resolveTableName(schema, pair.FromTable); rs.tableActive(streamID) {
				// Indicate that change streaming for this table has failed.
				if err := rs.emitEvent(ctx, &sqlcapture.TableDropEvent{
					StreamID: streamID,
					Cause:    fmt.Sprintf("table %q was renamed by query %q", streamID, query),
				}); err != nil {
					return err
				} else if err := rs.deactivateTable(streamID); err != nil {
					return err
				}
			}
		}
	case *sqlparser.Insert:
		if streamID := resolveTableName(schema, stmt.Table); rs.tableActive(streamID) {
			return fmt.Errorf("unsupported DML query (go.estuary.dev/IK5EVx): %s", query)
		}
	case *sqlparser.Update:
		// TODO(wgd): It would be nice to only halt on UPDATE statements impacting
		// active tables. Unfortunately UPDATE queries are complicated and it's not
		// as simple to implement that check as for INSERT and DELETE.
		return fmt.Errorf("unsupported DML query (go.estuary.dev/IK5EVx): %s", query)
	case *sqlparser.Delete:
		for _, target := range stmt.Targets {
			if streamID := resolveTableName(schema, target); rs.tableActive(streamID) {
				return fmt.Errorf("unsupported DML query (go.estuary.dev/IK5EVx): %s", query)
			}
		}
	case *sqlparser.OtherAdmin, *sqlparser.OtherRead:
		logrus.WithField("query", query).Debug("ignoring benign query")
	default:
		return fmt.Errorf("unhandled query (go.estuary.dev/ceqr74): unhandled type %q: %q", reflect.TypeOf(stmt).String(), query)
	}

	return nil
}

func (rs *mysqlReplicationStream) handleAlterTable(ctx context.Context, stmt *sqlparser.AlterTable, query string, streamID string) error {
	// This lock and assignment to `meta` isn't actually needed unless we are able to handle the
	// alteration. But if we can't handle the alteration the connector is probably going to crash,
	// so any performance implication is negligible at that point and it makes things a little
	// easier to get the lock here.
	rs.tables.Lock()
	defer rs.tables.Unlock()
	meta := rs.tables.metadata[streamID]

	for _, alterOpt := range stmt.AlterOptions {
		switch alter := alterOpt.(type) {
		// These should be all of the table alterations which might possibly impact our capture
		// in ways we don't currently support, so the default behavior can be to log and ignore.
		case *sqlparser.RenameColumn:
			var oldName = alter.OldName.Name.String()
			var newName = alter.NewName.Name.String()

			var colIndex = slices.Index(meta.Schema.Columns, oldName)
			if colIndex == -1 {
				return fmt.Errorf("unknown column %q", oldName)
			}
			meta.Schema.Columns[colIndex] = newName

			var colType = meta.Schema.ColumnTypes[oldName]
			meta.Schema.ColumnTypes[oldName] = nil
			meta.Schema.ColumnTypes[newName] = colType
		case *sqlparser.RenameTableName:
			return fmt.Errorf("unsupported table alteration (go.estuary.dev/eVVwet): %s", query)
		case *sqlparser.ChangeColumn:
			var oldName = alter.OldColumn.Name.String()
			var oldIndex = slices.Index(meta.Schema.Columns, oldName)
			if oldIndex == -1 {
				return fmt.Errorf("unknown column %q", oldName)
			}
			meta.Schema.Columns = slices.Delete(meta.Schema.Columns, oldIndex, oldIndex+1)

			var newName = alter.NewColDefinition.Name.String()
			var newType = translateDataType(meta, alter.NewColDefinition.Type)
			var newIndex = oldIndex
			if alter.First {
				newIndex = 0
			} else if alter.After != nil {
				var afterName = alter.After.Name.String()
				var afterIndex = slices.Index(meta.Schema.Columns, afterName)
				if afterIndex == -1 {
					return fmt.Errorf("unknown column %q", afterName)
				}
				newIndex = afterIndex + 1
			}
			meta.Schema.Columns = slices.Insert(meta.Schema.Columns, newIndex, newName)
			meta.Schema.ColumnTypes[oldName] = nil // Set to nil rather than delete so that JSON patch merging deletes it
			meta.Schema.ColumnTypes[newName] = newType
			logrus.WithField("columns", meta.Schema.Columns).WithField("types", meta.Schema.ColumnTypes).Info("processed CHANGE COLUMN alteration")
		case *sqlparser.ModifyColumn:
			var colName = alter.NewColDefinition.Name.String()
			var oldIndex = slices.Index(meta.Schema.Columns, colName)
			if oldIndex == -1 {
				return fmt.Errorf("unknown column %q", colName)
			}
			meta.Schema.Columns = slices.Delete(meta.Schema.Columns, oldIndex, oldIndex+1)

			var newType = translateDataType(meta, alter.NewColDefinition.Type)
			var newIndex = oldIndex
			if alter.First {
				newIndex = 0
			} else if alter.After != nil {
				var afterName = alter.After.Name.String()
				var afterIndex = slices.Index(meta.Schema.Columns, afterName)
				if afterIndex == -1 {
					return fmt.Errorf("unknown column %q", afterName)
				}
				newIndex = afterIndex + 1
			}

			meta.Schema.Columns = slices.Insert(meta.Schema.Columns, newIndex, colName)
			meta.Schema.ColumnTypes[colName] = newType
			logrus.WithField("columns", meta.Schema.Columns).WithField("types", meta.Schema.ColumnTypes).Info("processed MODIFY COLUMN alteration")
		case *sqlparser.AddColumns:
			var insertAt = len(meta.Schema.Columns)
			if alter.First {
				insertAt = 0
			} else if after := alter.After; after != nil {
				var afterIndex = slices.Index(meta.Schema.Columns, after.Name.String())
				if afterIndex == -1 {
					return fmt.Errorf("unknown column %q", after.Name.String())
				}
				insertAt = afterIndex + 1
			}

			var newCols []string
			for _, col := range alter.Columns {
				newCols = append(newCols, col.Name.String())
				var dataType = translateDataType(meta, col.Type)
				meta.Schema.ColumnTypes[col.Name.String()] = dataType
			}

			meta.Schema.Columns = slices.Insert(meta.Schema.Columns, insertAt, newCols...)
			logrus.WithField("columns", meta.Schema.Columns).WithField("types", meta.Schema.ColumnTypes).Info("processed CHANGE COLUMN alteration")
		case *sqlparser.DropColumn:
			var colName = alter.Name.Name.String()
			var oldIndex = slices.Index(meta.Schema.Columns, colName)
			if oldIndex == -1 {
				return fmt.Errorf("unknown column %q", colName)
			}
			meta.Schema.Columns = slices.Delete(meta.Schema.Columns, oldIndex, oldIndex+1)
			meta.Schema.ColumnTypes[colName] = nil // Set to nil rather than delete so that JSON patch merging deletes it
			logrus.WithField("columns", meta.Schema.Columns).WithField("types", meta.Schema.ColumnTypes).Info("processed CHANGE COLUMN alteration")
		default:
			logrus.WithField("query", query).Info("ignorable table alteration")
		}
	}

	var bs, err = json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("error serializing metadata JSON for %q: %w", streamID, err)
	}
	if err := rs.emitEvent(ctx, &sqlcapture.MetadataEvent{
		StreamID: streamID,
		Metadata: json.RawMessage(bs),
	}); err != nil {
		return err
	}

	return nil
}

func translateDataType(meta *mysqlTableMetadata, t sqlparser.ColumnType) any {
	switch typeName := strings.ToLower(t.Type); typeName {
	case "enum":
		return &mysqlColumnType{Type: typeName, EnumValues: append([]string{""}, unquoteEnumValues(t.EnumValues)...)}
	case "set":
		return &mysqlColumnType{Type: typeName, EnumValues: unquoteEnumValues(t.EnumValues)}
	case "tinyint", "smallint", "mediumint", "int", "bigint":
		return &mysqlColumnType{Type: typeName, Unsigned: t.Unsigned}
	case "char", "varchar", "tinytext", "text", "mediumtext", "longtext":
		var charset string
		if t.Charset.Name != "" {
			charset = t.Charset.Name // If explicitly specified, the declared charset wins
		} else if t.Options.Collate != "" {
			charset = charsetFromCollation(t.Options.Collate) // If only a collation is declared, figure out what charset that implies
		} else if meta.DefaultCharset != "" {
			charset = meta.DefaultCharset // In the absence of a column-specific declaration, use the default table charset
		} else {
			charset = mysqlDefaultCharset // Finally fall back to UTF-8 if nothing else supersedes that
		}
		return &mysqlColumnType{Type: typeName, Charset: charset}
	case "binary":
		var columnLength int
		if t.Length == nil {
			columnLength = 1 // A type of just 'BINARY' is allowed and is a synonym for 'BINARY(1)'
		} else if n, err := strconv.Atoi(t.Length.Val); err == nil {
			columnLength = n // Otherwise if a length is specified we should be able to parse that as an integer and use that
		}
		return &mysqlColumnType{Type: typeName, MaxLength: columnLength}
	default:
		return typeName
	}
}

// unquoteEnumValues applies MySQL single-quote-unescaping to a list of single-quoted
// escaped string values such as the EnumValues list returned by the Vitess SQL Parser
// package when parsing a DDL query involving enum/set values.
//
// The single-quote wrapping and escaping of these strings is clearly deliberate, as
// under the hood the package actually tokenizes the strings to raw values and then
// explicitly calls `encodeSQLString()` to re-wrap them when building the AST. The
// actual reason for doing this is unknown however, and it makes very little sense.
//
// So whatever, here's a helper function to undo that escaping and get back down to
// the raw strings again.
func unquoteEnumValues(values []string) []string {
	var unquoted []string
	for _, qval := range values {
		unquoted = append(unquoted, unquoteMySQLString(qval))
	}
	return unquoted
}

func resolveTableName(defaultSchema string, name sqlparser.TableName) string {
	var schema, table = name.Qualifier.String(), name.Name.String()
	if schema == "" {
		schema = defaultSchema
	}
	return sqlcapture.JoinStreamID(schema, table)
}

func (rs *mysqlReplicationStream) tableMetadata(streamID string) (*mysqlTableMetadata, bool) {
	rs.tables.RLock()
	defer rs.tables.RUnlock()
	var meta, ok = rs.tables.metadata[streamID]
	return meta, ok
}

func (rs *mysqlReplicationStream) tableActive(streamID string) bool {
	rs.tables.RLock()
	defer rs.tables.RUnlock()
	var _, ok = rs.tables.active[streamID]
	return ok
}

func (rs *mysqlReplicationStream) schemaActive(schema string) bool {
	rs.tables.RLock()
	defer rs.tables.RUnlock()
	for streamID := range rs.tables.active {
		var schemaName, _ = splitStreamID(streamID)
		if strings.EqualFold(schemaName, schema) {
			return true
		}
	}
	return false
}

// isNonTransactional returns true if the stream ID refers to a table which
// uses a storage engine such as MyISAM which doesn't support transactions.
// Changes to non-transactional tables are never followed by a commit event,
// so we have to take care of that for ourselves.
func (rs *mysqlReplicationStream) isNonTransactional(streamID string) bool {
	rs.tables.RLock()
	defer rs.tables.RUnlock()
	return rs.tables.nonTransactional[streamID]
}

// splitStreamID is the inverse of JoinStreamID and splits a stream name back into
// separate schema and table name components. Its use is generally kind of a hack
// and suggests that the surrounding plumbing is probably subtly wrong, because the
// normalization from schema+table to stream IDs is supposed to go one way only.
func splitStreamID(streamID string) (string, string) {
	var bits = strings.SplitN(streamID, ".", 2)
	return bits[0], bits[1]
}

func (rs *mysqlReplicationStream) keyColumns(streamID string) ([]string, bool) {
	rs.tables.RLock()
	defer rs.tables.RUnlock()
	var keyColumns, ok = rs.tables.keyColumns[streamID]
	return keyColumns, ok
}

func (rs *mysqlReplicationStream) ActivateTable(ctx context.Context, streamID string, keyColumns []string, discovery *sqlcapture.DiscoveryInfo, metadataJSON json.RawMessage) error {
	rs.tables.Lock()
	defer rs.tables.Unlock()

	// Do nothing if the table is already active.
	if _, ok := rs.tables.active[streamID]; ok {
		return nil
	}

	// Extract some details from the provided discovery info, if present.
	var nonTransactional bool
	if discovery != nil {
		if extraDetails, ok := discovery.ExtraDetails.(*mysqlTableDiscoveryDetails); ok {
			nonTransactional = extraDetails.StorageEngine == "MyISAM"
		}
	}

	// If metadata JSON is present then parse it into a usable object. Otherwise
	// initialize new metadata based on discovery results.
	var metadata *mysqlTableMetadata
	if metadataJSON != nil {
		if err := json.Unmarshal(metadataJSON, &metadata); err != nil {
			return fmt.Errorf("error parsing metadata JSON: %w", err)
		}
		// Fix up complex (non-string) column types, since the JSON round-trip
		// will turn *mysqlColumnType values into map[string]interface{}.
		for column, columnType := range metadata.Schema.ColumnTypes {
			if columnType, ok := columnType.(map[string]interface{}); ok {
				var parsedType mysqlColumnType
				if err := mapstructure.Decode(columnType, &parsedType); err == nil {
					metadata.Schema.ColumnTypes[column] = &parsedType
				}
			}
		}

		// This is a temporary piece of migration logic added in March of 2024. In the PR
		// https://github.com/estuary/connectors/pull/1336 the ordering of enum cases in this
		// metadata was changed to simplify the decoding logic. Specifically the order of the
		// cases was changed from ["A", "B", ""] to ["", "A", "B"] to more directly mirror how
		// MySQL represents the illegal-enum value "" as integer 0. However this introduced a bug
		// when the new indexing code was used with older metadata from before that change, and
		// by the time this was discovered there were captures in production with the new ordering
		// in their checkpointed metadata, so a simple revert was not an option.
		//
		// The solution is to push forward and add the missing migration step, so that upon table
		// activation any metadata containing the old ordering will be updated to the new. Once all
		// metadata in production has been so updated, it will be safe to remove this logic and the
		// associated 'TestEnumDecodingFix' test case.
		for column, columnType := range metadata.Schema.ColumnTypes {
			if enumType, ok := columnType.(*mysqlColumnType); ok && enumType.Type == "enum" {
				if len(enumType.EnumValues) > 0 && enumType.EnumValues[0] != "" {
					var logEntry = logrus.WithField("column", column)
					var vals = enumType.EnumValues
					logEntry.WithField("vals", vals).Warn("old enum metadata ordering detected, will migrate")
					if vals[len(vals)-1] == "" {
						logEntry.Info("trimming empty-string case from the end")
						vals = vals[:len(vals)-1]
					}
					vals = append([]string{""}, vals...)
					logEntry.WithField("vals", vals).Info("migrated old enum metadata")
					enumType.EnumValues = vals
				}
			}
		}
	} else if discovery != nil {
		// If metadata JSON is not present, construct new default metadata based on the discovery info.
		logrus.WithField("stream", streamID).Debug("initializing table metadata")
		metadata = new(mysqlTableMetadata)
		var colTypes = make(map[string]interface{})
		for colName, colInfo := range discovery.Columns {
			colTypes[colName] = colInfo.DataType
		}

		metadata.Schema.Columns = discovery.ColumnNames
		metadata.Schema.ColumnTypes = colTypes
		if extraDetails, ok := discovery.ExtraDetails.(*mysqlTableDiscoveryDetails); ok {
			metadata.DefaultCharset = extraDetails.DefaultCharset
		}

		logrus.WithFields(logrus.Fields{
			"stream":  streamID,
			"columns": metadata.Schema.Columns,
			"types":   metadata.Schema.ColumnTypes,
			"charset": metadata.DefaultCharset,
		}).Debug("initialized table metadata")
	}

	// Finally, mark the table as active and store the updated metadata.
	rs.tables.active[streamID] = struct{}{}
	rs.tables.keyColumns[streamID] = keyColumns
	rs.tables.metadata[streamID] = metadata
	rs.tables.nonTransactional[streamID] = nonTransactional
	rs.tables.dirtyMetadata = append(rs.tables.dirtyMetadata, streamID)
	return nil
}

func (rs *mysqlReplicationStream) deactivateTable(streamID string) error {
	rs.tables.Lock()
	defer rs.tables.Unlock()

	delete(rs.tables.active, streamID)
	delete(rs.tables.keyColumns, streamID)
	delete(rs.tables.metadata, streamID) // No need to mark metadata as dirty, just forget it
	delete(rs.tables.nonTransactional, streamID)
	return nil
}

func (rs *mysqlReplicationStream) emitEvent(ctx context.Context, event sqlcapture.DatabaseEvent) error {
	select {
	case rs.events <- event:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (rs *mysqlReplicationStream) StreamToFence(ctx context.Context, fenceAfter time.Duration, callback func(event sqlcapture.DatabaseEvent) error) error {
	// Report "Metadata Change" events when necessary.
	rs.tables.RLock()
	if len(rs.tables.dirtyMetadata) > 0 {
		for _, streamID := range rs.tables.dirtyMetadata {
			var bs, err = json.Marshal(rs.tables.metadata[streamID])
			if err != nil {
				return fmt.Errorf("error serializing metadata JSON for %q: %w", streamID, err)
			}
			if err := callback(&sqlcapture.MetadataEvent{
				StreamID: streamID,
				Metadata: json.RawMessage(bs),
			}); err != nil {
				return err
			}
		}
		rs.tables.dirtyMetadata = nil
	}
	rs.tables.RUnlock()

	// Time-based event streaming until the fenceAfter duration is reached.
	var timedEventsSinceFlush int
	var latestFlushCursor = fmt.Sprintf("%s:%d", rs.fencePosition.Name, rs.fencePosition.Pos)
	logrus.WithField("cursor", latestFlushCursor).Debug("beginning timed streaming phase")
	if fenceAfter > 0 {
		var deadline = time.NewTimer(fenceAfter)
		defer deadline.Stop()
	loop:
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-deadline.C:
				break loop
			case event, ok := <-rs.events:
				if !ok {
					return sqlcapture.ErrFenceNotReached
				} else if err := callback(event); err != nil {
					return err
				}
				timedEventsSinceFlush++
				if event, ok := event.(*sqlcapture.FlushEvent); ok {
					latestFlushCursor = event.Cursor
					timedEventsSinceFlush = 0
				}
			}
		}
	}
	logrus.WithField("cursor", latestFlushCursor).Debug("finished timed streaming phase")

	// Establish a binlog-position fence.
	var fencePosition, err = rs.db.queryBinlogPosition()
	if err != nil {
		return fmt.Errorf("error establishing binlog fence position: %w", err)
	}
	logrus.WithField("cursor", latestFlushCursor).WithField("target", fencePosition.Pos).Debug("beginning fenced streaming phase")
	if latestFlushPosition, err := parseCursor(latestFlushCursor); err != nil {
		return fmt.Errorf("internal error: failed to parse flush cursor value %q", latestFlushCursor)
	} else if fencePosition == latestFlushPosition {
		// As an internal sanity check, we assert that it should never be possible
		// to hit this early exit unless the database has been idle since the last
		// flush event we observed.
		if timedEventsSinceFlush > 0 {
			return fmt.Errorf("internal error: sanity check failed: already at fence after processing %d changes during timed phase", timedEventsSinceFlush)
		}

		// Mark the position of the flush event as the latest fence before returning.
		rs.fencePosition = latestFlushPosition

		// Since we're still at a valid flush position and those are always between
		// transactions, we can safely emit a synthetic FlushEvent here. This means
		// that every StreamToFence operation ends in a flush, and is helpful since
		// there's a lot of implicit assumptions of regular events / flushes.
		return callback(&sqlcapture.FlushEvent{Cursor: latestFlushCursor})
	}

	// Given that the early-exit fast path was not taken, there must be further data for
	// us to read. Thus if we sit idle for a nontrivial length of time without reaching
	// our fence position, something is wrong and we should error out instead of blocking
	// forever.
	var fenceWatchdog = time.NewTimer(streamToFenceWatchdogTimeout)

	// Stream replication events until the fence is reached.
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-fenceWatchdog.C:
			var fenceCursor = fmt.Sprintf("%s:%d", fencePosition.Name, fencePosition.Pos)
			return fmt.Errorf("replication became idle while streaming from %q to an established fence at %q", latestFlushCursor, fenceCursor)
		case event, ok := <-rs.events:
			fenceWatchdog.Reset(streamToFenceWatchdogTimeout)
			if !ok {
				return sqlcapture.ErrFenceNotReached
			} else if err := callback(event); err != nil {
				return err
			}

			// The first flush event whose cursor position is equal to or after the fence
			// position ends the stream-to-fence operation.
			if event, ok := event.(*sqlcapture.FlushEvent); ok {
				// It might be a bit inefficient to re-parse every flush cursor here, but
				// realistically it's probably not a significant slowdown and it would be
				// a bit of work to preserve the position as a typed struct.
				var eventPosition, err = parseCursor(event.Cursor)
				if err != nil {
					return fmt.Errorf("internal error: failed to parse flush event cursor value %q", event.Cursor)
				}
				if eventPosition.Compare(fencePosition) >= 0 {
					logrus.WithField("cursor", event.Cursor).Debug("finished fenced streaming phase")
					rs.fencePosition = eventPosition
					return nil
				}
			}
		}
	}
}

func (rs *mysqlReplicationStream) Acknowledge(ctx context.Context, cursor string) error {
	// No acknowledgements are necessary or possible in MySQL. The binlog is just
	// a series of logfiles on disk which get erased after log rotation according
	// to a time-based retention policy, without any server-side "have all clients
	// consumed these events" tracking.
	//
	// See also: The 'Binlog Retention Sanity Check' logic in source-mysql/main.go
	return nil
}

func (rs *mysqlReplicationStream) Close(ctx context.Context) error {
	rs.cancel()
	return <-rs.errCh
}

func (db *mysqlDatabase) ReplicationDiagnostics(ctx context.Context) error {
	var query = func(q string) {
		logrus.WithField("query", q).Info("running diagnostics query")
		var result, err = db.conn.Execute(q)
		if err != nil {
			logrus.WithFields(logrus.Fields{
				"query": q,
				"err":   err,
			}).Error("unable to execute diagnostics query")
			return
		}
		defer result.Close()

		if len(result.Values) == 0 {
			logrus.WithField("query", q).Info("no results")
		}
		for _, row := range result.Values {
			var logFields = logrus.Fields{}
			for idx, column := range row {
				var key = string(result.Fields[idx].Name)
				var val = column.Value()
				if bs, ok := val.([]byte); ok {
					val = string(bs)
				}
				logFields[key] = val
			}
			logrus.WithFields(logFields).Info("got diagnostic row")
		}
	}

	query("SELECT @@GLOBAL.log_bin;")
	query("SELECT @@GLOBAL.binlog_format;")
	query("SHOW PROCESSLIST;")
	query("SHOW BINARY LOGS;")
	var newspeakQueries = db.versionProduct == "MySQL" && ((db.versionMajor == 8 && db.versionMinor >= 4) || db.versionMajor > 8)
	if newspeakQueries {
		query("SHOW BINARY LOG STATUS;")
		query("SHOW REPLICAS;")
	} else {
		query("SHOW MASTER STATUS;")
		query("SHOW SLAVE HOSTS;")
	}
	return nil
}
