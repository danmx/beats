// Licensed to Elasticsearch B.V. under one or more contributor
// license agreements. See the NOTICE file distributed with
// this work for additional information regarding copyright
// ownership. Elasticsearch B.V. licenses this file to you under
// the Apache License, Version 2.0 (the "License"); you may
// not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

package add_process_metadata

import (
	"fmt"
	"strconv"
	"time"

	"github.com/pkg/errors"

	"github.com/elastic/beats/v7/libbeat/beat"
	"github.com/elastic/beats/v7/libbeat/common"
	"github.com/elastic/beats/v7/libbeat/common/atomic"
	"github.com/elastic/beats/v7/libbeat/logp"
	"github.com/elastic/beats/v7/libbeat/processors"
	jsprocessor "github.com/elastic/beats/v7/libbeat/processors/script/javascript/module/processor"
	"github.com/elastic/gosigar/cgroup"
)

const (
	processorName   = "add_process_metadata"
	cacheExpiration = time.Second * 30
)

var (
	// ErrNoMatch is returned when the event doesn't contain any of the fields
	// specified in match_pids.
	ErrNoMatch = errors.New("none of the fields in match_pids found in the event")

	// ErrNoProcess is returned when metadata for a process can't be collected.
	ErrNoProcess = errors.New("process not found")

	procCache = newProcessCache(cacheExpiration, gosysinfoProvider{})

	processCgroupPaths = cgroup.ProcessCgroupPaths

	instanceID atomic.Uint32

	isContainerIDEnabled bool
)

type addProcessMetadata struct {
	config      config
	provider    processMetadataProvider
	cidProvider cidProvider
	log         *logp.Logger
	mappings    common.MapStr
}

type processMetadata struct {
	name, title, exe string
	args             []string
	env              map[string]string
	startTime        time.Time
	pid, ppid        int
	//
	fields common.MapStr
}

type processMetadataProvider interface {
	GetProcessMetadata(pid int) (*processMetadata, error)
}

type cidProvider interface {
	GetCid(pid int) (string, error)
}

func init() {
	processors.RegisterPlugin(processorName, New)
	jsprocessor.RegisterPlugin("AddProcessMetadata", New)
}

// New constructs a new add_process_metadata processor.
func New(cfg *common.Config) (processors.Processor, error) {
	return newProcessMetadataProcessorWithProvider(cfg, &procCache)
}

func newProcessMetadataProcessorWithProvider(cfg *common.Config, provider processMetadataProvider) (proc processors.Processor, err error) {
	// Logging (each processor instance has a unique ID).
	var (
		id  = int(instanceID.Inc())
		log = logp.NewLogger(processorName).With("instance_id", id)
	)

	config := defaultConfig()
	if err = cfg.Unpack(&config); err != nil {
		return nil, errors.Wrapf(err, "fail to unpack the %v configuration", processorName)
	}

	mappings, err := config.getMappings()

	if err != nil {
		return nil, errors.Wrapf(err, "error unpacking %v.target_fields", processorName)
	}

	var p addProcessMetadata

	// don't use cgroup.ProcessCgroupPaths to save it from doing the work when container id disabled
	if ok := containsValue(mappings, "container.id"); ok {
		p = addProcessMetadata{
			config:      config,
			provider:    provider,
			cidProvider: newCidProvider(config.HostPath, config.CgroupPrefixes, processCgroupPaths),
			log:         log,
			mappings:    mappings,
		}
	} else {
		p = addProcessMetadata{
			config:   config,
			provider: provider,
			log:      log,
			mappings: mappings,
		}
	}

	return &p, nil
}

func containsValue(m common.MapStr, v string) bool {
	for _, x := range m {
		if x == v {
			return true
		}
	}
	return false
}

// Run enriches the given event with the host meta data
func (p *addProcessMetadata) Run(event *beat.Event) (*beat.Event, error) {
	for _, pidField := range p.config.MatchPIDs {
		result, err := p.enrich(event.Fields, pidField)
		if err != nil {
			switch err {
			case common.ErrKeyNotFound:
				continue
			case ErrNoProcess:
				return event, err
			default:
				return event, errors.Wrapf(err, "error applying %s processor", processorName)
			}
		}
		if result != nil {
			event.Fields = result
		}
		return event, nil
	}
	if p.config.IgnoreMissing {
		return event, nil
	}
	return event, ErrNoMatch
}

func (p *addProcessMetadata) enrich(event common.MapStr, pidField string) (result common.MapStr, err error) {
	pidIf, err := event.GetValue(pidField)
	if err != nil {
		return nil, err
	}

	var pid int
	switch v := pidIf.(type) {
	case string:
		pid, err = strconv.Atoi(v)
		if err != nil {
			return nil, errors.Wrapf(err, "cannot convert string field '%s' to an integer", pidField)
		}
	case int:
		pid = v
	default:
		return nil, errors.Errorf("cannot parse field '%s' (not an integer or string)", pidField)
	}

	metaPtr, err := p.provider.GetProcessMetadata(pid)
	if err != nil || metaPtr == nil {
		p.log.Debugf("failed to get process metadata for PID=%d: %v", pid, err)
		return nil, ErrNoProcess
	}
	meta := metaPtr.fields

	if err = p.enrichContainerID(pid, meta); err != nil {
		return nil, err
	}

	result = event.Clone()
	for dest, sourceIf := range p.mappings {
		source, castOk := sourceIf.(string)
		if !castOk {
			// Should never happen, as source is generated by Config.prepareMappings()
			return nil, errors.New("source is not a string")
		}
		if !p.config.OverwriteKeys {
			if found, _ := result.HasKey(dest); found {
				return nil, errors.Errorf("target field '%s' already exists and overwrite_keys is false", dest)
			}
		}

		value, err := meta.GetValue(source)
		if err != nil {
			// Should never happen
			return nil, err
		}

		if _, err = result.Put(dest, value); err != nil {
			return nil, err
		}
	}

	return result, nil
}

// addProcessMetadata add container.id into meta for mapping to pickup
func (p *addProcessMetadata) enrichContainerID(pid int, meta common.MapStr) error {
	if p.cidProvider == nil {
		return nil
	}
	cid, err := p.cidProvider.GetCid(pid)
	if err != nil {
		return err
	}
	if _, err = meta.Put("container", common.MapStr{"id": cid}); err != nil {
		return err
	}
	return nil
}

// String returns the processor representation formatted as a string
func (p *addProcessMetadata) String() string {
	return fmt.Sprintf("%v=[match_pids=%v, mappings=%v, ignore_missing=%v, overwrite_fields=%v, restricted_fields=%v]",
		processorName, p.config.MatchPIDs, p.mappings, p.config.IgnoreMissing,
		p.config.OverwriteKeys, p.config.RestrictedFields)
}

func (p *processMetadata) toMap() common.MapStr {
	return common.MapStr{
		"process": common.MapStr{
			"name":       p.name,
			"title":      p.title,
			"executable": p.exe,
			"args":       p.args,
			"env":        p.env,
			"pid":        p.pid,
			"ppid":       p.ppid,
			"start_time": p.startTime,
		},
	}
}
