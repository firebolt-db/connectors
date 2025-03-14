#
# Copyright (c) 2023 Airbyte, Inc., all rights reserved.
#

import time
from abc import ABC
from datetime import timedelta
from typing import Any, Iterable, List, Mapping, MutableMapping, Optional, Union

import pendulum
import requests
from airbyte_cdk.models import FailureType
from airbyte_cdk.sources.streams.http import HttpStream
from airbyte_cdk.sources.streams.http.auth import HttpAuthenticator
from airbyte_cdk.utils import AirbyteTracedException
from pendulum import Date
from source_mixpanel_native.utils import fix_date_time


class MixpanelStream(HttpStream, ABC):
    """
    Formatted API Rate Limit  (https://help.mixpanel.com/hc/en-us/articles/115004602563-Rate-Limits-for-API-Endpoints):
      A maximum of 5 concurrent queries
      60 queries per hour.
    """

    DEFAULT_REQS_PER_HOUR_LIMIT = 60

    @property
    def url_base(self):
        prefix = "eu." if self.region == "EU" else ""
        return f"https://{prefix}mixpanel.com/api/2.0/"

    @property
    def reqs_per_hour_limit(self):
        # https://help.mixpanel.com/hc/en-us/articles/115004602563-Rate-Limits-for-Export-API-Endpoints#api-export-endpoint-rate-limits
        return self._reqs_per_hour_limit

    @reqs_per_hour_limit.setter
    def reqs_per_hour_limit(self, value):
        self._reqs_per_hour_limit = value

    def __init__(
        self,
        authenticator: HttpAuthenticator,
        region: str,
        project_timezone: str,
        start_date: Date = None,
        end_date: Date = None,
        date_window_size: int = 30,  # in days
        attribution_window: int = 0,  # in days
        minimal_cohort_members_properties: bool = True,
        page_size: int = 50000,
        project_id: int = None,
        reqs_per_hour_limit: int = DEFAULT_REQS_PER_HOUR_LIMIT,
        **kwargs,
    ):
        self.start_date = start_date
        self.end_date = end_date
        self.date_window_size = date_window_size
        self.attribution_window = attribution_window
        self.minimal_cohort_members_properties = minimal_cohort_members_properties
        self.page_size = page_size
        self.region = region
        self.project_timezone = project_timezone
        self.project_id = project_id
        self.retries = 0
        self._reqs_per_hour_limit = reqs_per_hour_limit
        super().__init__(authenticator=authenticator)

    def next_page_token(self, response: requests.Response) -> Optional[Mapping[str, Any]]:
        """Define abstract method"""
        return None

    def request_headers(
        self, stream_state: Mapping[str, Any], stream_slice: Mapping[str, Any] = None, next_page_token: Mapping[str, Any] = None
    ) -> Mapping[str, Any]:
        return {"Accept": "application/json"}

    def process_response(self, response: requests.Response, **kwargs) -> Iterable[Mapping]:
        json_response = response.json()
        if self.data_field is not None:
            data = json_response.get(self.data_field, [])
        elif isinstance(json_response, list):
            data = json_response
        elif isinstance(json_response, dict):
            data = [json_response]

        for record in data:
            fix_date_time(record)
            yield record

    def parse_response(
        self,
        response: requests.Response,
        stream_state: Mapping[str, Any],
        **kwargs,
    ) -> Iterable[Mapping]:
        # parse the whole response
        start_time = time.time()
        yield from self.process_response(response, stream_state=stream_state, **kwargs)
        end_time = time.time()
        response_processing_duration = int(end_time - start_time)

        # Don't execute the sleep logic if there's no API request limit.
        if self.reqs_per_hour_limit <= 0:
            return

        # Sleep for N seconds to spread out requests & avoid hitting the API limit.
        remaining_wait_time = (3600 / self.reqs_per_hour_limit) - response_processing_duration
        if remaining_wait_time > 0:
            self.logger.info(f"Sleep for {remaining_wait_time} seconds to match API limitations after reading from {self.name}")
            time.sleep(remaining_wait_time)
        else:
            self.logger.info(f"Processing response from {self.name} took {response_processing_duration} seconds.")

    @property
    def max_retries(self) -> Union[int, None]:
        # we want to limit the max sleeping time by 2^3 * 60 = 8 minutes
        return 3

    def backoff_time(self, response: requests.Response) -> float:
        """
        Some API endpoints do not return "Retry-After" header.
        """

        retry_after = response.headers.get("Retry-After")
        if retry_after:
            self.logger.debug(f"API responded with `Retry-After` header: {retry_after}")
            return float(retry_after)

        self.retries += 1
        return 2**self.retries * 60

    def should_retry(self, response: requests.Response) -> bool:
        if response.status_code == 402:
            self.logger.warning(f"Unable to perform a request. Payment Required: {response.json()['error']}")
            return False
        if response.status_code == 400 and "Unable to authenticate request" in response.text:
            message = (
                f"Your credentials might have expired. Please update your config with valid credentials."
                f" See more details: {response.text}"
            )
            raise AirbyteTracedException(message=message, internal_message=message, failure_type=FailureType.config_error)
        return super().should_retry(response)

    def get_stream_params(self) -> Mapping[str, Any]:
        """
        Fetch required parameters in a given stream. Used to create sub-streams
        """
        params = {
            "authenticator": self.authenticator,
            "region": self.region,
            "project_timezone": self.project_timezone,
            "reqs_per_hour_limit": self.reqs_per_hour_limit,
        }
        if self.project_id:
            params["project_id"] = self.project_id
        return params

    def request_params(
        self,
        stream_state: Mapping[str, Any],
        stream_slice: Mapping[str, Any] = None,
        next_page_token: Mapping[str, Any] = None,
    ) -> MutableMapping[str, Any]:
        if self.project_id:
            return {"project_id": str(self.project_id)}
        return {}


