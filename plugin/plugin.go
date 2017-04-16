// +build go1.5,cgo

/*
Package plugin exports the functions required to write collectd plugins in Go.

This package provides the abstraction necessary to write plugins for collectd
in Go, compile them into a shared object and let the daemon load and use them.

Example plugin

To understand how this module is being used, please consider the following
example:

  package main

  import (
	  "time"

	  "collectd.org/api"
	  "collectd.org/plugin"
  )

  type ExamplePlugin struct{}

  func (*ExamplePlugin) Read() error {
	  vl := &api.ValueList{
		  Identifier: api.Identifier{
			  Host:   "example.com",
			  Plugin: "goplug",
			  Type:   "gauge",
		  },
		  Time:     time.Now(),
		  Interval: 10 * time.Second,
		  Values:   []api.Value{api.Gauge(42)},
		  DSNames:  []string{"value"},
	  }
	  if err := plugin.Write(context.Background(), vl); err != nil {
		  return err
	  }

	  return nil
  }

  func init() {
	  plugin.RegisterRead("example", &ExamplePlugin{})
  }

  func main() {} // ignored

The first step when writing a new plugin with this package, is to create a new
"main" package. Even though it has to have a main() function to make cgo happy,
the main() function is ignored. Instead, put your startup code into the init()
function which essentially takes on the same role as the module_register()
function in C based plugins.

Then, define a type which implements the Reader interface by implementing the
"Read() error" function. In the example above, this type is called
ExamplePlugin. Create an instance of this type and pass it to RegisterRead() in
the init() function.

Build flags

To compile your plugin, set up the CGO_CPPFLAGS environment variable and call
"go build" with the following options:

  export COLLECTD_SRC="/path/to/collectd"
  export CGO_CPPFLAGS="-I${COLLECTD_SRC}/src/daemon -I${COLLECTD_SRC}/src"
  go build -buildmode=c-shared -o example.so
*/
package plugin // import "collectd.org/plugin"

// #cgo CPPFLAGS: -DHAVE_CONFIG_H
// #cgo LDFLAGS: -ldl
// #include <stdlib.h>
// #include <dlfcn.h>
// #include "plugin.h"
//
// int dispatch_values_wrapper (value_list_t const *vl);
// int register_read_wrapper (char const *group, char const *name,
//     plugin_read_cb callback,
//     cdtime_t interval,
//     user_data_t *ud);
//
// data_source_t *ds_dsrc(data_set_t const *ds, size_t i);
//
// void value_list_add_counter (value_list_t *, counter_t);
// void value_list_add_derive  (value_list_t *, derive_t);
// void value_list_add_gauge   (value_list_t *, gauge_t);
// counter_t value_list_get_counter (value_list_t *, size_t);
// derive_t  value_list_get_derive  (value_list_t *, size_t);
// gauge_t   value_list_get_gauge   (value_list_t *, size_t);
//
// typedef int (*plugin_complex_config_cb)(oconfig_item_t);
//
// int wrap_read_callback(user_data_t *);
//
// int register_write_wrapper (char const *, plugin_write_cb, user_data_t *);
// int wrap_write_callback(data_set_t *, value_list_t *, user_data_t *);
//
// int register_shutdown_wrapper (char *, plugin_shutdown_cb);
// int wrap_shutdown_callback(void);
//
// int register_complex_config_wrapper (char *, plugin_complex_config_cb);
// int process_complex_config(oconfig_item_t*);
// char *process_string_value(oconfig_item_t*, int);
import "C"

import (
	"collectd.org/api"
	"collectd.org/cdtime"
	"context"
	"fmt"
	"reflect"
	"unsafe"
)

var (
	ctx = context.Background()
)

// Reader defines the interface for read callbacks, i.e. Go functions that are
// called periodically from the collectd daemon.
type Reader interface {
	Read() error
}

func strcpy(dst []C.char, src string) {
	byteStr := []byte(src)
	cStr := make([]C.char, len(byteStr)+1)

	for i, b := range byteStr {
		cStr[i] = C.char(b)
	}
	cStr[len(cStr)-1] = C.char(0)

	copy(dst, cStr)
}

func newValueListT(vl *api.ValueList) (*C.value_list_t, error) {
	ret := &C.value_list_t{}

	strcpy(ret.host[:], vl.Host)
	strcpy(ret.plugin[:], vl.Plugin)
	strcpy(ret.plugin_instance[:], vl.PluginInstance)
	strcpy(ret._type[:], vl.Type)
	strcpy(ret.type_instance[:], vl.TypeInstance)
	ret.interval = C.cdtime_t(cdtime.NewDuration(vl.Interval))
	ret.time = C.cdtime_t(cdtime.New(vl.Time))

	for _, v := range vl.Values {
		switch v := v.(type) {
		case api.Counter:
			if _, err := C.value_list_add_counter(ret, C.counter_t(v)); err != nil {
				return nil, fmt.Errorf("value_list_add_counter: %v", err)
			}
		case api.Derive:
			if _, err := C.value_list_add_derive(ret, C.derive_t(v)); err != nil {
				return nil, fmt.Errorf("value_list_add_derive: %v", err)
			}
		case api.Gauge:
			if _, err := C.value_list_add_gauge(ret, C.gauge_t(v)); err != nil {
				return nil, fmt.Errorf("value_list_add_gauge: %v", err)
			}
		default:
			return nil, fmt.Errorf("not yet supported: %T", v)
		}
	}

	return ret, nil
}

