// Copyright (c) 2024, NVIDIA CORPORATION.  All rights reserved.
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
	"flag"
	"fmt"
	"os"

	"github.com/go-logr/logr"
	"github.com/go-logr/zapr"
	"github.com/spf13/cobra"
	"gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/tools/pkg/generate"
	"go.uber.org/zap"
)

var (
	credentialsPath, gpuHealthMonitorXIDPath, nvSwitchHealthMonitorSXIDPath string
	logger = func() logr.Logger {
		zapLog, _ := zap.NewDevelopment()
		return zapr.NewLogger(zapLog)
	}()
	rootCmd = &cobra.Command{
		Use:   "cli",
		Short: "generate XID/SXID configmap",
		Long:  "A tool for generating fatality map for XID/SXID errors\nFor setup instructions, please refer to https://developers.google.com/sheets/api/quickstart/go",
		Run: func(cmd *cobra.Command, args []string) {
			err := generate.Generate(logger, credentialsPath, gpuHealthMonitorXIDPath, nvSwitchHealthMonitorSXIDPath)
			if err != nil {
				logger.Error(err, "Failed to generate XID/SXID configmap")
				os.Exit(1)
			}
		},
	}
)

func init() {
	
	rootCmd.Flags().StringVarP(&credentialsPath, "credentialsPath", "c", "credentials.json", "Path to the credentials file, can be download from https://console.cloud.google.com/auth/clients?project=proj-dgxc-runai-np-dev-mega")
	rootCmd.Flags().StringVarP(&gpuHealthMonitorXIDPath, "gpuHealthMonitorXIDPath", 
		"g", "../distros/kubernetes/nvsentinel/charts/gpu-health-monitor/xiderrorsmapping.csv", "Path to the gpu health monitor XID error mappings file")
		rootCmd.Flags().StringVarP(&nvSwitchHealthMonitorSXIDPath, "nvSwitchHealthMonitorSXIDPath", 
		"n", "../distros/kubernetes/nvsentinel/charts/nvswitch-health-monitor/sxiderrorsmapping.csv", "Path to the nv switch health monitor SXID error mappings file")

	rootCmd.PersistentFlags().AddGoFlagSet(flag.CommandLine)
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}