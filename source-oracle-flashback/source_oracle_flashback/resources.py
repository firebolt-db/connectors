from datetime import datetime, UTC, timedelta
from typing import AsyncGenerator, Awaitable, Iterable, Dict
from logging import Logger
import functools
import oracledb
import asyncio

from estuary_cdk.flow import CaptureBinding
from estuary_cdk.capture import Task
from estuary_cdk.capture import common
from estuary_cdk.http import HTTPSession, HTTPMixin, TokenSource


from .models import (
    EndpointConfig,
    build_table,
    Table,
    ResourceState,
    ResourceConfig,
)
from .api import (
    fetch_tables,
    fetch_columns,
    fetch_page,
    fetch_changes,
)

one_week_seconds = 60 * 60 * 24 * 7


async def validate_flashback(
    log: Logger, config: EndpointConfig, pool: oracledb.AsyncConnectionPool,
):
    skip_retention_checks = config.advanced.skip_flashback_retention_checks
    async with pool.acquire() as conn:
        with conn.cursor() as c:
            await c.execute("SELECT flashback_on FROM V$DATABASE")
            flashback_on = (await c.fetchone())[0] == "YES"

            await c.execute("SELECT name, value FROM V$PARAMETER WHERE NAME IN ('undo_tablespace', 'undo_management', 'undo_retention')")
            params = dict(await c.fetchall())
            undo_tablespace = params['undo_tablespace']
            undo_management = params['undo_management']
            undo_retention_seconds = int(params['undo_retention'])

            await c.execute("SELECT max_size, retention FROM dba_tablespaces WHERE tablespace_name=:tablespace", tablespace=undo_tablespace)
            max_size, retention_mode = await c.fetchone()

            await c.execute("SELECT autoextensible FROM dba_data_files WHERE tablespace_name=:tablespace", tablespace=undo_tablespace)
            autoextensible = (await c.fetchone())[0] == "YES"

            avg_retention = None
            if params['undo_management'] == 'AUTO':
                await c.execute("SELECT AVG(tuned_undoretention) from v$undostat")
                avg_retention_seconds = int((await c.fetchone())[0])

            log.info("flashback configuration", {
                "undo_tablespace": undo_tablespace,
                "undo_management": undo_management,
                "undo_retention_seconds": undo_retention_seconds,
                "max_size": max_size,
                "retention_mode": retention_mode,
                "autoextensible": autoextensible,
                "flashback_on": flashback_on,
                "avg_retention_seconds": avg_retention_seconds,
            })

            if not flashback_on:
                msg = "Flashback must be enabled on the database. See go.estuary.dev/source-oracle-flashback for more information."
                if skip_retention_checks:
                    log.warn(msg)
                else:
                    raise Exception(msg)

            if undo_retention_seconds < one_week_seconds:
                msg = f"We require a minimum of 7 days UNDO_RETENTION to ensure consistency of this task. The current UNDO_RETENTION is {undo_retention_seconds} seconds. See go.estuary.dev/source-oracle for more information on how to configure the UNDO_RETENTION."  # nopep8
                if skip_retention_checks:
                    log.warn(msg)
                else:
                    raise Exception(msg)

            if avg_retention_seconds < one_week_seconds:
                msg = f"We require a minimum of 7 days undo retention to ensure consistency of this task. The current average auto-tuned retention of the database for the past four days is {avg_retention_seconds} seconds. See go.estuary.dev/source-oracle for more information on how to configure the UNDO_RETENTION."  # nopep8
                if skip_retention_checks:
                    log.warn(msg)
                else:
                    raise Exception(msg)

            if not autoextensible:
                log.warn("We recommend making your undo tablespace auto-extensible. See go.estuary.dev/source-oracle for more information.")

            if retention_mode != 'GUARANTEE':
                log.warn("We recommend guaranteeing retention of the undo tablespace. See go.estuary.dev/source-oracle for more information.")


async def all_resources(
    log: Logger, http: HTTPMixin, config: EndpointConfig, pool: oracledb.AsyncConnectionPool,
) -> list[common.Resource]:
    resources_list = []

    oracle_tables = await fetch_tables(log, pool)
    owners = set([t.owner for t in oracle_tables])
    oracle_columns = await fetch_columns(log, pool, owners)

    current_scn = None
    async with pool.acquire() as conn:
        with conn.cursor() as c:
            await c.execute("SELECT current_scn FROM V$DATABASE")
            current_scn = (await c.fetchone())[0]

    log.debug("current scn", current_scn)

    for ot in oracle_tables:
        columns = [col for col in oracle_columns if col.table_name == ot.table_name and col.owner == ot.owner]
        t = build_table(config, ot.owner, ot.table_name, columns)

        max_rowid = None
        async with pool.acquire() as conn:
            with conn.cursor() as c:
                await c.execute(f"SELECT max(ROWID) FROM {t.quoted_owner}.{t.quoted_table_name}")
                max_rowid = (await c.fetchone())[0]
        # if max_rowid is None, that maens there are no rows in the table, so we
        # skip backfill
        backfill = ResourceState.Backfill(cutoff=(max_rowid,)) if max_rowid is not None else None

        def open(
            table: Table,
            binding: CaptureBinding[ResourceConfig],
            binding_index: int,
            state: ResourceState,
            task: Task,
            _all_bindings,
        ):
            sync_lock = asyncio.Lock()
            common.open_binding(
                binding,
                binding_index,
                state,
                task,
                fetch_page=functools.partial(fetch_page, table, pool, task, config.advanced.backfill_chunk_size, sync_lock),
                fetch_changes=functools.partial(fetch_changes, table, pool, sync_lock),
            )

        keys = [f"/{c.column_name}" for c in t.primary_key]
        if len(keys) < 1:
            keys = ["/_meta/source/row_id"]

        resources_list.append(common.Resource(
            name=f"{t.owner}_{t.table_name}",
            key=keys,
            model=t.create_model(),
            open=functools.partial(open, t),
            initial_state=ResourceState(
                backfill=backfill,
                inc=ResourceState.Incremental(cursor=current_scn),
            ),
            initial_config=ResourceConfig(
                schema=t.owner,
                name=t.table_name,
                interval=config.advanced.default_interval,
            ),
            schema_inference=False,
        ))

    return resources_list