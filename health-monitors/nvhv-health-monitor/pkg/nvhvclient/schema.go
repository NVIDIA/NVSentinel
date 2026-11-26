// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package nvhvclient

import (
	"fmt"
	"strconv"

	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	"github.com/nvidia/nvsentinel/health-monitors/nvhv-health-monitor/pkg/verdict"
)

// schema is NVHV's verdict schema as the monitor reads it: the Evaluate method
// and the fields it needs, found once and checked against what NVHV sent.
type schema struct {
	method            string
	request, response protoreflect.MessageDescriptor

	xidFrom, xidTo   protoreflect.FieldDescriptor // the request's Xid window
	version, results protoreflect.FieldDescriptor
	check            checkFields
	action           actionFields
	valueString      protoreflect.FieldDescriptor // string_value of google.protobuf.Value
}

type checkFields struct {
	id, severity, category, skipped, messages, action, entities, observed protoreflect.FieldDescriptor
}

type actionFields struct {
	state, steps protoreflect.FieldDescriptor
}

// severities are the enum values the monitor's rules are written for.
var severities = []protoreflect.Name{"PASS", "INFO", "WARNING", "UNKNOWN", "CRITICAL"}

// newSchema finds the Evaluate method and every field the monitor reads, and
// fails with ErrIncompatibleSchema if one is missing or has another type.
func newSchema(files *protoregistry.Files) (*schema, error) {
	d, err := files.FindDescriptorByName(serviceName)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrIncompatibleSchema, err)
	}

	svc, ok := d.(protoreflect.ServiceDescriptor)
	if !ok {
		return nil, fmt.Errorf("%w: %s is not a service", ErrIncompatibleSchema, serviceName)
	}

	m := svc.Methods().ByName("Evaluate")
	if m == nil {
		return nil, fmt.Errorf("%w: %s has no Evaluate method", ErrIncompatibleSchema, serviceName)
	}

	s := &schema{
		method:   "/" + string(svc.FullName()) + "/" + string(m.Name()),
		request:  m.Input(),
		response: m.Output(),
	}

	if err := s.bind(); err != nil {
		return nil, err
	}

	return s, nil
}

// bind looks the fields up, stopping at the first problem.
func (s *schema) bind() error {
	var err error

	f := func(md protoreflect.MessageDescriptor, name string, kind protoreflect.Kind, list bool) protoreflect.FieldDescriptor {
		if err != nil {
			return nil
		}

		fd := md.Fields().ByName(protoreflect.Name(name))
		if fd == nil || fd.Kind() != kind || fd.IsList() != list || fd.IsMap() {
			err = fmt.Errorf("%w: %s.%s must be a %s%s", ErrIncompatibleSchema, md.FullName(), name, listWord(list), kind)

			return nil
		}

		return fd
	}

	s.xidFrom = f(s.request, "xid_from", protoreflect.Int64Kind, false)
	s.xidTo = f(s.request, "xid_to", protoreflect.Int64Kind, false)
	s.version = f(s.response, "schema_version", protoreflect.StringKind, false)
	s.results = f(s.response, "check_results", protoreflect.MessageKind, true)

	if err != nil {
		return err
	}

	cm := s.results.Message()
	s.check = checkFields{
		id:       f(cm, "check_id", protoreflect.StringKind, false),
		severity: f(cm, "severity", protoreflect.EnumKind, false),
		category: f(cm, "category", protoreflect.EnumKind, false),
		skipped:  f(cm, "skipped", protoreflect.BoolKind, false),
		messages: f(cm, "messages", protoreflect.StringKind, true),
		action:   f(cm, "recommended_action", protoreflect.MessageKind, false),
		entities: f(cm, "affected_entities", protoreflect.StringKind, true),
	}

	if err != nil {
		return err
	}

	s.check.observed = cm.Fields().ByName("observed_values")
	if s.check.observed == nil || !s.check.observed.IsMap() || s.check.observed.MapValue().Kind() != protoreflect.MessageKind {
		return fmt.Errorf("%w: %s.observed_values must be a map of messages", ErrIncompatibleSchema, cm.FullName())
	}

	am := s.check.action.Message()
	s.action = actionFields{
		state: f(am, "operational_state", protoreflect.EnumKind, false),
		steps: f(am, "remediation_actions", protoreflect.StringKind, true),
	}

	if err != nil {
		return err
	}

	for _, name := range severities {
		if s.check.severity.Enum().Values().ByName(name) == nil {
			return fmt.Errorf("%w: severity enum has no %s", ErrIncompatibleSchema, name)
		}
	}

	// A value without string_value just has no observed strings.
	s.valueString = s.check.observed.MapValue().Message().Fields().ByName("string_value")

	return nil
}

func listWord(list bool) string {
	if list {
		return "repeated "
	}

	return ""
}

// decode reads a reply into plain verdict values.
func (s *schema) decode(m protoreflect.Message) *verdict.Verdict {
	v := &verdict.Verdict{SchemaVersion: m.Get(s.version).String()}

	list := m.Get(s.results).List()
	for i := range list.Len() {
		v.Results = append(v.Results, s.decodeResult(list.Get(i).Message()))
	}

	return v
}

func (s *schema) decodeResult(m protoreflect.Message) verdict.Result {
	c := s.check

	r := verdict.Result{
		CheckID:          m.Get(c.id).String(),
		Severity:         verdict.Severity(enumName(m, c.severity)),
		Category:         enumName(m, c.category),
		Skipped:          m.Get(c.skipped).Bool(),
		Messages:         strings(m.Get(c.messages).List()),
		AffectedEntities: strings(m.Get(c.entities).List()),
		ObservedStrings:  s.observedStrings(m.Get(c.observed).Map()),
	}

	if m.Has(c.action) {
		am := m.Get(c.action).Message()
		r.OperationalState = enumName(am, s.action.state)
		r.RemediationActions = strings(am.Get(s.action.steps).List())
	}

	return r
}

// observedStrings keeps the observed values that are strings.
func (s *schema) observedStrings(m protoreflect.Map) map[string]string {
	if s.valueString == nil {
		return nil
	}

	var out map[string]string

	m.Range(func(k protoreflect.MapKey, v protoreflect.Value) bool {
		vm := v.Message()
		if vm.Has(s.valueString) {
			if out == nil {
				out = map[string]string{}
			}

			out[k.String()] = vm.Get(s.valueString).String()
		}

		return true
	})

	return out
}

// enumName is the name of an enum field's value, or its number if NVHV sent a
// value this schema does not name.
func enumName(m protoreflect.Message, fd protoreflect.FieldDescriptor) string {
	n := m.Get(fd).Enum()
	if v := fd.Enum().Values().ByNumber(n); v != nil {
		return string(v.Name())
	}

	return strconv.Itoa(int(n))
}

func strings(l protoreflect.List) []string {
	var out []string

	for i := range l.Len() {
		out = append(out, l.Get(i).String())
	}

	return out
}
