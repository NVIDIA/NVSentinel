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

package tests

import (
	"fmt"
	"os"
	"testing"

	"github.com/go-logr/logr"
	"go.uber.org/zap/zapcore"
	"sigs.k8s.io/controller-runtime/pkg/log"
	kwokv1alpha1 "sigs.k8s.io/kwok/pkg/apis/v1alpha1"

	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/e2e-framework/pkg/env"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
)

var testEnv env.Environment

// TestMain sets up the test environment and makes testEnv available
func TestMain(m *testing.M) {
	log.SetLogger(NewLogger())

	cfg, err := envconf.NewFromFlags()
	if err != nil {
		panic(fmt.Sprintf("failed to create environment config: %v", err))
	}

	// Register KWOK types with the scheme for typed client support
	if err := kwokv1alpha1.AddToScheme(cfg.Client().Resources().GetScheme()); err != nil {
		panic(fmt.Sprintf("failed to register KWOK types: %v", err))
	}

	testEnv = env.NewWithConfig(cfg)
	os.Exit(testEnv.Run(m))
}

func NewLogger(opts ...zap.Opts) logr.Logger {
	zapOpts := &zap.Options{}
	zapOpts.Development = true
	zapOpts.StacktraceLevel = zapcore.PanicLevel
	zapOpts.EncoderConfigOptions = append(zapOpts.EncoderConfigOptions, func(ec *zapcore.EncoderConfig) {
		ec.EncodeLevel = zapcore.CapitalColorLevelEncoder
		ec.EncodeTime = zapcore.TimeEncoderOfLayout("[15:04:05.000]")
	})
	return zap.New(append([]zap.Opts{zap.UseFlagOptions(zapOpts)}, opts...)...)
}
