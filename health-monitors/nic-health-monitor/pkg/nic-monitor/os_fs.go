/*
 * Copyright (c) 2024, NVIDIA CORPORATION.  All rights reserved.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package nic_monitor

import (
	"io/fs"
	"os"
	"strings"
	"testing/fstest"
)

var fileSystem FileSystem = osFS{}

type FileSystem interface {
	Stat(name string) (os.FileInfo, error)
	ReadFile(name string) ([]byte, error)
	ReadDir(name string) ([]fs.DirEntry, error)
}

// osFS implements fileSystem using the local disk.
type osFS struct{}

func (osFS) Stat(name string) (os.FileInfo, error)      { return os.Stat(name) }
func (osFS) ReadFile(name string) ([]byte, error)       { return os.ReadFile(name) }
func (osFS) ReadDir(name string) ([]fs.DirEntry, error) { return os.ReadDir(name) }

// MockFileSystem implements mock fileSystem
type MockFileSystem struct {
	Fs fstest.MapFS
}

// fstest does not support absolute path, so we trim prefixed "/"
func (m MockFileSystem) Stat(name string) (os.FileInfo, error) {
	return m.Fs.Stat(strings.TrimPrefix(name, "/"))
}
func (m MockFileSystem) ReadFile(name string) ([]byte, error) {
	return m.Fs.ReadFile(strings.TrimPrefix(name, "/"))
}
func (m MockFileSystem) ReadDir(name string) ([]fs.DirEntry, error) {
	return m.Fs.ReadDir(strings.TrimPrefix(name, "/"))
}
