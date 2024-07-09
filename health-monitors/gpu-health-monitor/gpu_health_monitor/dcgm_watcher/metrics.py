from prometheus_client import Histogram, Gauge, Counter

dcgm_api_latency = Histogram(
    "dcgm_api_latency", "Amount of time spent calling dcgm APIs", labelnames=["operation_name"]
)
overall_reconcile_loop_time = Histogram("dcgm_reconcile_time", "Amount of time spent running a single reconcile loop")
num_health_watches = Gauge("number_of_health_watches", "Number of health watches available")
num_fields = Gauge("number_of_fields", "Number of available fields to monitor")
callback_failures = Counter(
    "callback_failures",
    "Number of times a callback function has thrown an exception",
    labelnames=["class_name", "func_name"],
)
callback_success = Counter(
    "callback_success",
    "Number of times a callback function has successfully completed",
    labelnames=["class_name", "func_name"],
)
