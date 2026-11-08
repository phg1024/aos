package agentflow

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
)

// DecodeWorkflowSpec decodes a workflow spec strictly: a field the spec
// format does not have is refused, anywhere in the spec, with its path and
// the nearest known name ("agents[1].max_step: unknown field; did you mean
// max_steps?"), rather than dropped. A per-agent model, profile or backend
// is refused as not supported yet, and an explicit "max_steps": 0, which
// would read as the default, is refused too. Every place that reads a spec
// a person wrote decodes it here; a spec AOS recorded (run_spec) is read as
// written.
func DecodeWorkflowSpec(raw []byte) (WorkflowSpec, error) {
	var spec WorkflowSpec
	if err := json.Unmarshal(raw, &spec); err != nil {
		return WorkflowSpec{}, fmt.Errorf("%w: %v", ErrWorkflowSpecInvalid, err)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var generic any
	if err := decoder.Decode(&generic); err != nil {
		return WorkflowSpec{}, fmt.Errorf("%w: %v", ErrWorkflowSpecInvalid, err)
	}
	var problems []string
	checkFields(generic, reflect.TypeOf(spec), "", &problems)
	if len(problems) > 0 {
		return WorkflowSpec{}, fmt.Errorf("%w: %s", ErrWorkflowSpecInvalid, strings.Join(problems, "; "))
	}
	return spec, nil
}

// perAgentModelFields are the fields a spec might use to give an agent its
// own model, which AOS does not support yet.
var perAgentModelFields = map[string]bool{"model": true, "profile": true, "backend": true}

// checkFields appends to problems every key of value that type t has no
// field for, recursing through structs, slices, arrays and maps. Keys match
// fields as encoding/json matches them, ignoring case.
func checkFields(value any, t reflect.Type, path string, problems *[]string) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.Struct:
		object, ok := value.(map[string]any)
		if !ok {
			return
		}
		fields := jsonFields(t)
		keys := make([]string, 0, len(object))
		for key := range object {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			where := joinPath(path, key)
			field, known := lookupField(fields, key)
			switch {
			case !known && t == reflect.TypeOf(AgentSpec{}) && perAgentModelFields[strings.ToLower(key)]:
				*problems = append(*problems, where+": per-agent models are not supported yet; every agent runs on the run's backend (aos run --profile, or the AOS_LLM_* settings)")
			case !known:
				*problems = append(*problems, where+": unknown field; "+suggest(key, fields))
			case t == reflect.TypeOf(AgentSpec{}) && field.name == "max_steps" && isZero(object[key]):
				*problems = append(*problems, where+": 0 is not a step limit; leave max_steps out for the default of 24, or give 1 to 1000")
			default:
				checkFields(object[key], field.typ, where, problems)
			}
		}
	case reflect.Slice, reflect.Array:
		items, ok := value.([]any)
		if !ok {
			return
		}
		for i, item := range items {
			checkFields(item, t.Elem(), fmt.Sprintf("%s[%d]", path, i), problems)
		}
	case reflect.Map:
		object, ok := value.(map[string]any)
		if !ok {
			return
		}
		for key, item := range object {
			checkFields(item, t.Elem(), joinPath(path, key), problems)
		}
	}
}

type jsonField struct {
	name string
	typ  reflect.Type
}

// jsonFields are t's fields by their JSON names, embedded structs' fields
// included.
func jsonFields(t reflect.Type) []jsonField {
	var fields []jsonField
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		if !field.IsExported() && !field.Anonymous {
			continue
		}
		tag := field.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		if field.Anonymous && name == "" {
			embedded := field.Type
			if embedded.Kind() == reflect.Pointer {
				embedded = embedded.Elem()
			}
			if embedded.Kind() == reflect.Struct {
				fields = append(fields, jsonFields(embedded)...)
				continue
			}
		}
		if name == "" {
			name = field.Name
		}
		fields = append(fields, jsonField{name: name, typ: field.Type})
	}
	return fields
}

func lookupField(fields []jsonField, key string) (jsonField, bool) {
	for _, field := range fields {
		if field.name == key {
			return field, true
		}
	}
	for _, field := range fields {
		if strings.EqualFold(field.name, key) {
			return field, true
		}
	}
	return jsonField{}, false
}

func joinPath(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

func isZero(value any) bool {
	number, ok := value.(json.Number)
	if !ok {
		return false
	}
	f, err := number.Float64()
	return err == nil && f == 0
}

// suggest names the known field nearest key, or lists them all when none
// is near: within a third of its length in edits, at least two.
func suggest(key string, fields []jsonField) string {
	best, bestDistance := "", -1
	for _, field := range fields {
		distance := editDistance(strings.ToLower(key), strings.ToLower(field.name))
		if bestDistance < 0 || distance < bestDistance {
			best, bestDistance = field.name, distance
		}
	}
	if best != "" && bestDistance <= max(2, len(key)/3) {
		return "did you mean " + best + "?"
	}
	names := make([]string, 0, len(fields))
	for _, field := range fields {
		names = append(names, field.name)
	}
	return "known fields here: " + strings.Join(names, ", ")
}

// editDistance is the Levenshtein distance between a and b, in bytes.
func editDistance(a, b string) int {
	previous := make([]int, len(b)+1)
	current := make([]int, len(b)+1)
	for j := range previous {
		previous[j] = j
	}
	for i := 1; i <= len(a); i++ {
		current[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			current[j] = min(previous[j]+1, current[j-1]+1, previous[j-1]+cost)
		}
		previous, current = current, previous
	}
	return previous[len(b)]
}
