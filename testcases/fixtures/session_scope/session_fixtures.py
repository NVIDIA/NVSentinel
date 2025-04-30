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

import pytest
import logging
from copy import deepcopy


from testcases.utils.utils import decode_password


logger = logging.getLogger(__name__)
RUNNING_CONFIG = "http://cqa-fs01.nvidia.com/Automation/RUNAI/runai_api.config"





@pytest.fixture(scope="session")
def get_project_namespace(request, runai_cli):
    project = runai_cli.config.project
    if project == "":
        project = "qa-automation-test"
    return "runai-" + project


@pytest.fixture(scope="session", autouse=True)
def request_config(request):
    config = deepcopy(request.config.option)
    if config.db_password:
        encoded_password = config.db_password
        decoded_password = decode_password(encoded_password)
        config.db_password = decoded_password
    return config



@pytest.fixture(scope="session")
def case_id():
    pass
