# SPDX-FileCopyrightText: Copyright (c) 2023 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: LicenseRef-NvidiaProprietary
#
# NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
# property and proprietary rights in and to this material, related
# documentation and any modifications thereto. Any use, reproduction,
# disclosure or distribution of this material and related documentation
# without an express license agreement from NVIDIA CORPORATION or
# its affiliates is strictly prohibited.

import re
import logging
import pexpect


class PexpectWrapper:
    """
    A wrapper class for running commands using Pexpect.

    Attributes:
        command (str): The command to initialize the wrapper.
        username (str, optional): The username for login, if required.
        password (str, optional): The password for login, if required.
        prompt (str): The prompt to expect after running commands.
        child (pexpect.spawn): The Pexpect spawn object.
    """

    def __init__(
        self, command, username=None, password=None, prompt="root@.*:/#", env=None
    ):
        """
        Initialize the wrapper with the given command, and optional username and password.

        Args:
            command (str): The command to initialize the wrapper.
            username (str, optional): The username for login, if required.
            password (str, optional): The password for login, if required.
            prompt (str): The prompt to expect after running commands.
            env (dict, optional): Environment variables to set.
        """
        self.ansi_escape = re.compile(
            r"\x1b\[[0-9;]*[mGKH]|\[(\?)[0-9;]*[a-zA-Z]|\]0;|\x1b[^m]*m|\x1b"
        )
        self.command = command
        self.username = username
        self.password = password
        self.prompt = prompt
        self.env = env or {}
        self.child = None

    def login(self):
        """
        Log in to the system using the provided username and password.
        """
        self.child = pexpect.spawn(self.command, env=self.env)

        if self.username and self.password:
            self.child.expect("login: ")
            self.child.sendline(self.username)
            self.child.expect("Password: ")
            self.child.sendline(self.password)

        self.child.expect(self.prompt)

    def run_command(self, command, prompt=None):
        """
        Run the given command and return the output.

        Args:
            command (str): The command to run.

        Returns:
            str: The output of the command.
        """
        logging.info(f"Running command: {command}")
        self.child.sendline(command)
        if not prompt:
            prompt = self.prompt
        prompt_result = self.child.expect(prompt)
        logging.info(f"Prompt result: {prompt_result}")
        ret = self.ansi_escape.sub("", self.child.before.decode("utf-8"))
        logging.info(f"ansi_escape.sub ret: {ret}")
        return ret

    def check_output(self, expected_output):
        """
        Check the command output against the expected output.

        Args:
            expected_output (str): The expected output to check against.

        Returns:
            bool: True if the output matches the expected output, False otherwise.
        """
        return expected_output in self.ansi_escape.sub(
            "", self.child.before.decode("utf-8")
        )

    def logout(self):
        """
        Close the Pexpect session.
        """
        self.child.sendline("exit")
        self.child.expect(pexpect.EOF)

    def close(self):
        """
        Close the Pexpect session.
        """
        self.child.terminate()
