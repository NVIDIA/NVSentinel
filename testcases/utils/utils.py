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

import base64
import os
import string
import random
import logging
import time
from functools import wraps
from typing import Type, Union, Callable, Any


def encode_password(password):
    encoded_bytes = base64.b64encode(password.encode("utf-8"))
    encoded_str = str(encoded_bytes, "utf-8")
    return encoded_str


def decode_password(encoded_str):
    decoded_bytes = base64.b64decode(encoded_str)
    decoded_str = str(decoded_bytes, "utf-8")
    return decoded_str


def is_running_in_docker():
    # Check environment variable
    if os.getenv("RUNNING_IN_DOCKER") == "true":
        return True
    # Check /proc/self/cgroup file
    try:
        with open("/proc/self/cgroup", "rt") as ifh:
            return "docker" in ifh.read()
    except FileNotFoundError:
        return False


def generate_random_suffix(length=6):
    characters = string.ascii_lowercase + string.digits
    random_suffix = "".join(random.choice(characters) for _ in range(length))
    return random_suffix


def retry(
    max_retries: int = 3,
    retry_delay: float = 1,
    exponential_backoff: bool = True,
    exceptions: Union[Type[Exception], tuple[Type[Exception], ...]] = Exception,
):
    """
    A retry decorator with exponential backoff

    Args:
        max_retries: Maximum number of retries
        retry_delay: Initial delay between retries in seconds
        exponential_backoff: Whether to use exponential backoff
        exceptions: Exception or tuple of exceptions to catch
    """

    def decorator(func: Callable) -> Callable:
        @wraps(func)
        def wrapper(*args, **kwargs) -> Any:
            current_delay = retry_delay
            last_exception = None

            for attempt in range(max_retries):
                try:
                    return func(*args, **kwargs)
                except exceptions as e:
                    last_exception = e
                    if attempt == max_retries - 1:  # Last attempt
                        logging.error(
                            f"Max retries ({max_retries}) reached for {func.__name__}"
                        )
                        raise last_exception

                    logging.warning(
                        f"Attempt {attempt + 1}/{max_retries} failed for {func.__name__}. "
                        f"Error: {str(e)}. Retrying in {current_delay} seconds..."
                    )

                    time.sleep(current_delay)
                    if exponential_backoff:
                        current_delay *= 2

            return None  # Should never reach here

        return wrapper

    return decorator
