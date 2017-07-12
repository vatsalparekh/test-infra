/*
Copyright 2017 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package options

import (
	"bytes"
	"flag"
	"fmt"
	"io/ioutil"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/ghodss/yaml"
	"github.com/golang/glog"
)

// Options represents the configuration options for mungegithub.
//
// Options are loaded from a yaml string->string configmap and are updated whenever Load is called.
// Options must be registered at least once before they can be retrieved, but registration and
// loading may happen in any order (this makes Options compatible with a plugin architecture).
// Option keys must be unique across all options of all types.
// Options may be registered multiple times safely so long as the option is always bound to the same
// pointer. (registration is idempotent)
// The defaultVal is used if the options does not have a value specified.
// The description explains the option as an entry in the text returned by Descriptions().
type Options struct {
	rawConfig []byte
	options   map[string]*option

	callbacks []UpdateCallback
}

func New() *Options {
	return &Options{options: map[string]*option{}}
}

type UpdateCallback func()

func (o *Options) RegisterUpdateCallback(callback UpdateCallback) {
	o.callbacks = append(o.callbacks, callback)
}

type optionType string

const (
	typeString      optionType = "string"
	typeStringSlice optionType = "[]string"
	typeInt         optionType = "int"
	typeUint64      optionType = "uint64"
	typeBool        optionType = "bool"
	typeDuration    optionType = "time.Duration"
	// typeSecret is the same as typeString except that values are not printed.
	typeSecret optionType = "SECRET"
	// typeUnknown is assigned to options that appear in the configmap, but are not registered.
	// Options of this type are represented as strings.
	typeUnknown optionType = "UNKNOWN"
)

type option struct {
	description string
	optType     optionType
	// val and defaultVal include a level of pointer indirection.
	// (e.g. If optType=="string", val and defaultVal are of type *string not string.)
	val        interface{}
	defaultVal interface{}

	raw string
}

// ToFlags registers all options as string flags with the flag.CommandLine flagset.
// All options should be registered before ToFlags is called.
func (o *Options) ToFlags() {
	for key, opt := range o.options {
		flag.String(key, strings.Trim(toString(opt.optType, opt.defaultVal), "\""), opt.description)
	}
}

// Load updates options based on the contents of a config file.
// If the file cannot be read and options has not been loaded previously this is fatal.
func (o *Options) Load(file string) {
	firstLoad := o.rawConfig == nil
	changed, err := o.tryLoad(file)
	if err != nil {
		if firstLoad {
			// This is fatal since we have not previously loaded a configmap.
			glog.Fatalf("Failed to load options: %v.", err)
		}
		glog.Errorf("Failed to reload options: %v.", err)
		return
	}
	if changed && !firstLoad {
		for _, callback := range o.callbacks {
			callback()
		}
	}
}

// PopulateFromString loads values from the provided yaml string into the Options struct.
// This function should only be used in tests where the config is not loaded from a file.
func (o *Options) PopulateFromString(yaml string) {
	if err := o.populateFromYaml([]byte(yaml)); err != nil {
		glog.Fatalf("Failed to populate Options with values from \"%s\". Err: %v.", yaml, err)
	}
}

// PopulateFromFlags loads values into options from command line flags.
// This function must be proceeded by a call to ToFlags and the flags must have been parsed since
// then.
func (o *Options) PopulateFromFlags() {
	if !flag.Parsed() {
		flag.Parse()
	}

	flags := map[string]string{}
	flag.Visit(func(f *flag.Flag) {
		flags[f.Name] = f.Value.String()
	})

	o.populateFromMap(flags)
}

// FlagsSpecified returns the names of the flags that were specified that correspond to options.
// This function must have been proceeded by a call to ToFlags and the flags must have been parsed
// since then.
func (o *Options) FlagsSpecified() []string {
	if !flag.Parsed() {
		flag.Parse()
	}

	specified := []string{}
	flag.Visit(func(f *flag.Flag) {
		if _, ok := o.options[f.Name]; ok {
			specified = append(specified, f.Name)
		}
	})
	return specified
}

func (o *Options) tryLoad(file string) (bool, error) {
	b, err := ioutil.ReadFile(file)
	if err != nil || b == nil {
		return false, fmt.Errorf("failed to read configmap from file '%s': %v", file, err)
	}
	if reflect.DeepEqual(o.rawConfig, b) {
		// ConfigMap has not changed so there is nothing to do.
		return false, nil
	}

	return true, o.populateFromYaml(b)
}

func (o *Options) populateFromYaml(rawCM []byte) error {
	var configmap map[string]string
	if err := yaml.Unmarshal(rawCM, &configmap); err != nil {
		return fmt.Errorf("failed to unmarshal configmap from yaml: %v", err)
	}

	o.populateFromMap(configmap)
	o.rawConfig = rawCM
	return nil
}

func (o *Options) populateFromMap(configmap map[string]string) {
	for key, opt := range o.options {
		if opt.optType == typeUnknown {
			delete(o.options, key)
			continue
		}
		if raw, ok := configmap[key]; ok {
			opt.raw = raw
			opt.fromString()
			delete(configmap, key)
		} else {
			opt.moveToVal(opt.defaultVal)
		}
	}
	for key, raw := range configmap {
		o.options[key] = &option{
			optType: typeUnknown,
			raw:     raw,
		}
	}
}

// fromString converts opt.raw to opt.optType and moves the resulting value into opt.val.
func (opt *option) fromString() {
	var err error
	var newVal interface{}
	switch opt.optType {
	case typeString, typeSecret:
		newVal = &opt.raw
	case typeStringSlice:
		slice := []string{}
		for _, raw := range strings.Split(opt.raw, ",") {
			if raw = strings.TrimSpace(raw); len(raw) > 0 {
				slice = append(slice, raw)
			}
		}
		newVal = &slice
	case typeInt:
		var i int
		if i, err = strconv.Atoi(opt.raw); err != nil {
			glog.Fatalf("Cannot convert %q to type 'int'.", opt.raw)
		}
		newVal = &i
	case typeUint64:
		var ui uint64
		if ui, err = strconv.ParseUint(opt.raw, 10, 64); err != nil {
			glog.Fatalf("Cannot convert %q to type 'uint64'.", opt.raw)
		}
		newVal = &ui
	case typeBool:
		var b bool
		if newVal, err = strconv.ParseBool(opt.raw); err != nil {
			glog.Fatalf("Cannot convert %q to type 'bool'.", opt.raw)
		}
		newVal = &b
	case typeDuration:
		var dur time.Duration
		if dur, err = time.ParseDuration(opt.raw); err != nil {
			glog.Fatalf("Cannot convert %q to type 'time.Duration'.", opt.raw)
		}
		newVal = &dur
	default:
		glog.Fatalf("Unrecognized type '%s'.", opt.optType)
	}
	opt.moveToVal(newVal)
}

// moveToVal moves the specified value to 'val', maintaining the original 'val' ptr.
func (opt *option) moveToVal(newVal interface{}) {
	switch opt.optType {
	case typeString, typeSecret:
		*opt.val.(*string) = *newVal.(*string)
	case typeStringSlice:
		*opt.val.(*[]string) = *newVal.(*[]string)
	case typeInt:
		*opt.val.(*int) = *newVal.(*int)
	case typeUint64:
		*opt.val.(*uint64) = *newVal.(*uint64)
	case typeBool:
		*opt.val.(*bool) = *newVal.(*bool)
	case typeDuration:
		*opt.val.(*time.Duration) = *newVal.(*time.Duration)
	default:
		glog.Fatalf("Unrecognized type '%s'.", opt.optType)
	}
}

// register tries to register an option of any optionType (with the exception of typeUnknown).
// register may be called before or after the configmap is loaded, but options cannot be retrieved
// until they are registered.
func (o *Options) register(optType optionType, key, description string, val, defaultVal interface{}) interface{} {
	if optType == typeUnknown {
		glog.Fatalf("Key '%s' cannot be registered as type 'typeUnknown'.", key)
	}
	opt, ok := o.options[key]
	if ok {
		if opt.optType == typeUnknown {
			// Convert opt.raw to optType.
			opt.val = val
			opt.optType = optType
			opt.defaultVal = defaultVal
			opt.description = description
			opt.fromString()
		} else if opt.optType != optType {
			glog.Fatalf(
				"Cannot register key: '%s' as a '%s'. It is already registered as a '%s'.",
				key,
				optType,
				opt.optType,
			)
		} else if opt.val != val {
			glog.Fatalf(
				"Cannot register key: '%s' to pointer %p. It is already bound to %p.",
				key,
				val,
				opt.val,
			)
		} else if description != opt.description {
			glog.Fatalf(
				"Cannot register key: '%s' with description %q. It already has description %q.",
				key,
				description,
				opt.description,
			)
		} else if !reflect.DeepEqual(defaultVal, opt.defaultVal) {
			glog.Fatalf(
				"Cannot register key: '%s' with default value %s. It already has default value %s.",
				key,
				toString(optType, defaultVal),
				toString(optType, opt.defaultVal),
			)
		}
	} else {
		opt = &option{
			description: description,
			optType:     optType,
			val:         val,
			defaultVal:  defaultVal,
		}
		o.options[key] = opt
		opt.moveToVal(defaultVal)
	}
	return opt.val
}

// RegisterString registers a `string` option under the specified key.
func (o *Options) RegisterString(ptr *string, key string, defaultVal string, description string) {
	o.register(typeString, key, description, ptr, &defaultVal)
}

// RegisterStringSlice registers a `[]string` option under the specified key.
func (o *Options) RegisterStringSlice(ptr *[]string, key string, defaultVal []string, description string) {
	*ptr = defaultVal
	o.register(typeStringSlice, key, description, ptr, &defaultVal)
}

// RegisterInt registers an `int` option under the specified key.
func (o *Options) RegisterInt(ptr *int, key string, defaultVal int, description string) {
	o.register(typeInt, key, description, ptr, &defaultVal)
}

// RegisterUint64 registers a `uint64` option under the specified key.
func (o *Options) RegisterUint64(ptr *uint64, key string, defaultVal uint64, description string) {
	o.register(typeUint64, key, description, ptr, &defaultVal)
}

// RegisterBool registers a `bool` option under the specified key.
func (o *Options) RegisterBool(ptr *bool, key string, defaultVal bool, description string) {
	o.register(typeBool, key, description, ptr, &defaultVal)
}

// RegisterDuration registers a `time.Duration` option under the specified key.
func (o *Options) RegisterDuration(ptr *time.Duration, key string, defaultVal time.Duration, description string) {
	o.register(typeDuration, key, description, ptr, &defaultVal)
}

// GetString gets the `string` option under the specified key.
func (o *Options) GetString(key string) *string {
	opt, ok := o.options[key]
	if !ok {
		glog.Fatalf("Programmer Error: option key '%s' is not registered!", key)
	}
	if opt.optType != typeString {
		glog.Fatalf("The option with key '%s' has type '%s' not '%s'.", key, opt.optType, typeString)
	}
	return o.options[key].val.(*string)
}

// GetStringSlice gets the `[]string` option under the specified key.
func (o *Options) GetStringSlice(key string) *[]string {
	opt, ok := o.options[key]
	if !ok {
		glog.Fatalf("Programmer Error: option key '%s' is not registered!", key)
	}
	if opt.optType != typeStringSlice {
		glog.Fatalf("The option with key '%s' has type '%s' not '%s'.",
			key,
			opt.optType,
			typeStringSlice,
		)
	}
	return o.options[key].val.(*[]string)
}

// GetInt gets then `int` option under the specified key.
func (o *Options) GetInt(key string) *int {
	opt, ok := o.options[key]
	if !ok {
		glog.Fatalf("Programmer Error: option key '%s' is not registered!", key)
	}
	if opt.optType != typeInt {
		glog.Fatalf("The option with key '%s' has type '%s' not '%s'.", key, opt.optType, typeInt)
	}
	return o.options[key].val.(*int)
}

// GetUint64 gets the `uint64` option under the specified key.
func (o *Options) GetUint64(key string) *uint64 {
	opt, ok := o.options[key]
	if !ok {
		glog.Fatalf("Programmer Error: option key '%s' is not registered!", key)
	}
	if opt.optType != typeUint64 {
		glog.Fatalf("The option with key '%s' has type '%s' not '%s'.", key, opt.optType, typeUint64)
	}
	return o.options[key].val.(*uint64)
}

// GetBool gets the `bool` option under the specified key.
func (o *Options) GetBool(key string) *bool {
	opt, ok := o.options[key]
	if !ok {
		glog.Fatalf("Programmer Error: option key '%s' is not registered!", key)
	}
	if opt.optType != typeBool {
		glog.Fatalf("The option with key '%s' has type '%s' not '%s'.", key, opt.optType, typeBool)
	}
	return o.options[key].val.(*bool)
}

// GetDuration gets the `time.Duration` option under the specified key.
func (o *Options) GetDuration(key string) *time.Duration {
	opt, ok := o.options[key]
	if !ok {
		glog.Fatalf("Programmer Error: option key '%s' is not registered!", key)
	}
	if opt.optType != typeDuration {
		glog.Fatalf("The option with key '%s' has type '%s' not '%s'.", key, opt.optType, typeDuration)
	}
	return o.options[key].val.(*time.Duration)
}

func toString(optType optionType, val interface{}) string {
	switch optType {
	case typeString:
		return fmt.Sprintf("%q", *val.(*string))
	case typeStringSlice:
		if len(*val.(*[]string)) == 0 {
			return "[]"
		}
		return fmt.Sprintf("[\"%s\"]", strings.Join(*val.(*[]string), "\", \""))
	case typeInt:
		return fmt.Sprintf("%d", *val.(*int))
	case typeUint64:
		return fmt.Sprintf("%d", *val.(*uint64))
	case typeBool:
		return fmt.Sprintf("%v", *val.(*bool))
	case typeDuration:
		return fmt.Sprintf("%v", *val.(*time.Duration))
	case typeSecret:
		return "<REDACTED>"
	case typeUnknown:
		return fmt.Sprintf("<UNREGISTERED> %q", val)
	default:
		glog.Fatalf("Unrecognized type '%s'.", optType)
		return ""
	}
}

func (o *Options) Descriptions() string {
	var buf bytes.Buffer
	fmt.Fprint(&buf, "The below options are available. They are listed in the format 'option: (default value) \"Description\"'.\n")
	for key, opt := range o.options {
		if opt.optType != typeUnknown {
			fmt.Fprintf(&buf, "%s: (%s) %q\n", key, toString(opt.optType, opt.defaultVal), opt.description)
		}
	}
	return buf.String()
}

func (o *Options) CurrentValues() string {
	var buf bytes.Buffer
	fmt.Fprint(&buf, "Currently configured option values:\n")
	for key, opt := range o.options {
		if opt.optType != typeUnknown {
			fmt.Fprintf(&buf, "%s: %s\n", key, toString(opt.optType, opt.val))
		}
	}
	return buf.String()
}
