// Copyright 2022 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package weaver

import (
	"crypto/tls"
	"net"
	"sync"

	"github.com/ServiceWeaver/weaver/internal/register"
	"github.com/ServiceWeaver/weaver/runtime/codegen"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/exp/slog"
)

// Glossary of component related types:
//  - codegen.Registration: information about an available component.
//    Created in code generated by "weaver generate".
//  - component: created when a component is first requested. Contains
//    a componentImpl if the component is local, a componentStub if it
//    is remote.
//  - componentImpl: created when a component is instantiated in this process.
//  - Instance: common interface implemented by all local component implementations.
//    Implemented by componentImpl.
//  - componentStub: proxies method calls to remote componentImpls.

// Instance is the interface implemented by all component implementations
// (by virtue of [weaver.Implements] being embedded inside the component implementation).
// An Instance for a particular component only exists in processes that are hosting
// that component.
type Instance interface {
	// Logger returns a logger that associates its log entries with this component.
	Logger() *slog.Logger

	// rep is for internal use.
	rep() *component
}

// InstanceOf[T] is the interface implemented by a struct that embeds
// weaver.Implements[T].
type InstanceOf[T any] interface {
	Instance
	implements(T)
}

// componentImpl is a fully instantiated Service Weaver component that is running locally on
// this process. If the component's method is invoked from the local process,
// a local stub is used; otherwise, a server stub is used.
type componentImpl struct {
	component  *component     // passed to component constructor
	impl       interface{}    // user implementation of component
	serverStub codegen.Server // handles calls from other processes
}

// component represents a Service Weaver component and all corresponding metadata.
type component struct {
	wlet      *weavelet             // read-only, once initialized
	info      *codegen.Registration // read-only, once initialized
	clientTLS *tls.Config           // read-only, once initialized

	registerInit sync.Once // used to register the component
	registerErr  error     // non-nil if registration fails

	implInit sync.Once      // used to initialize impl, logger
	implErr  error          // non-nil if impl creation fails
	impl     *componentImpl // only ever non-nil if this component is local
	logger   *slog.Logger   // read-only after implInit.Do()
	tracer   trace.Tracer   // read-only after implInit.Do()

	// TODO(mwhittaker): We have one client for every component. Every client
	// independently maintains network connections to every weavelet hosting
	// the component. Thus, there may be many redundant network connections to
	// the same weavelet. Given n weavelets hosting m components, there's at
	// worst n^2m connections rather than a more optimal n^2 (a single
	// connection between every pair of weavelets). We should rewrite things to
	// avoid the redundancy.
	clientInit sync.Once // used to initialize client
	client     *client   // only evern non-nil if this component is remote or routed

	stubInit sync.Once // used to initialize stub
	stubErr  error     // non-nil if stub creation fails
	stub     *stub     // only ever non-nil if this component is remote or routed

	local register.WriteOnce[bool] // routed locally?
	load  *loadCollector           // non-nil for routed components
}

var _ Instance = &componentImpl{}

// Main is interface implemented by an application's main component.
type Main interface{}

// Implements[T] is a type that can be embedded inside a component implementation
// struct to indicate that the struct implements a component of type T. E.g.,
// consider a Cache component.
//
//	type Cache interface {
//		Get(ctx context.Context, key string) (string, error)
//		Put(ctx context.Context, key, value string) error
//	}
//
// A concrete type that implements the Cache component will be marked as follows:
//
//	type lruCache struct {
//		weaver.Implements[Cache]
//		...
//	}
//
// Implements is embedded inside the component implementation, and therefore
// methods of Implements (as well as methods of [weaver.Instance]) are available
// as methods of the implementation type and can be invoked directly on an
// implementation type instance.
type Implements[T any] struct {
	*componentImpl

	// Given a component implementation type, there is currently no nice way,
	// using reflection, to get the corresponding component interface type [1].
	// The component_interface_type field exists to make it possible.
	//
	// [1]: https://github.com/golang/go/issues/54393.
	component_interface_type T //nolint:unused
}

// setInstance is used during component initialization to fill Implements.component.
func (i *Implements[T]) setInstance(c *componentImpl) { i.componentImpl = c }

