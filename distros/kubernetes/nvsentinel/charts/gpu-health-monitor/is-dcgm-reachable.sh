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

#!/bin/bash

DCGM_HOST="nvidia-dcgm.gpu-operator.svc"
DCGM_PORT=5555
MAX_ATTEMPTS=60
INTERVAL=60

check_dcgm() {
    output=$(nc -zv $DCGM_HOST $DCGM_PORT 2>&1)
    if [ $? -eq 0 ]; then
    if echo "$output" | grep -q "open"; then
        echo "DCGM service is reachable"
        echo "Details: $output"
        return 0
    else
        echo "Unexpected output from netcat: $output"
        return 1
    fi
    else
    echo "Unable to connect to DCGM service"
    echo "Error: $output"
    return 1
    fi
}

for attempt in $(seq 1 $MAX_ATTEMPTS); do
    echo "Attempt $attempt of $MAX_ATTEMPTS"
    if check_dcgm; then
    echo "DCGM service is reachable. Exiting successfully."
    exit 0
    fi
    if [ $attempt -lt $MAX_ATTEMPTS ]; then
    echo "Waiting $INTERVAL seconds before next attempt."
    sleep $INTERVAL
    fi
done

echo "Max attempts reached. DCGM service is not reachable."
exit 1