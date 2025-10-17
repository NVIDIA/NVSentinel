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

import click
from .cleanup import main as cleanup_main


@click.command()
@click.option("--metrics-port", type=int, default=9002, help="Port for metrics server")
@click.option("--retention-days", type=int, default=7, help="Number of days to retain files")
@click.option("--sleep-interval", type=int, default=86400, help="Sleep interval between cleanup cycles in seconds")
@click.option("--target-directory", type=str, default="/usr/share/nginx/html", help="Target directory to clean up")
@click.option("--verbose", is_flag=True, help="Enable verbose logging")
def cli(metrics_port, retention_days, sleep_interval, target_directory, verbose):
    """File Server Log Cleanup and Metrics Service"""
    cleanup_main(
        metrics_port=metrics_port,
        retention_days=retention_days,
        sleep_interval=sleep_interval,
        target_directory=target_directory,
        verbose=verbose,
    )


if __name__ == "__main__":
    cli()
