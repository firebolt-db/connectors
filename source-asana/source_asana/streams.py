#
# Copyright (c) 2023 Airbyte, Inc., all rights reserved.
#


from abc import ABC
from itertools import islice
from typing import Any, Iterable, Mapping, MutableMapping, Optional, Type, Union
from functools import cache

import requests
from airbyte_cdk.models import SyncMode
from airbyte_cdk.sources.streams.http import HttpStream
from airbyte_cdk.sources.streams.http.auth.core import HttpAuthenticator
from requests.auth import AuthBase
from http import HTTPStatus

ASANA_ERRORS_MAPPING = {
    402: "This stream is available to premium organizations and workspaces only",
    403: "Missing permissions to consume this stream enough permissions",
    404: "The object specified by the request does not exist",
    451: "This request was blocked for legal reasons",
}


class AsanaStream(HttpStream, ABC):
    url_base = "https://app.asana.com/api/1.0/"
    primary_key = "gid"
    # Asana pagination could be from 1 to 100.
    page_size = 100
    raise_on_http_errors = True

    @property
    def AsanaStreamType(self) -> Type:
        return self.__class__

    def __init__(
        self,
        authenticator: Union[AuthBase, HttpAuthenticator] = None,
        test_mode: bool = False,
    ):
        super().__init__(authenticator=authenticator)
        self.test_mode = test_mode

    def should_retry(self, response: requests.Response) -> bool:
        if response.status_code in ASANA_ERRORS_MAPPING.keys():
            self.logger.error(
                f"Skipping stream {self.name}. {ASANA_ERRORS_MAPPING.get(response.status_code)}. Full error message: {response.text}"
            )
            setattr(self, "raise_on_http_errors", False)
            return False
        return super().should_retry(response)

    def backoff_time(self, response: requests.Response) -> Optional[int]:
        delay_time = response.headers.get("Retry-After")
        if delay_time:
            return int(delay_time)

    def next_page_token(
        self, response: requests.Response
    ) -> Optional[Mapping[str, Any]]:
        decoded_response = response.json()
        next_page = decoded_response.get("next_page")
        if next_page:
            return {"offset": next_page["offset"]}

    def request_params(
        self, next_page_token: Mapping[str, Any] = None, **kwargs
    ) -> MutableMapping[str, Any]:
        params = {"limit": self.page_size}
        params.update(self.get_opt_fields())
        if next_page_token:
            params.update(next_page_token)
        return params

    def get_opt_fields(self) -> MutableMapping[str, str]:
        """
        For "GET all" request for almost each stream Asana API by default returns 3 fields for each
        record: `gid`, `name`, `resource_type`. Since we want to get all fields we need to specify those fields in each
        request. For each stream set of fields will be different and we get those fields from stream's schema.
        Also each nested object, like `workspace`, or list of nested objects, like `followers`, also by default returns
        those 3 fields mentioned above, so for nested stuff we also need to specify fields we want to return and we
        decided that for all nested objects and list of objects we will be getting only `gid` field.
        Plus each stream can have it's exceptions about how request required fields, like in `Tasks` stream.
        More info can be found here - https://developers.asana.com/docs/input-output-options.
        """
        opt_fields = list()
        schema = self.get_json_schema()

        for prop, value in schema["properties"].items():
            if "object" in value["type"]:
                opt_fields.append(self._handle_object_type(prop, value))
            elif "array" in value["type"]:
                opt_fields.append(self._handle_array_type(prop, value.get("items", [])))
            else:
                opt_fields.append(prop)

        return {"opt_fields": ",".join(opt_fields)} if opt_fields else dict()

    def _handle_object_type(self, prop: str, value: MutableMapping[str, Any]) -> str:
        return f"{prop}.gid"

    def _handle_array_type(self, prop: str, value: MutableMapping[str, Any]) -> str:
        if "type" in value and "object" in value["type"]:
            return self._handle_object_type(prop, value)

        return prop

    def parse_response(
        self, response: requests.Response, **kwargs
    ) -> Iterable[Mapping]:
        response_json = response.json()
        # Asana puts records in a container array "data"
        yield from response_json.get("data", [])

    def read_slices_from_records(
        self, stream_class: AsanaStreamType, slice_field: str
    ) -> Iterable[Optional[Mapping[str, Any]]]:
        """
        General function for getting parent stream (which should be passed through `stream_class`) slice.
        Generates dicts with `gid` of parent streams.
        """
        stream = stream_class(authenticator=self.authenticator)
        stream_slices = stream.stream_slices(sync_mode=SyncMode.full_refresh)

        for stream_slice in stream_slices:
            for record in stream.read_records(
                sync_mode=SyncMode.full_refresh, stream_slice=stream_slice
            ):
                yield {slice_field: record["gid"]}

    @cache  # Caching this method, so we only make this request once
    def get_current_user_data(self) -> dict:
        url = f"{self.url_base}users/me"

        auth_header = self.authenticator.get_auth_header()
        headers = {"Accept": "application/json", **auth_header}

        response = requests.get(url, headers=headers)
        response.raise_for_status()
        data = response.json()

        return data["data"]


