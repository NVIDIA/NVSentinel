# Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

import logging
import os
import sys
from importlib.metadata import version, PackageNotFoundError

from .config import Config
from .diag import DCGMDiagnostic
from .health import HealthReporter
from .logger import setup_logging
from .protos import health_event_pb2 as pb


def get_version() -> str:
    try:
        return version("dcgm-diag")
    except PackageNotFoundError:
        return "dev"


def main() -> None:
    log_level = os.getenv("LOG_LEVEL", "info")
    setup_logging("preflight-dcgm-diag", get_version(), log_level)
    log = logging.getLogger(__name__)

    try:
        cfg = Config.from_env()
    except ValueError as e:
        log.error("Configuration error", extra={"error": str(e)})
        sys.exit(1)

    log.info(
        "Starting preflight dcgm-diag check",
        extra={
            "diag_level": cfg.diag_level,
            "processing_strategy": pb.ProcessingStrategy.Name(cfg.processing_strategy),
        },
    )

    reporter = HealthReporter(
        socket_path=cfg.connector_socket,
        node_name=cfg.node_name,
        processing_strategy=cfg.processing_strategy,
    )

    diag = DCGMDiagnostic(hostengine_addr=cfg.hostengine_addr)

    try:
        results = diag.run(cfg.diag_level)
    except Exception as e:
        log.error("DCGM diagnostic failed", extra={"error": str(e)})
        # is_fatal=True: sets node condition for visibility, even though this may be
        # an infrastructure issue (DCGM unavailable) rather than a confirmed GPU failure.
        reporter.send_event(gpu_uuid="", is_healthy=False, is_fatal=True, message=str(e))
        sys.exit(1)

    failures = [r for r in results if r.status == "fail"]
    warnings = [r for r in results if r.status == "warn"]
    passes = [r for r in results if r.status == "pass"]

    log.info(
        "Diagnostic summary",
        extra={
            "passed": len(passes),
            "failed": len(failures),
            "warned": len(warnings),
            "skipped": len(results) - len(passes) - len(failures) - len(warnings),
            "total": len(results),
        },
    )

    # Send one event per test result with specific test name
    for r in results:
        if r.status not in ("pass", "warn", "fail"):
            continue

        is_pass = r.status == "pass"
        is_fatal = r.status == "fail"
        message = "Test passed" if is_pass else r.error_message

        log.log(
            logging.INFO if is_pass else (logging.ERROR if is_fatal else logging.WARNING),
            f"Test {r.status}",
            extra={"gpu": r.gpu_uuid, "test": r.test_name, "error_code": r.error_code, "detail": message},
        )
        reporter.send_event(
            gpu_uuid=r.gpu_uuid,
            is_healthy=is_pass,
            is_fatal=is_fatal,
            message=message,
            error_code=r.error_code if not is_pass else 0,
            test_name=r.test_name,
        )

    if failures:
        log.error("DCGM diagnostic check failed")
        sys.exit(1)

    log.info("DCGM diagnostic check passed")
    sys.exit(0)


if __name__ == "__main__":
    main()
