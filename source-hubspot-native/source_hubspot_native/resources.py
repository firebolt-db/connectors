import functools
import itertools
from datetime import UTC, datetime, timedelta
from logging import Logger
from typing import AsyncGenerator, Iterable

from estuary_cdk.capture import Task
from estuary_cdk.capture.common import LogCursor, PageCursor, Resource, open_binding
from estuary_cdk.flow import CaptureBinding
from estuary_cdk.http import HTTPMixin, HTTPSession, TokenSource

from .api import (
    FetchDelayedFn,
    FetchRecentFn,
    fetch_delayed_companies,
    fetch_delayed_contacts,
    fetch_delayed_custom_objects,
    fetch_delayed_deals,
    fetch_delayed_email_events,
    fetch_delayed_engagements,
    fetch_delayed_tickets,
    fetch_email_events_page,
    fetch_page_with_associations,
    fetch_properties,
    fetch_recent_companies,
    fetch_recent_contacts,
    fetch_recent_custom_objects,
    fetch_recent_deals,
    fetch_recent_email_events,
    fetch_recent_engagements,
    fetch_recent_tickets,
    list_custom_objects,
    process_changes,
)
from .models import (
    OAUTH2_SPEC,
    Company,
    Contact,
    CRMObject,
    CustomObject,
    Deal,
    EmailEvent,
    EndpointConfig,
    Engagement,
    Names,
    Property,
    ResourceConfig,
    ResourceState,
    Ticket,
)


async def all_resources(
    log: Logger, http: HTTPMixin, config: EndpointConfig
) -> list[Resource]:
    http.token_source = TokenSource(oauth_spec=OAUTH2_SPEC, credentials=config.credentials)

    standard_object_names: list[str] = [
        Names.companies,
        Names.contacts,
        Names.deals,
        Names.engagements,
        Names.tickets,
    ]

    custom_object_names = await list_custom_objects(log, http)
    # Some HubSpot endpoints like /v3/properties/{objectType} do not work for every custom object type.
    # However, these endpoints do work if we prepend a "p_" to the beginning of the custom object name
    # and use that in the path instead. 
    # Docs reference: https://developers.hubspot.com/docs/api/crm/crm-custom-objects#retrieve-existing-custom-objects
    custom_object_path_components = [f"p_{n}" for n in custom_object_names]

    custom_object_resources = [
        crm_object_with_associations(
            CustomObject,
            n,
            custom_object_path_components[index],
            http,
            functools.partial(fetch_recent_custom_objects, custom_object_path_components[index]),
            functools.partial(fetch_delayed_custom_objects, custom_object_path_components[index]),
        )
        for index, n in enumerate(custom_object_names)
    ]

    return [
        crm_object_with_associations(Company, Names.companies, Names.companies, http, fetch_recent_companies, fetch_delayed_companies),
        crm_object_with_associations(Contact, Names.contacts, Names.contacts, http, fetch_recent_contacts, fetch_delayed_contacts),
        crm_object_with_associations(Deal, Names.deals, Names.deals, http, fetch_recent_deals, fetch_delayed_deals),
        crm_object_with_associations(Engagement, Names.engagements, Names.engagements, http, fetch_recent_engagements, fetch_delayed_engagements),
        crm_object_with_associations(Ticket, Names.tickets, Names.tickets, http, fetch_recent_tickets, fetch_delayed_tickets),
        properties(http, itertools.chain(standard_object_names, custom_object_path_components)),
        email_events(http),
        *custom_object_resources,
    ]

def crm_object_with_associations(
    cls: type[CRMObject],
    object_name: str,
    path_component: str,
    http: HTTPSession,
    fetch_recent: FetchRecentFn,
    fetch_delayed: FetchDelayedFn,
) -> Resource:

    def open(
        binding: CaptureBinding[ResourceConfig],
        binding_index: int,
        state: ResourceState,
        task: Task,
        all_bindings
    ):
        open_binding(
            binding,
            binding_index,
            state,
            task,
            fetch_changes=functools.partial(
                process_changes,
                path_component,
                fetch_recent,
                fetch_delayed,
                http,
            ),
            fetch_page=functools.partial(fetch_page_with_associations, cls, http, path_component),
        )

    started_at = datetime.now(tz=UTC)

    return Resource(
        name=object_name,
        key=["/id"],
        model=cls,
        open=open,
        initial_state=ResourceState(
            inc=ResourceState.Incremental(cursor=started_at),
            backfill=ResourceState.Backfill(next_page=None, cutoff=started_at),
        ),
        initial_config=ResourceConfig(name=object_name),
        schema_inference=True,
    )


def properties(http: HTTPSession, object_names: Iterable[str]) -> Resource:

    async def snapshot(log: Logger) -> AsyncGenerator[Property, None]:
        for obj in object_names:
            properties = await fetch_properties(log, http, obj)
            for prop in properties.results:
                yield prop

    def open(
        binding: CaptureBinding[ResourceConfig],
        binding_index: int,
        state: ResourceState,
        task: Task,
        all_bindings
    ):
        open_binding(
            binding,
            binding_index,
            state,
            task,
            fetch_snapshot=snapshot,
            tombstone=Property(_meta=Property.Meta(op="d")),
        )

    return Resource(
        name=Names.properties,
        key=["/_meta/row_id"],
        model=Property,
        open=open,
        initial_state=ResourceState(),
        initial_config=ResourceConfig(
            name=Names.properties, interval=timedelta(days=1)
        ),
        schema_inference=True,
    )

def email_events(http: HTTPSession) -> Resource:
    def open(
        binding: CaptureBinding[ResourceConfig],
        binding_index: int,
        state: ResourceState,
        task: Task,
        all_bindings
    ):
        open_binding(
            binding,
            binding_index,
            state,
            task,
            fetch_changes=functools.partial(
                process_changes,
                Names.email_events,
                fetch_recent_email_events,
                fetch_delayed_email_events,
                http,
            ),
            fetch_page=functools.partial(fetch_email_events_page, http),
        )

    started_at = datetime.now(tz=UTC)

    return Resource(
        name=Names.email_events,
        key=["/id"],
        model=EmailEvent,
        open=open,
        initial_state=ResourceState(
            inc=ResourceState.Incremental(cursor=started_at),
            backfill=ResourceState.Backfill(next_page=None, cutoff=started_at),
        ),
        initial_config=ResourceConfig(name=Names.email_events),
        schema_inference=True,
    )