// writer implements the api.Write interface.
type writer struct{}

// NewWriter returns an object implementing the api.Writer interface for the
// collectd daemon.
func NewWriter() api.Writer {
	return writer{}
}

// Write implements the api.Writer interface for the collectd daemon.
func (writer) Write(_ context.Context, vl *api.ValueList) error {
	return Write(vl)
}

// Write converts a ValueList and calls the plugin_dispatch_values() function
// of the collectd daemon.
func Write(vl *api.ValueList) error {
	vlt, err := newValueListT(vl)
	if err != nil {
		return err
	}
	defer C.free(unsafe.Pointer(vlt.values))

	status, err := C.dispatch_values_wrapper(vlt)
	if err != nil {
		return err
	} else if status != 0 {
		return fmt.Errorf("dispatch_values failed with status %d", status)
	}

	return nil
}

// readFuncs holds references to all read callbacks, so the garbage collector
// doesn't get any funny ideas.
var readFuncs = make(map[string]Reader)

// RegisterRead registers a new read function with the daemon which is called
// periodically.
func RegisterRead(name string, r Reader) error {
	cGroup := C.CString("golang")
	defer C.free(unsafe.Pointer(cGroup))

	cName := C.CString(name)
	ud := C.user_data_t{
		data:      unsafe.Pointer(cName),
		free_func: nil,
	}

	status, err := C.register_read_wrapper(cGroup, cName,
		C.plugin_read_cb(C.wrap_read_callback),
		C.cdtime_t(0),
		&ud)
	if err != nil {
		return err
	} else if status != 0 {
		return fmt.Errorf("register_read_wrapper failed with status %d", status)
	}

	readFuncs[name] = r
	return nil
}

//export wrap_read_callback
func wrap_read_callback(ud *C.user_data_t) C.int {
	name := C.GoString((*C.char)(ud.data))
	r, ok := readFuncs[name]
	if !ok {
		return -1
	}

	if err := r.Read(); err != nil {
		Errorf("%s plugin: Read() failed: %v", name, err)
		return -1
	}

	return 0
}

// writeFuncs holds references to all write callbacks, so the garbage collector
// doesn't get any funny ideas.
var writeFuncs = make(map[string]api.Writer)

// RegisterWrite registers a new write function with the daemon which is called
// for every metric collected by collectd.
//
// Please note that multiple threads may call this function concurrently. If
// you're accessing shared resources, such as a memory buffer, you have to
// implement appropriate locking around these accesses.
func RegisterWrite(name string, w api.Writer) error {
	cName := C.CString(name)
	ud := C.user_data_t{
		data:      unsafe.Pointer(cName),
		free_func: nil,
	}

	status, err := C.register_write_wrapper(cName, C.plugin_write_cb(C.wrap_write_callback), &ud)
	if err != nil {
		return err
	} else if status != 0 {
		return fmt.Errorf("register_write_wrapper failed with status %d", status)
	}

	writeFuncs[name] = w
	return nil
}

//export wrap_write_callback
func wrap_write_callback(ds *C.data_set_t, cvl *C.value_list_t, ud *C.user_data_t) C.int {
	name := C.GoString((*C.char)(ud.data))
	w, ok := writeFuncs[name]
	if !ok {
		return -1
	}

	vl := &api.ValueList{
		Identifier: api.Identifier{
			Host:           C.GoString(&cvl.host[0]),
			Plugin:         C.GoString(&cvl.plugin[0]),
			PluginInstance: C.GoString(&cvl.plugin_instance[0]),
			Type:           C.GoString(&cvl._type[0]),
			TypeInstance:   C.GoString(&cvl.type_instance[0]),
		},
		Time:     cdtime.Time(cvl.time).Time(),
		Interval: cdtime.Time(cvl.interval).Duration(),
	}

	// TODO: Remove 'size_t' cast on 'ds_num' upon 5.7 release.
	for i := C.size_t(0); i < C.size_t(ds.ds_num); i++ {
		dsrc := C.ds_dsrc(ds, i)

		switch dsrc._type {
		case C.DS_TYPE_COUNTER:
			v := C.value_list_get_counter(cvl, i)
			vl.Values = append(vl.Values, api.Counter(v))
		case C.DS_TYPE_DERIVE:
			v := C.value_list_get_derive(cvl, i)
			vl.Values = append(vl.Values, api.Derive(v))
		case C.DS_TYPE_GAUGE:
			v := C.value_list_get_gauge(cvl, i)
			vl.Values = append(vl.Values, api.Gauge(v))
		default:
			Errorf("%s plugin: data source type %d is not supported", name, dsrc._type)
			return -1
		}

		vl.DSNames = append(vl.DSNames, C.GoString(&dsrc.name[0]))
	}

	if err := w.Write(ctx, vl); err != nil {
		Errorf("%s plugin: Write() failed: %v", name, err)
		return -1
	}

	return 0
}

