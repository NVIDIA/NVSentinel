# SPDX-FileCopyrightText: Copyright (c) 2021 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: LicenseRef-NvidiaProprietary
#
# NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
# property and proprietary rights in and to this material, related
# documentation and any modifications thereto. Any use, reproduction,
# disclosure or distribution of this material and related documentation
# without an express license agreement from NVIDIA CORPORATION or
# its affiliates is strictly prohibited.
import copy
import json
import logging
import traceback


class Logger(logging.Logger):
    FRAMEWORK_SEPARATION_SIGN = "=" * 80
    FRAMEWORK_SUB_SEPARATION_SIGN = "-" * 80
    TEST_SEPARATION_SIGN = "-" * 60
    TEST_SUB_SEPARATION_SIGN = "." * 50

    def __init__(self, name="pytest_logger"):
        super().__init__(name)
        self.logger = logging.getLogger(name)

        self._step_number = 0
        self._step_title = ""
        self.sub_step_number = 0

    def get_logger(self):
        return self

    def info(self, msg, *args, **kwargs):
        self.logger.info(msg, *args, **kwargs)

    def warning(self, msg, *args, **kwargs):
        self.logger.warning(msg, *args, **kwargs)

    def error(self, msg, *args, **kwargs):
        self.logger.error(msg, *args, **kwargs)

    def critical(self, msg, *args, **kwargs):
        self.logger.critical(msg, *args, **kwargs)

    def exception(self, msg, *args, **kwargs):
        self.logger.exception(msg, *args, **kwargs)

    def debug(self, msg, *args, **kwargs):
        self.logger.debug(msg, *args, **kwargs)

    def print_framework_separation_sign(self):
        self.logger.info(self.FRAMEWORK_SEPARATION_SIGN)

    def print_framework_sub_separation_sign(self):
        self.logger.info(self.FRAMEWORK_SUB_SEPARATION_SIGN)

    def print_test_separation_sign(self):
        self.logger.info(self.TEST_SEPARATION_SIGN)

    def print_test_sub_separation_sign(self):
        self.logger.info(self.TEST_SUB_SEPARATION_SIGN)

    def print_header(self, title: str):
        self.step_number += 1
        self._step_title = title
        self.print_test_separation_sign()
        self.logger.info(self.step)
        self.print_test_separation_sign()

    def print_footer(self):
        self.print_test_separation_sign()

    def print_subheader(self, title: str):
        self.sub_step_number += 1
        self.logger.info(f"{self.step_number}.{self.sub_step_number} {title}")

    def print_subfooter(self):
        self.logger.print_test_sub_separation_sign()

    @property
    def step(self) -> str:
        if self._step_title:
            return f"{self.step_number}. {self._step_title}"
        return f"{self.step_number}"

    @property
    def step_number(self) -> int:
        return self._step_number

    @step_number.setter
    def step_number(self, value: int):
        self._step_number = value
        self._step_title = ""
        self.sub_step_number = 0


class JsonLinesFileHandler(logging.FileHandler):
    def emit(self, record: logging.LogRecord):
        json_record = copy.copy(record)
        attributes = copy.copy(record.__dict__)
        if not isinstance(record.msg, str):
            attributes["msg"] = str(record.msg)
        elif record.args:
            attributes["msg"] = record.msg % record.args
        if record.exc_info:
            attributes["exc_info"] = traceback.format_exception(*record.exc_info)
            json_record.exc_info = None  # Disable standard logging traceback
        if record.args:
            attributes["args"] = [str(arg) for arg in record.args]
            json_record.args = (
                None  # Drop format args - attributes['msg'] already formatted
            )
        json_record.msg = json.dumps(attributes)
        super(JsonLinesFileHandler, self).emit(json_record)
