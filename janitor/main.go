// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"os"
	"os/signal"
	"syscall"

	"k8s.io/klog/v2"
	"k8s.io/klog/v2/textlogger"
)

var (
	// These variables will be populated during the build process
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func run() error {
	logger := textlogger.NewLogger(textlogger.NewConfig()).WithValues(
		"version", version,
		"module", "janitor",
	)

	klog.SetLogger(logger)
	klog.InfoS("Starting janitor module", "version", version, "commit", commit, "date", date)

	// Wait for termination signal - business logic will be added here
	klog.Info("Janitor module is running...")

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	sig := <-sigChan
	klog.InfoS("Received signal, shutting down", "signal", sig)

	return nil
}

func main() {
	klog.InitFlags(nil)

	if err := run(); err != nil {
		klog.ErrorS(err, "Fatal error")
		klog.Flush()
		//nolint:gocritic // exitAfterDefer: klog.Flush() is explicitly called before os.Exit()
		os.Exit(1)
	}

	klog.Flush()
}
