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

import grpc
import time
import argparse
from protos import platformconnector_pb2, platformconnector_pb2_grpc


# Function to send the HealthEvent message
def send_health_event(health_event: platformconnector_pb2.HealthEvent):
    # Path to the UNIX domain socket
    unix_socket_path = "/var/run/nvsentinel.sock"

    with grpc.insecure_channel(f"unix://{unix_socket_path}") as chan:
        stub = platformconnector_pb2_grpc.PlatformConnectorStub(chan)
        events = [health_event]
        stub.HealthEventOccuredV1(
            platformconnector_pb2.HealthEvents(events=events, version=1)
        )


if __name__ == "__main__":
    # Parse command line arguments
    parser = argparse.ArgumentParser(
        description="Send HealthEvent message to gRPC server."
    )
    parser.add_argument(
        "--nodeName", required=True, help="The nodeName to be set in the HealthEvent."
    )
    parser.add_argument(
        "--version",
        type=int,
        required=True,
        help="The version to be set in the HealthEvent.",
    )
    parser.add_argument(
        "--agent", required=True, help="The agent to be set in the HealthEvent."
    )
    parser.add_argument(
        "--componentClass",
        required=True,
        help="The componentClass to be set in the HealthEvent.",
    )
    parser.add_argument(
        "--checkName", required=True, help="The checkName to be set in the HealthEvent."
    )
    parser.add_argument(
        "--entityType",
        required=True,
        help="The entityType to be set in the HealthEvent.",
    )
    parser.add_argument(
        "--entityValue",
        required=True,
        help="The entityValue to be set in the HealthEvent.",
    )
    args = parser.parse_args()

    # Clear all xid errors on the node
    health_event = platformconnector_pb2.HealthEvent()
    health_event.version = args.version  # Set version from command line argument
    health_event.agent = args.agent  # Set agent from command line argument
    health_event.componentClass = (
        args.componentClass
    )  # Set componentClass from command line argument
    health_event.checkName = args.checkName  # Set checkName from command line argument
    health_event.isFatal = False
    health_event.isHealthy = True
    health_event.message = "No Health Failures"
    health_event.recommendedAction = platformconnector_pb2.NONE
    health_event.generatedTimestamp.seconds = int(time.time())
    health_event.generatedTimestamp.nanos = 0
    health_event.entitiesImpacted.append(
        platformconnector_pb2.Entity(
            entityType=args.entityType, entityValue=args.entityValue
        )
    )  # Set entityType and entityValue from command line arguments
    health_event.nodeName = args.nodeName  # Set nodeName from command line argument

    # Send the HealthEvent message with the provided arguments
    send_health_event(health_event)
