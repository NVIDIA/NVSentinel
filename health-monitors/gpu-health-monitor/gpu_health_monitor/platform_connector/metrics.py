from prometheus_client import Histogram

dcgm_health_events_publish_time_to_grpc_channel = Histogram(
    "dcgm_health_events_publish_time_to_grpc_channel",
    "Amount of time spent in publishing dcgm health events on the grpc channel",
    labelnames=["operation_name"],
)
xid_events_publish_time_to_grpc_channel = Histogram(
    "xid_events_publish_time_to_grpc_channel",
    "Amount of time spent in publishing xid events on the grpc channel",
    labelnames=["operation_name"],
)
