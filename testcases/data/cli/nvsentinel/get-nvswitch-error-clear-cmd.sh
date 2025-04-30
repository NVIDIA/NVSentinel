#!/bin/bash
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

# This script retrieves the quarantineHealthEvent JSON from a given node,
# transforms it to the desired schema, and prints a MongoDB insertOne command.

# Accept the node name as a command-line argument; if not provided, prompt the user.
if [ -z "$1" ]; then
  read -p "Enter node name: " nodename
else
  nodename="$1"
fi

json_output=$(kubectl get node "$nodename" -o jsonpath='{.metadata.annotations.quarantineHealthEvent}')

if [ -z "$json_output" ]; then
  echo "No quarantineHealthEvent found for node '$nodename'."
  exit 1
fi

version=$(echo "$json_output" | jq '.version')
agent=$(echo "$json_output" | jq -r '.agent')
componentClass=$(echo "$json_output" | jq -r '.componentClass')
checkName=$(echo "$json_output" | jq -r '.checkName')
message=$(echo "$json_output" | jq -r '.message')
recommendedAction=$(echo "$json_output" | jq '.recommendedAction')
errorCode=$(echo "$json_output" | jq '.errorCode')
entitiesImpacted=$(echo "$json_output" | jq '[.entitiesImpacted[] | {entitytype: (.entityType | ascii_upcase), entityvalue: .entityValue}]')
nodeName=$(echo "$json_output" | jq -r '.nodeName')

seconds=$(date +%s)
nanos=$(date +%N)

cat <<EOF
db.HealthEvents.insertOne({
  _id: ObjectId(),
  createdAt: new Date(),
  healthevent: {
    version: NumberLong($version),
    agent: "$agent",
    componentclass: "$componentClass",
    checkname: "$checkName",
    isfatal: false,
    ishealthy: true,
    message: "$message",
    recommendedaction: $recommendedAction,
    errorcode: $errorCode,
    entitiesimpacted: $entitiesImpacted,
    metadata: null,
    generatedtimestamp: { seconds: NumberLong($seconds), nanos: NumberLong($nanos) },
    nodename: "$nodeName"
  }
});
EOF