// implements is a method that can only be implemented inside the weaver package.
//
//nolint:unused
func (i *Implements[T]) implements(T) {}

func (*componentImpl) routedBy(if_youre_seeing_this_you_probably_forgot_to_run_weaver_generate) {}

var _ Unrouted = (*componentImpl)(nil)

// Ref[T] is a field that can be placed inside a component implementation
// struct. T must be a component type. Service Weaver will automatically
// fill such a field with a handle to the corresponding component.
type Ref[T any] struct {
	value T
}

// Get returns a handle to the component of type T.
func (r Ref[T]) Get() T { return r.value }

// isRef is an internal interface that is only implemented by Ref[T] and is
// used by the implementation to check that a value is of type Ref[T].
func (r Ref[T]) isRef() {}

// Listener is a network listener that can be placed as a field inside a
// component implementation struct. Once placed, Service Weaver automatically
// initializes the Listener and makes it suitable for receiving network traffic.
// For example:
//
//	type myComponentImpl struct {
//	  weaver.Implements[MyComponent]
//	  myListener      weaver.Listener
//	  myOtherListener weaver.Listener
//	}
//
// By default, all listeners listen on address ":0". This behavior can be
// modified by passing options for individual listeners in the application
// config. For example, to specify local addresses for the above two listeners,
// the user can add the following lines to the application config file:
//
//	[listeners]
//	myListener      = {local_address = "localhost:9000"}
//	myOtherListener = {local_address = "localhost:9001"}
//
// Listeners are identified by their field names in the component implementation
// structs (e.g., myListener and myOtherListener).
// If the user wishes to assign different names to their listeners, they may do
// so by adding a `weaver:"name"` struct tag to their listener fields, e.g.:
//
//	type myComponentImpl struct {
//	  weaver.Implements[MyComponent]
//	  myListener      weaver.Listener
//	  myOtherListener weaver.Listener `weaver:"mylistener2"`
//	}
//
// Listener names must be unique inside a given application binary, regardless
// of which components they are specified in. For example, it is illegal to
// declare a Listener field "foo" in two different component implementation
// structs, unless one is renamed using the `weaver:"name"` struct tag.
//
// HTTP servers constructed using this listener are expected to perform
// health checks on the reserved HealthzURL path. (Note that this
// URL path is configured to never receive any user traffic.)
//
// [1] https://en.wikipedia.org/wiki/Domain_name
type Listener struct {
	net.Listener        // underlying listener
	proxyAddr    string // address of proxy that forwards to the listener
}

// isListener is an internal interface that is only implemented by Listener and
// is used by the implementation to check that a value is of type Listener.
func (l Listener) isListener() {}

// String returns the address clients should dial to connect to the
// listener; this will be the proxy address if available, otherwise
// the <host>:<port> for this listener.
func (l Listener) String() string {
	if l.proxyAddr != "" {
		return l.proxyAddr
	}
	return l.Addr().String()
}

// ProxyAddr returns the dialable address of the proxy that forwards traffic to
// this listener, or returns the empty string if there is no such proxy.
func (l *Listener) ProxyAddr() string {
	return l.proxyAddr
}

func (c *componentImpl) rep() *component { return c.component }

// Logger returns a logger that associates its log entries with this component.
func (c *componentImpl) Logger() *slog.Logger { return c.component.logger }

