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

package sxid_monitor

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"k8s.io/klog"
)

const (
	KMSG_DEV = "/dev/kmsg"
)

type SxidErrorMonitor struct {
	fileName  string
	file      *os.File
	scanner   *bufio.Scanner
	EventChan chan *SXIDErrorEvent
}

func NewSxidErrorMonitor() (*SxidErrorMonitor, error) {
	collector := &SxidErrorMonitor{
		fileName:  KMSG_DEV,
		EventChan: make(chan *SXIDErrorEvent),
	}

	var err error
	collector.file, err = os.OpenFile(collector.fileName, os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}

	_, err = collector.file.Seek(0, io.SeekEnd)
	if err != nil {
		collector.file.Close() //nolint:errcheck
		return nil, fmt.Errorf("error seeking to the tail of kmsg: %w", err)
	}

	collector.scanner = bufio.NewScanner(collector.file)

	return collector, err
}

func (c *SxidErrorMonitor) Close() error {
	return c.file.Close()
}

func (c *SxidErrorMonitor) Run() error {
	klog.Infof("Collecting SXid events from %s\n", c.fileName)
	for c.scanner.Scan() {
		log := c.scanner.Text()

		m, err := ParseSXIDError(log)
		if err != nil {
			klog.Error("Report Error\n")
			continue
		}

		if m != nil {
			klog.Info(m)
			c.EventChan <- m
		}
	}

	if err := c.scanner.Err(); err != nil {
		klog.Errorf("error occurred: %v\n", err)
	}

	return nil
}

type SXIDErrorEvent struct {
	ErrorNum int
	IsFatal  bool
	NVSwitch int
	PCI      string
	Link     int
	Message  string
}

// nolint:cyclop
// most of complexity comes from error checking. so we skip cyclomatic check here
func ParseSXIDError(str string) (*SXIDErrorEvent, error) {
	const (
		// index for each token of the log
		// PRIORITY,SEQUENCE_NUM,TIMESTAMP,-;MESSAGE
		// MESSAGE := nvidia-nvswitch1 SXid (PCI:0000:06:00.0): 20009, Non-fatal, Link 04\n"
		NvswitchX   = 0
		SXidConst   = 1
		pciIdx      = 2
		errorNumIdx = 3
		fatalIdx    = 4
		linkStrIdx  = 5
		linkIdx     = 6
	)

	words := strings.Fields(str)

	if len(words) <= SXidConst {
		// log does not have enough information to check if the log is for SXid, so skip.
		return nil, nil
	}

	if !strings.Contains(words[NvswitchX], "nvidia-nvswitch") || words[SXidConst] != "SXid" {
		// skip for non SXId log
		return nil, nil
	}

	if len(words) <= linkIdx {
		return nil, fmt.Errorf("log message truncated: %s", str)
	}

	kmsgMsgIdx := strings.Index(words[NvswitchX], ";")

	nvswitchStr := strings.TrimPrefix(words[NvswitchX][kmsgMsgIdx+1:], "nvidia-nvswitch")
	nvswitch, err := strconv.Atoi(nvswitchStr)
	if err != nil {
		return nil, fmt.Errorf("cannot parse nvidia-nvswitch %s", words[NvswitchX][kmsgMsgIdx:])
	}

	pci := words[pciIdx]
	pci = strings.TrimPrefix(pci, "(PCI:")
	pci = strings.TrimSuffix(pci, "):")

	errorNumStr := words[errorNumIdx]
	errorNumStr = strings.TrimSuffix(errorNumStr, ",")
	errorNum, err := strconv.Atoi(errorNumStr)
	if err != nil {
		return nil, fmt.Errorf("wrong errorNum %s", words[errorNumIdx])
	}

	var isFatal bool
	switch {
	case strings.HasPrefix(words[fatalIdx], "Fatal"):
		isFatal = true
	case strings.HasPrefix(words[fatalIdx], "Non-fatal"):
		isFatal = false
	default:
		// it is a continuing error line, so we don't report metric
		return nil, nil
	}

	// The first log of each SXid incidents must have "Fatal" | "Non-fatal" information

	if words[linkStrIdx] != "Link" {
		// Some old log has missing Link information - we'd like report this as error and monitor
		return nil, errors.New("link information is missing")
	}

	link, err := strconv.Atoi(words[linkIdx])
	if err != nil {
		return nil, fmt.Errorf("cannot parse link %s", words[linkIdx])
	}

	m := SXIDErrorEvent{
		NVSwitch: nvswitch,
		PCI:      pci,
		ErrorNum: errorNum,
		IsFatal:  isFatal,
		Link:     link,
		Message:  str[kmsgMsgIdx+1:],
	}

	return &m, nil
}