class WorkspaceRelatedStream(AsanaStream, ABC):
    """
    Few streams (CustomFields, Projects, Tags, Teams and Users) require passing `workspace` either as request argument
    or as part of a path. The point of this class is to get `workspace_gid`. Child classes then either will insert it
    into the path or will pass it as a request parameter.
    """

    def stream_slices(self, **kwargs) -> Iterable[Optional[Mapping[str, Any]]]:
        workspaces_stream = Workspaces(authenticator=self.authenticator)
        for workspace in workspaces_stream.read_records(
            sync_mode=SyncMode.full_refresh
        ):
            yield {"workspace_gid": workspace["gid"]}


class WorkspaceRequestParamsRelatedStream(WorkspaceRelatedStream, ABC):
    """
    Few streams (Projects, Tags and Users) require passing `workspace` as request argument.
    So this is basically the whole point of this class - to pass `workspace` as request argument.
    """

    def request_params(
        self, stream_slice: Mapping[str, Any] = None, **kwargs
    ) -> MutableMapping[str, Any]:
        params = super().request_params(**kwargs)
        params["workspace"] = stream_slice["workspace_gid"]
        return params


class ProjectRelatedStream(AsanaStream, ABC):
    """
    Few streams (SectionsCompact and Tasks) depends on `project gid`: SectionsCompact as a part of url and Tasks as `projects`
    argument in request.
    """

    def stream_slices(self, **kwargs) -> Iterable[Optional[Mapping[str, Any]]]:
        project_slices = self.read_slices_from_records(
            stream_class=Projects, slice_field="project_gid"
        )

        for s in project_slices:
            self.logger.info(f"Fetching {self.name} for project {s["project_gid"]}.")
            yield s


class AttachmentsCompact(AsanaStream):
    use_cache = True

    def path(self, stream_slice: Mapping[str, Any] = None, **kwargs) -> str:
        return "attachments"

    def request_params(
        self, stream_slice: Mapping[str, Any] = None, **kwargs
    ) -> MutableMapping[str, Any]:
        params = super().request_params(**kwargs)
        params["parent"] = stream_slice["parent_gid"]
        return params

    def stream_slices(self, **kwargs) -> Iterable[Optional[Mapping[str, Any]]]:
        yield from self.read_slices_from_records(
            stream_class=Projects, slice_field="parent_gid"
        )
        yield from self.read_slices_from_records(
            stream_class=Tasks, slice_field="parent_gid"
        )


class Attachments(AsanaStream):
    def path(self, stream_slice: Mapping[str, Any] = None, **kwargs) -> str:
        attachment_gid = stream_slice["attachment_gid"]
        return f"attachments/{attachment_gid}"

    def stream_slices(self, **kwargs) -> Iterable[Optional[Mapping[str, Any]]]:
        yield from self.read_slices_from_records(
            stream_class=AttachmentsCompact, slice_field="attachment_gid"
        )

    def parse_response(
        self, response: requests.Response, **kwargs
    ) -> Iterable[Mapping]:
        response_json = response.json()
        section_data = response_json.get("data", {})
        if isinstance(section_data, dict):  # Check if section_data is a dictionary
            yield section_data
        elif isinstance(section_data, list):  # Check if section_data is a list
            yield from section_data


class CustomFields(WorkspaceRelatedStream):
    def path(self, stream_slice: Mapping[str, Any] = None, **kwargs) -> str:
        workspace_gid = stream_slice["workspace_gid"]
        return f"workspaces/{workspace_gid}/custom_fields"


