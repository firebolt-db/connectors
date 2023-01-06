package main

import (
	"context"
	stdsql "database/sql"
	"encoding/json"
	"testing"

	"github.com/bradleyjkemp/cupaloy"
	sql "github.com/estuary/connectors/materialize-sql"
	"github.com/estuary/connectors/testsupport"
	"github.com/estuary/flow/go/protocols/catalog"
	pf "github.com/estuary/flow/go/protocols/flow"
	pm "github.com/estuary/flow/go/protocols/materialize"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestQueryGeneration(t *testing.T) {
	var spec *pf.MaterializationSpec
	require.NoError(t, testsupport.CatalogExtract(t, "testdata/flow.yaml",
		func(db *stdsql.DB) error {
			var err error
			spec, err = catalog.LoadMaterialization(db, "test/sqlite")
			return err
		}))

	var shape1 = sql.BuildTableShape(spec, 0, tableConfig{
		Table: "testTable",
		Delta: false,
	})
	table, err := sql.ResolveTable(shape1, snowflakeDialect)
	require.NoError(t, err)

	var loadUUID = uuid.UUID{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}
	var storeUUID = uuid.UUID{15, 14, 13, 12, 11, 10, 9, 8, 7, 6, 5, 4, 3, 2, 1}

	loadQuery, err := RenderTableWithRandomUUIDTemplate(table, loadUUID, tplLoadQuery)
	require.NoError(t, err)
	copyInto, err := RenderTableWithRandomUUIDTemplate(table, storeUUID, tplCopyInto)
	require.NoError(t, err)
	mergeInto, err := RenderTableWithRandomUUIDTemplate(table, storeUUID, tplMergeInto)
	require.NoError(t, err)

	// Note the intentional missing semicolon, as this is a subquery.
	require.Equal(t, `
	SELECT 0, testTable.flow_document
	FROM testTable
	JOIN (
		SELECT $1[0] AS key1, $1[1] AS key2
		FROM @flow_v1/00010203-0405-0607-0809-0a0b0c0d0e0f
	) AS r
	ON testTable.key1 = r.key1 AND testTable.key2 = r.key2`,
		loadQuery)

	require.Equal(t, `
	COPY INTO testTable (
		key1, key2, boolean, integer, number, string, flow_document
	) FROM (
		SELECT $1[0] AS key1, $1[1] AS key2, $1[2] AS boolean, $1[3] AS integer, $1[4] AS number, $1[5] AS string, $1[6] AS flow_document
		FROM @flow_v1/0f0e0d0c-0b0a-0908-0706-050403020100
	);`,
		copyInto)

	require.Equal(t, `
	MERGE INTO testTable
	USING (
		SELECT $1[0] AS key1, $1[1] AS key2, $1[2] AS boolean, $1[3] AS integer, $1[4] AS number, $1[5] AS string, $1[6] AS flow_document
		FROM @flow_v1/0f0e0d0c-0b0a-0908-0706-050403020100
	) AS r
	ON testTable.key1 = r.key1 AND testTable.key2 = r.key2
	WHEN MATCHED AND IS_NULL_VALUE(r.flow_document) THEN
		DELETE
	WHEN MATCHED THEN
		UPDATE SET testTable.boolean = r.boolean, testTable.integer = r.integer, testTable.number = r.number, testTable.string = r.string, testTable.flow_document = r.flow_document
	WHEN NOT MATCHED THEN
		INSERT (key1, key2, boolean, integer, number, string, flow_document)
		VALUES (r.key1, r.key2, r.boolean, r.integer, r.number, r.string, r.flow_document);`,
		mergeInto)
}

func TestSpecification(t *testing.T) {
	var resp, err = newSnowflakeDriver().
		Spec(context.Background(), &pm.SpecRequest{EndpointType: pf.EndpointType_AIRBYTE_SOURCE})
	require.NoError(t, err)

	formatted, err := json.MarshalIndent(resp, "", "  ")
	require.NoError(t, err)

	cupaloy.SnapshotT(t, formatted)
}
