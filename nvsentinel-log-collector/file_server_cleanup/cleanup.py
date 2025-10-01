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

import os
import time
import shutil
import subprocess
import logging
from prometheus_client import Counter, Gauge, start_http_server

logging.basicConfig(level=logging.INFO, format="%(asctime)s - %(levelname)s - %(message)s")
logger = logging.getLogger(__name__)
successful_cleanups = Counter("fileserver_log_rotation_successful_total", "Total successful log cleanup operations")
failed_cleanups = Counter("fileserver_log_rotation_failed_total", "Total failed log cleanup operations")
disk_free_bytes = Gauge("fileserver_disk_space_free_bytes", "Free disk space in bytes")


def get_env_int(key, default):
    try:
        return int(os.getenv(key, str(default)))
    except ValueError:
        logger.warning(f"Invalid value for {key}, using default: {default}")
        return default


def cleanup_old_files(target_dir, retention_days):
    # Validate target_dir to prevent directory traversal attacks
    normalized_path = os.path.normpath(target_dir)
    if not os.path.isabs(normalized_path) or not normalized_path.startswith("/usr/share/nginx/html"):
        logger.error(f"Invalid target directory: {target_dir}")
        failed_cleanups.inc()
        return False

    try:
        cmd = ["find", target_dir, "-type", "f", "-mtime", f"+{retention_days}", "-delete"]

        result = subprocess.run(cmd, timeout=300)

        if result.returncode == 0:
            logger.info(f"Cleanup completed for files older than {retention_days} days")
            successful_cleanups.inc()
            return True
        else:
            logger.error(f"Cleanup failed: {result.stderr}")
            failed_cleanups.inc()
            return False

    except subprocess.TimeoutExpired:
        logger.exception("Cleanup operation timed out")
        failed_cleanups.inc()
        return False
    except Exception as e:
        logger.exception(f"Cleanup operation failed: {e}")
        failed_cleanups.inc()
        return False


def update_disk_metrics(target_dir):
    try:
        usage = shutil.disk_usage(target_dir)
        disk_free_bytes.set(usage.free)
    except Exception as e:
        logger.error(f"Failed to update disk metrics: {e}")


def main(metrics_port=None, retention_days=None, sleep_interval=None, target_directory=None, verbose=False):
    # Use CLI parameters if provided, otherwise fall back to environment variables
    metrics_port = metrics_port or get_env_int("CLEANUP_METRICS_PORT", 9002)
    retention_days = retention_days or get_env_int("LOG_RETENTION_DAYS", 7)
    sleep_interval = sleep_interval or get_env_int("LOG_CLEANUP_SLEEP_INTERVAL", 86400)
    target_directory = target_directory or os.getenv("TARGET_DIRECTORY", "/usr/share/nginx/html")

    # Set logging level based on verbose flag
    if verbose:
        logging.getLogger().setLevel(logging.DEBUG)

    logger.info(
        f"Starting File Server Cleanup Service - Port: {metrics_port}, Retention: {retention_days} days, Interval: {sleep_interval}s"
    )

    try:
        start_http_server(metrics_port)
        logger.info(f"Metrics server started on port {metrics_port}")
    except Exception as e:
        logger.error(f"Failed to start metrics server: {e}")
        return 1

    while True:
        try:
            update_disk_metrics(target_directory)
            cleanup_old_files(target_directory, retention_days)
            logger.info(f"Cleanup cycle complete. Next run in {sleep_interval} seconds")
            time.sleep(sleep_interval)
        except Exception as e:
            logger.error(f"Unexpected error: {e}")
            failed_cleanups.inc()
            time.sleep(60)

    logger.info("File Server Cleanup Service stopped")
    return 0


if __name__ == "__main__":
    exit(main())