class Events(AsanaStream):
    primary_key = "created_at"
    sync_token: Optional[str] = None
    has_more: bool = False
    raise_on_http_errors = False

    def path(self, stream_slice: Mapping[str, Any] = None, **kwargs) -> str:
        return "events"

    def parse_response(
        self, response: requests.Response, **kwargs
    ) -> Iterable[Mapping]:
        payload: dict = response.json()
        data = payload.get("data", [])

        if (  # Check if response is a 412 error
            response.status_code == HTTPStatus.PRECONDITION_FAILED
            and not self.sync_token
        ):
            self.logger.warning(
                "Sync token expired. Fetch the full dataset for this query now."
            )

        self.sync_token = payload.get("sync")

        return data

    def next_page_token(
        self, response: requests.Response
    ) -> Optional[Mapping[str, Any]]:
        payload = response.json()

        has_more = bool(payload.get("has_more"))
        # self.sync_token = payload.get("sync")

        if not has_more:
            self.logger.info("Nothing to read.")
            return None
        return {"sync": self.sync_token}

    def request_params(
        self, stream_slice: Mapping[str, Any] = None, **kwargs
    ) -> MutableMapping[str, Any]:
        params = super().request_params(**kwargs)
        params["resource"] = stream_slice["resource_gid"]
        params["sync"] = self.sync_token
        return params

    def stream_slices(self, **kwargs) -> Iterable[Optional[Mapping[str, Any]]]:
        yield from self.read_slices_from_records(
            stream_class=Projects, slice_field="resource_gid"
        )
        yield from self.read_slices_from_records(
            stream_class=Tasks, slice_field="resource_gid"
        )


class OrganizationExports(AsanaStream):
    def __init__(self, organization_export_ids: str, **kwargs):
        super().__init__(**kwargs)
        self._organization_export_ids = organization_export_ids

    def path(self, stream_slice: Mapping[str, Any] = None, **kwargs) -> str:
        organization_export_gid = stream_slice["organization_export_gid"]
        return f"organization_exports/{organization_export_gid}"

    def stream_slices(self, **kwargs) -> Iterable[Optional[Mapping[str, Any]]]:
        yield from [
            {
                "organization_export_gid": organization_export_id
                for organization_export_id in self._organization_export_ids
            }
        ]


class Projects(WorkspaceRequestParamsRelatedStream):
    use_cache = True

    def path(self, **kwargs) -> str:
        return "projects"


class PortfoliosCompact(WorkspaceRequestParamsRelatedStream):
    def path(self, **kwargs) -> str:
        return "portfolios"

    def request_params(
        self, stream_slice: Optional[dict] = None, **kwargs
    ) -> MutableMapping[str, Any]:
        params = super().request_params(stream_slice=stream_slice, **kwargs)

        user_data = self.get_current_user_data()
        params["owner"] = user_data.get("gid")

        return params

    def parse_response(
        self, response: requests.Response, **kwargs
    ) -> Iterable[Mapping]:
        payload = response.json()

        # Handle case of non-paid users
        if response.status_code == HTTPStatus.PAYMENT_REQUIRED:
            yield from []
        else:
            yield from payload.get("data", [])


class Portfolios(AsanaStream):
    def path(self, stream_slice: Mapping[str, Any] = None, **kwargs) -> str:
        portfolio_gid = stream_slice["portfolio_gid"]
        return f"portfolios/{portfolio_gid}"

    def stream_slices(self, **kwargs) -> Iterable[Optional[Mapping[str, Any]]]:
        yield from self.read_slices_from_records(
            stream_class=PortfoliosCompact, slice_field="portfolio_gid"
        )

    def parse_response(
        self, response: requests.Response, **kwargs
    ) -> Iterable[Mapping]:
        # Handle case of non-paid users
        if response.status_code == HTTPStatus.PAYMENT_REQUIRED:
            return []
        response_json = response.json()
        section_data = response_json.get("data", {})
        if isinstance(section_data, dict):  # Check if section_data is a dictionary
            yield section_data
        elif isinstance(section_data, list):  # Check if section_data is a list
            yield from section_data


class PortfoliosMemberships(AsanaStream):
    def path(self, **kwargs) -> str:
        return "portfolio_memberships"

    def stream_slices(self, **kwargs) -> Iterable[Optional[Mapping[str, Any]]]:
        yield from self.read_slices_from_records(
            stream_class=PortfoliosCompact, slice_field="portfolio_gid"
        )

    def request_params(
        self, stream_slice: Mapping[str, Any] = None, **kwargs
    ) -> MutableMapping[str, Any]:
        params = super().request_params(stream_slice=stream_slice, **kwargs)
        params["portfolio"] = stream_slice["porfolio_gid"]
        return params

    def parse_response(
        self, response: requests.Response, **kwargs
    ) -> Iterable[Mapping]:
        payload = response.json()

        # Handle case of non-paid users
        if response.status_code == HTTPStatus.PAYMENT_REQUIRED:
            yield from []
        else:
            yield from payload.get("data", [])


