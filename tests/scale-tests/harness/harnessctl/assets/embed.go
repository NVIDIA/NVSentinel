/*
Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package assets

import "embed"

// FS holds the KWOK stages embedded into harnessctl for bringup. Helm values
// are supplied at runtime via --nvsentinel-values / --monitoring-values.
//
//go:embed kwok/stages-custom.yaml
var FS embed.FS
