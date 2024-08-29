import socket
import argparse
import grpc
import time
from protos import platformconnector_pb2, platformconnector_pb2_grpc


# Function to send the HealthEvent message
def send_health_event(health_event: platformconnector_pb2.HealthEvent):
    # Path to the UNIX domain socket
    unix_socket_path = "/var/run/nvsentinel.sock"

    with grpc.insecure_channel(f"unix://{unix_socket_path}") as chan:
        stub = platformconnector_pb2_grpc.PlatformConnectorStub(chan)
        events = [health_event]
        stub.HealthEventOccuredV1(platformconnector_pb2.HealthEvents(events=events, version=1))


if __name__ == "__main__":
    # clear all xid errors on the node
    health_event = platformconnector_pb2.HealthEvent()
    health_event.version = 1
    health_event.agent = "gpu-health-monitor"
    health_event.componentClass = "gpu"
    health_event.checkName = "GpuXidError"
    health_event.isFatal = False
    health_event.isHealthy = True
    health_event.message = "No Health Failures"
    health_event.recommendedAction = platformconnector_pb2.NONE
    health_event.generatedTimestamp.seconds = int(time.time())
    health_event.generatedTimestamp.nanos = 0
    # Send the HealthEvent message with the provided arguments
    send_health_event(health_event)