// First declare some types, interfaces, general functions

// Shutters are objects that when called will shut down the plugin gracefully
type Shutter interface {
	Shutdown() error
}

// shutdownFuncs holds references to all shutdown callbacks
var shutdownFuncs = make(map[string]Shutter)

//export wrap_shutdown_callback
func wrap_shutdown_callback() C.int {
	fmt.Println("wrap_shutdown_callback called")
	if len(shutdownFuncs) <= 0 {
		return 0
	}
	for n, s := range shutdownFuncs {
		if err := s.Shutdown(); err != nil {
			Errorf("%s plugin: Shutdown() failed: %v", n, s)
			return -1
		}
	}
	return 0
}

// RegisterShutdown registers a shutdown function with the daemon which is called
// when the plugin is required to shutdown gracefully.
func RegisterShutdown(name string, s Shutter) error {
	// Only register a callback the first time one is implemented, subsequent
	// callbacks get added to a list and called at the same time
	if len(shutdownFuncs) <= 0 {
		cName := C.CString(name)
		cCallback := C.plugin_shutdown_cb(C.wrap_shutdown_callback)

		status, err := C.register_shutdown_wrapper(cName, cCallback)
		if err != nil {
			Errorf("register_shutdown_wrapper failed with status: %v", status)
			return err
		}
	}
	shutdownFuncs[name] = s
	return nil
}

// Configuration defines the interface for complex_configure callbacks, i.e. Go
// structs that can hold and validate a configuration
type Configuration interface {
	Validate() error
}

var config_target Configuration
var Configured chan struct{}

// TODO: wrap the received pointer

// process_complex_config handles the actual translation of an oconfig_item_t
//export process_complex_config
func process_complex_config(oconfig *C.oconfig_item_t) C.int {

	fmt.Println("process_complex_config called")
	alignConfig(oconfig, config_target)
	return C.int(0)

}

// alignConfig takes an oconfig_item_t struct and a Go struct and attempts to
// line them up, calling itself recursively as required.
func alignConfig(oconfig *C.oconfig_item_t, target interface{}) error {
	fmt.Println("alignConfig called")
	fmt.Printf("Starting with %v children and %v values", oconfig.children_num, oconfig.values_num)

	t := reflect.ValueOf(target)

	if oconfig.values_num > 1 { // multiple values, need a slice
		fmt.Println("multiple values")
		key := C.GoString(oconfig.key)
		if t.Elem().FieldByName(key).Kind() == reflect.Slice {
			// TODO: handle a slice of values
		} else {
			// We received multiple values, but the supplied config struct can only
			// handle one here
			return fmt.Errorf("received multiple values but didn't have a slice to put them in")
		}
		//} else if oconfig.values_num == 1 { // single value
		//	fmt.Println("single value")
		//	return nil
	} else if oconfig.children_num > 0 { // a container of children
		fmt.Println(oconfig.values_num)
		fmt.Printf("\nfound structish with %T %v children\n\n", oconfig.children_num, oconfig.children_num)
		// two ways that we could emulate C pointer arithmetic, this one works at least
		start := unsafe.Pointer(oconfig.children)
		size := unsafe.Sizeof(*oconfig.children)

		for i := 0; i < int(oconfig.children_num); i++ {
			child := *(*C.oconfig_item_t)(unsafe.Pointer(uintptr(start) + size*uintptr(i)))
			fmt.Printf("Child number %v is named %v, has %v values and %v children\n", i, C.GoString(child.key), child.values_num, child.children_num)

		}
		return nil
	}
	fmt.Println("nothing matched?")
	return nil
}

// RequestConfiguration registers a Configuration struct with the daemon and
// requests a callback to fill it in. It returns a channel which will be
// closed when a configuration has been loaded
func RequestConfiguration(name string, c Configuration) (Configured chan struct{}, err error) {
	// TODO: consider any possible sanity checking that could apply here
	config_target = c
	cName := C.CString(name)
	cCallback := C.plugin_complex_config_cb(C.process_complex_config)
	status, err := C.register_complex_config_wrapper(cName, cCallback)
	if err != nil {
		Errorf("register_complex_config_wrapper failed with status %v", status)
		return nil, err
	}
	Configured = make(chan struct{})
	fmt.Println("Config Requsted")
	return Configured, nil
}

//export module_register
func module_register() {
}