class DateSlicesMixin:
    raise_on_http_errors = True

    def should_retry(self, response: requests.Response) -> bool:
        if response.status_code == requests.codes.bad_request:
            if "to_date cannot be later than today" in response.text:
                self._timezone_mismatch = True
                self.logger.warning(
                    "Your project timezone must be misconfigured. Please set it to the one defined in your Mixpanel project settings. "
                    "Stopping current stream sync."
                )
                setattr(self, "raise_on_http_errors", False)
                return False
        return super().should_retry(response)

    def __init__(self, *args, **kwargs):
        super().__init__(*args, **kwargs)
        self._timezone_mismatch = False

    def parse_response(self, *args, **kwargs):
        if self._timezone_mismatch:
            return []
        yield from super().parse_response(*args, **kwargs)

    def stream_slices(
        self, sync_mode, cursor_field: List[str] = None, stream_state: Mapping[str, Any] = None
    ) -> Iterable[Optional[Mapping[str, Any]]]:
        # use the latest date between self.start_date and stream_state
        start_date = self.start_date
        cursor_value = None

        if stream_state and self.cursor_field and self.cursor_field in stream_state:
            # Remove time part from state because API accept 'from_date' param in date format only ('YYYY-MM-DD')
            # It also means that sync returns duplicated entries for the date from the state (date range is inclusive)
            cursor_value = stream_state[self.cursor_field]
            stream_state_date = pendulum.parse(stream_state[self.cursor_field]).date()
            start_date = max(start_date, stream_state_date)

        # move start_date back <attribution_window> days to sync data since that time as well
        start_date = start_date - timedelta(days=self.attribution_window)

        # end_date cannot be later than today
        end_date = min(self.end_date, pendulum.today(tz=self.project_timezone).date())

        while start_date <= end_date:
            if self._timezone_mismatch:
                return
            current_end_date = start_date + timedelta(days=self.date_window_size - 1)  # -1 is needed because dates are inclusive
            stream_slice = {
                "start_date": str(start_date),
                "end_date": str(min(current_end_date, end_date)),
            }
            if cursor_value:
                stream_slice[self.cursor_field] = cursor_value
            yield stream_slice
            # add 1 additional day because date range is inclusive
            start_date = current_end_date + timedelta(days=1)

    def request_params(
        self, stream_state: Mapping[str, Any], stream_slice: Mapping[str, any] = None, next_page_token: Mapping[str, Any] = None
    ) -> MutableMapping[str, Any]:
        params = super().request_params(stream_state, stream_slice, next_page_token)
        return {
            **params,
            "from_date": stream_slice["start_date"],
            "to_date": stream_slice["end_date"],
        }


class IncrementalMixpanelStream(MixpanelStream, ABC):
    def get_updated_state(self, current_stream_state: MutableMapping[str, Any], latest_record: Mapping[str, Any]) -> Mapping[str, any]:
        updated_state = latest_record.get(self.cursor_field)
        if updated_state:
            state_value = current_stream_state.get(self.cursor_field)
            if state_value:
                updated_state = max(updated_state, state_value)
            current_stream_state[self.cursor_field] = updated_state
        return current_stream_state
