# Copyright (c) 2024, NVIDIA CORPORATION.  All rights reserved.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

import json
import logging
import requests
from typing import Dict, Optional, Union, Any
from requests.adapters import HTTPAdapter
from requests.packages.urllib3.util.retry import Retry
from urllib3.exceptions import InsecureRequestWarning

# Suppress only the single warning from urllib3 needed.
requests.packages.urllib3.disable_warnings(category=InsecureRequestWarning)

logger = logging.getLogger(__name__)


class HttpsClient:
    """A utility class for making HTTPS requests with retry mechanism and error handling"""

    def __init__(
        self,
        base_url: str = "",
        headers: Optional[Dict] = None,
        verify_ssl: bool = False,
        timeout: int = 30,
        max_retries: int = 3,
    ):
        """Initialize the HTTPS client

        Args:
            base_url: Base URL for all requests
            headers: Default headers to use for all requests
            verify_ssl: Whether to verify SSL certificates
            timeout: Request timeout in seconds
            max_retries: Maximum number of retries for failed requests
        """
        self.base_url = base_url.rstrip("/")
        self.headers = headers or {}
        self.verify_ssl = verify_ssl
        self.timeout = timeout

        # Setup session with retry strategy
        self.session = requests.Session()
        retry_strategy = Retry(
            total=max_retries, backoff_factor=0.5, status_forcelist=[500, 502, 503, 504]
        )
        adapter = HTTPAdapter(max_retries=retry_strategy)
        self.session.mount("https://", adapter)
        self.session.verify = verify_ssl

    def _build_url(self, endpoint: str) -> str:
        """Build full URL from endpoint"""
        if endpoint.startswith(("http://", "https://")):
            return endpoint
        return f"{self.base_url}/{endpoint.lstrip('/')}"

    def _handle_response(self, response: requests.Response) -> Any:
        """Handle API response and errors"""
        try:
            response.raise_for_status()
            return response.json() if response.content else None
        except requests.exceptions.JSONDecodeError:
            return response.text
        except requests.exceptions.RequestException as e:
            logger.error(f"Request failed: {str(e)}")
            logger.error(f"Response content: {response.text}")
            raise

    def get(self, endpoint: str, params: Optional[Dict] = None, **kwargs) -> Any:
        """Send GET request

        Args:
            endpoint: API endpoint
            params: Query parameters
            **kwargs: Additional arguments to pass to requests
        """
        url = self._build_url(endpoint)
        logger.debug(f"GET {url} with params {params}")

        response = self.session.get(
            url, params=params, headers=self.headers, timeout=self.timeout, **kwargs
        )
        return self._handle_response(response)

    def post(
        self,
        endpoint: str,
        data: Optional[Union[Dict, str]] = None,
        json_data: Optional[Dict] = None,
        **kwargs,
    ) -> Any:
        """Send POST request

        Args:
            endpoint: API endpoint
            data: Form data or raw string data
            json_data: JSON data
            **kwargs: Additional arguments to pass to requests
        """
        url = self._build_url(endpoint)
        logger.debug(f"POST {url}")
        if json_data:
            logger.debug(f"JSON data: {json.dumps(json_data)}")

        response = self.session.post(
            url,
            data=data,
            json=json_data,
            headers=self.headers,
            timeout=self.timeout,
            **kwargs,
        )
        return self._handle_response(response)

    def put(
        self,
        endpoint: str,
        data: Optional[Union[Dict, str]] = None,
        json_data: Optional[Dict] = None,
        **kwargs,
    ) -> Any:
        """Send PUT request"""
        url = self._build_url(endpoint)
        logger.debug(f"PUT {url}")

        response = self.session.put(
            url,
            data=data,
            json=json_data,
            headers=self.headers,
            timeout=self.timeout,
            **kwargs,
        )
        return self._handle_response(response)

    def delete(self, endpoint: str, **kwargs) -> Any:
        """Send DELETE request"""
        url = self._build_url(endpoint)
        logger.debug(f"DELETE {url}")

        response = self.session.delete(
            url, headers=self.headers, timeout=self.timeout, **kwargs
        )
        return self._handle_response(response)

    def patch(
        self,
        endpoint: str,
        data: Optional[Union[Dict, str]] = None,
        json_data: Optional[Dict] = None,
        **kwargs,
    ) -> Any:
        """Send PATCH request"""
        url = self._build_url(endpoint)
        logger.debug(f"PATCH {url}")

        response = self.session.patch(
            url,
            data=data,
            json=json_data,
            headers=self.headers,
            timeout=self.timeout,
            **kwargs,
        )
        return self._handle_response(response)

    def upload_file(
        self, endpoint: str, files: Dict[str, tuple], data: Optional[Dict] = None, **kwargs
    ) -> Any:
        """Upload file(s)

        Args:
            endpoint: API endpoint
            files: Dictionary of files to upload
            data: Additional form data
            **kwargs: Additional arguments to pass to requests
        """
        url = self._build_url(endpoint)
        logger.debug(f"POST (file upload) {url}")

        response = self.session.post(
            url,
            files=files,
            data=data,
            headers=self.headers,
            timeout=self.timeout,
            **kwargs,
        )
        return self._handle_response(response)

    def download_file(self, endpoint: str, local_path: str, **kwargs) -> None:
        """Download file

        Args:
            endpoint: API endpoint
            local_path: Path to save downloaded file
            **kwargs: Additional arguments to pass to requests
        """
        url = self._build_url(endpoint)
        logger.debug(f"GET (file download) {url}")

        response = self.session.get(
            url, headers=self.headers, stream=True, timeout=self.timeout, **kwargs
        )
        response.raise_for_status()

        with open(local_path, "wb") as f:
            for chunk in response.iter_content(chunk_size=8192):
                f.write(chunk)

    def set_token(self, token: str, token_type: str = "Bearer") -> None:
        """Set authentication token

        Args:
            token: Authentication token
            token_type: Token type (e.g., 'Bearer', 'Basic')
        """
        self.headers["Authorization"] = f"{token_type} {token}"

    def set_headers(self, headers: Dict[str, str]) -> None:
        """Update headers

        Args:
            headers: Headers to update/add
        """
        self.headers.update(headers)