class SectionsCompact(ProjectRelatedStream):
    use_cache = True

    def path(self, stream_slice: Mapping[str, Any] = None, **kwargs) -> str:
        project_gid = stream_slice["project_gid"]
        return f"projects/{project_gid}/sections"


class Sections(AsanaStream):
    def path(self, stream_slice: Mapping[str, Any] = None, **kwargs) -> str:
        section_gid = stream_slice["section_gid"]
        return f"sections/{section_gid}"

    def stream_slices(self, **kwargs) -> Iterable[Optional[Mapping[str, Any]]]:
        yield from self.read_slices_from_records(
            stream_class=SectionsCompact, slice_field="section_gid"
        )

    def parse_response(
        self, response: requests.Response, **kwargs
    ) -> Iterable[Mapping]:
        response_json = response.json()
        section_data = response_json.get("data", {})
        if isinstance(section_data, dict):  # Check if section_data is a dictionary
            yield section_data
        elif isinstance(section_data, list):  # Check if section_data is a list
            yield from section_data


class StoriesCompact(AsanaStream):
    use_cache = True

    def path(self, stream_slice: Mapping[str, Any] = None, **kwargs) -> str:
        task_gid = stream_slice["task_gid"]
        return f"tasks/{task_gid}/stories"

    def stream_slices(self, **kwargs) -> Iterable[Optional[Mapping[str, Any]]]:
        # This streams causes tests to timeout (> 2hrs), so we limit stream slices to 100 to make tests less noisy
        if self.test_mode:
            yield from islice(
                self.read_slices_from_records(
                    stream_class=Tasks, slice_field="task_gid"
                ),
                100,
            )
        else:
            yield from self.read_slices_from_records(
                stream_class=Tasks, slice_field="task_gid"
            )


class Stories(AsanaStream):
    def path(self, stream_slice: Mapping[str, Any] = None, **kwargs) -> str:
        story_gid = stream_slice["story_gid"]
        return f"stories/{story_gid}"

    def stream_slices(self, **kwargs) -> Iterable[Optional[Mapping[str, Any]]]:
        # This streams causes tests to timeout (> 2hrs), so we limit stream slices to 100 to make tests less noisy
        if self.test_mode:
            yield from islice(
                self.read_slices_from_records(
                    stream_class=StoriesCompact, slice_field="story_gid"
                ),
                100,
            )
        else:
            yield from self.read_slices_from_records(
                stream_class=StoriesCompact, slice_field="story_gid"
            )

    def parse_response(
        self, response: requests.Response, **kwargs
    ) -> Iterable[Mapping]:
        response_json = response.json()
        section_data = response_json.get("data", {})
        if isinstance(section_data, dict):  # Check if section_data is a dictionary
            yield section_data
        elif isinstance(section_data, list):  # Check if section_data is a list
            yield from section_data


class Tags(WorkspaceRequestParamsRelatedStream):
    def path(self, **kwargs) -> str:
        return "tags"


class Tasks(ProjectRelatedStream):
    def path(self, **kwargs) -> str:
        return "tasks"

    def request_params(
        self, stream_slice: Mapping[str, Any] = None, **kwargs
    ) -> MutableMapping[str, Any]:
        params = super().request_params(stream_slice=stream_slice, **kwargs)
        params["project"] = stream_slice["project_gid"]
        return params

    def _handle_object_type(self, prop: str, value: dict) -> str:
        if prop == "custom_fields":
            return prop
        elif prop in ("hearts", "likes"):
            return f"{prop}.user.gid"
        elif prop == "memberships":
            return "memberships.(project|section).gid"

        return f"{prop}.gid"


class Teams(WorkspaceRelatedStream):
    def path(self, stream_slice: Mapping[str, Any] = None, **kwargs) -> str:
        workspace_gid = stream_slice["workspace_gid"]
        return f"organizations/{workspace_gid}/teams"


class TeamMemberships(AsanaStream):
    def path(self, stream_slice: Mapping[str, Any] = None, **kwargs) -> str:
        team_gid = stream_slice["team_gid"]
        return f"teams/{team_gid}/team_memberships"

    def stream_slices(self, **kwargs) -> Iterable[Optional[Mapping[str, Any]]]:
        yield from self.read_slices_from_records(
            stream_class=Teams, slice_field="team_gid"
        )


class Users(WorkspaceRequestParamsRelatedStream):
    def path(self, **kwargs) -> str:
        return "users"

    def _handle_object_type(self, prop: str, value: MutableMapping[str, Any]) -> str:
        if prop == "photo":
            return prop

        return f"{prop}.gid"


class Workspaces(AsanaStream):
    use_cache = True

    def path(self, **kwargs) -> str:
        return "workspaces"