// WithRouter[T] is a type that can be embedded inside a component implementation
// struct to indicate that calls to a method M on the component must be routed according
// to the the value returned by T.M().
//
// # An Example
//
// For example, consider a Cache component that maintains an in-memory cache.
//
//	type Cache interface {
//	    Get(ctx context.Context, key string) (string, error)
//	    Put(ctx context.Context, key, value string) error
//	}
//
// We can create a router for the Cache component like this.
//
//	type cacheRouter struct{}
//	func (cacheRouter) Get(_ context.Context, key string) string { return key }
//	func (cacheRouter) Put(_ context.Context, key, value string) string { return key }
//
// To associate a router with its component, embed [weaver.WithRouter] in the component
// implementation.
//
//	type lruCache struct {
//		weaver.Implements[Cache]
//		weaver.WithRouter[cacheRouter]
//	}
//
// For every component method that needs to be routed (e.g., Get and Put), the
// associated router should implement an equivalent method (i.e., same name and
// argument types) whose return type is the routing key. When a component's
// routed method is invoked, its corresponding router method is invoked to
// produce a routing key. Method invocations that produce the same key are
// routed to the same replica.
//
// # Routing Keys
//
// A routing key can be any integer (e.g., int, int32), float (i.e. float32,
// float64), or string; or a struct that may optionaly embed the
// weaver.AutoMarshal struct and rest of the fields must be either integers,
// floats, or strings (e.g., struct{weaver.AutoMarshal; x int; y string},
// struct{x int; y string}).
// Every router method must return the same routing key type. The following,
// for example, is invalid:
//
//	// ERROR: Get returns a string, but Put returns an int.
//	func (cacheRouter) Get(_ context.Context, key string) string { return key }
//	func (cacheRouter) Put(_ context.Context, key, value string) int { return 42 }
//
// # Semantics
//
// NOTE that routing is done on a best-effort basis. Service Weaver will try to route
// method invocations with the same key to the same replica, but this is not
// guaranteed. As a corollary, you should never depend on routing for
// correctness. Only use routing to increase performance in the common case.
type WithRouter[T any] struct{}

//nolint:unused
func (WithRouter[T]) routedBy(T) {}

// RoutedBy[T] is the interface implemented by a struct that embeds
// weaver.RoutedBy[T].
type RoutedBy[T any] interface {
	routedBy(T)
}

// Unrouted is the interface implemented by instances that don't embed
// weaver.WithRouter[T].
type Unrouted interface {
	routedBy(if_youre_seeing_this_you_probably_forgot_to_run_weaver_generate)
}

type if_youre_seeing_this_you_probably_forgot_to_run_weaver_generate struct{}

// AutoMarshal is a type that can be embedded within a struct to indicate that
// "weaver generate" should generate serialization methods for the struct.
//
// Named struct types are not serializable by default. However, they can
// trivially be made serializable by embedding AutoMarshal. For example:
//
//	type Pair struct {
//	    weaver.AutoMarshal
//	    x, y int
//	}
//
// The AutoMarshal embedding instructs "weaver generate" to generate
// serialization methods for the struct, Pair in this example.
//
// Note, however, that AutoMarshal cannot magically make any type serializable.
// For example, "weaver generate" will raise an error for the following code
// because the NotSerializable struct is fundamentally not serializable.
//
//	// ERROR: NotSerializable cannot be made serializable.
//	type NotSerializable struct {
//	    weaver.AutoMarshal
//	    f func()   // functions are not serializable
//	    c chan int // chans are not serializable
//	}
type AutoMarshal struct{}

// TODO(mwhittaker): The following methods have AutoMarshal implement
// codegen.AutoMarshal. Alternatively, we could modify the code generator to
// ignore AutoMarshal during marshaling and unmarshaling.

func (AutoMarshal) WeaverMarshal(*codegen.Encoder)   {}
func (AutoMarshal) WeaverUnmarshal(*codegen.Decoder) {}

// WithConfig[T] is a type that can be embedded inside a component
// implementation. Service Weaver runtime will take per-component configuration
// information found in the application config file and use it
// to initialize the contents of T.
//
// For example: consider a cache component where the cache size should be configurable.
// Define a struct that includes the size, associate it with the component
// implementation, and use it inside the component methods.
//
//	type cacheConfig struct
//	    Size int
//	}
//
//	type cache struct {
//	    weaver.Implements[...]
//	    weaver.WithConfig[cacheConfig]
//	    ..
//	}
//
//	func (c *cache) Init(context.Context) error {
//	    ... use c.Config.Size ...
//	    return nil
//	}
//
// The application config file can specify these values as keys under the
// full component path.
//
//	["example.com/mypkg/MyComponent"]
//	Size = 1000
type WithConfig[T any] struct {
	config T
}

// Config returns the configuration information for the component that
// embeds this [weaver.WithConfig].
//
// Any fields in T that were not present in the application config
// file will have their default values.
//
// Any fields in the application config file that are not present in T
// will be flagged as an error at application startup.
func (wc *WithConfig[T]) Config() *T {
	return &wc.config
}
