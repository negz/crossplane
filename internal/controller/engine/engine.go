/*
Copyright 2024 The Crossplane Authors.

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

// Package engine manages the lifecycle of a set of controllers.
package engine

import (
	"context"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	kcontroller "sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/crossplane/crossplane-runtime/pkg/errors"
	"github.com/crossplane/crossplane-runtime/pkg/logging"
)

// A ControllerEngine manages a set of controllers that can be dynamically
// started and stopped. It also manages a dynamic set of watches per controller,
// and the informers that back them.
type ControllerEngine struct {
	// The manager of this engine's controllers.
	mgr manager.Manager

	// The engine must have exclusive use of these informers. All controllers
	// managed by the engine should use these informers.
	infs TrackingInformers

	log logging.Logger

	// Protects everything below.
	mx sync.RWMutex

	// Running controllers, by name.
	controllers map[string]*controller
}

// TrackingInformers is a set of Informers. It tracks which are active.
type TrackingInformers interface {
	cache.Informers
	ActiveInformers() []schema.GroupVersionKind
}

// New creates a new controller engine.
func New(mgr manager.Manager, infs TrackingInformers, o ...ControllerEngineOption) *ControllerEngine {
	e := &ControllerEngine{
		mgr:         mgr,
		infs:        infs,
		log:         logging.NewNopLogger(),
		controllers: make(map[string]*controller),
	}

	for _, fn := range o {
		fn(e)
	}

	return e
}

// An ControllerEngineOption configures a controller engine.
type ControllerEngineOption func(*ControllerEngine)

// WithLogger configures an Engine to use a logger.
func WithLogger(l logging.Logger) ControllerEngineOption {
	return func(e *ControllerEngine) {
		e.log = l
	}
}

type controller struct {
	// The running controller.
	ctrl kcontroller.Controller

	// Called to stop the controller.
	cancel context.CancelFunc

	// Protects the below map.
	mx sync.RWMutex

	// The controller's sources, by watched GVK.
	sources map[schema.GroupVersionKind]*StoppableSource
}

// A WatchGarbageCollector periodically garbage collects watches.
type WatchGarbageCollector interface {
	GarbageCollectWatches(ctx context.Context, interval time.Duration)
}

// ControllerOptions configure a controller.
type ControllerOptions struct {
	runtime kcontroller.Options
	gc      WatchGarbageCollector
}

// A ControllerOption configures a controller.
type ControllerOption func(o *ControllerOptions)

// WithRuntimeOptions configures the underlying controller-runtime controller.
func WithRuntimeOptions(ko kcontroller.Options) ControllerOption {
	return func(o *ControllerOptions) {
		o.runtime = ko
	}
}

// WithWatchGarbageCollector specifies an optional garbage collector this
// controller should use to remove unused watches.
func WithWatchGarbageCollector(gc WatchGarbageCollector) ControllerOption {
	return func(o *ControllerOptions) {
		o.gc = gc
	}
}

// Start a new controller.
func (e *ControllerEngine) Start(name string, o ...ControllerOption) error {
	co := &ControllerOptions{}
	for _, fn := range o {
		fn(co)
	}

	// Start is a no-op if the controller is already running.
	e.mx.RLock()
	_, ok := e.controllers[name]
	e.mx.RUnlock()
	if ok {
		return nil
	}

	c, err := kcontroller.NewUnmanaged(name, e.mgr, co.runtime)
	if err != nil {
		return errors.Wrap(err, "cannot create new controller")
	}

	// The caller will usually be a reconcile method. We want the controller
	// to keep running when the reconcile ends, so we create a new context
	// instead of taking one as an argument.
	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		// Don't start the controller until the manager is elected.
		<-e.mgr.Elected()

		e.log.Debug("Starting new controller", "controller", name)

		// Run the controller until its context is cancelled.
		if err := c.Start(ctx); err != nil {
			e.log.Info("Controller returned an error", "name", name, "error", err)
		}

		e.log.Debug("Stopped controller", "controller", name)
	}()

	if co.gc != nil {
		go func() {
			// Don't start the garbage collector until the manager is elected.
			<-e.mgr.Elected()

			e.log.Debug("Starting watch garbage collector for controller", "controller", name)

			// Run the collector every minute until its context is cancelled.
			co.gc.GarbageCollectWatches(ctx, 1*time.Minute)

			e.log.Debug("Stopped watch garbage collector for controller", "controller", name)
		}()
	}

	r := &controller{
		ctrl:    c,
		cancel:  cancel,
		sources: make(map[schema.GroupVersionKind]*StoppableSource),
	}

	e.mx.Lock()
	e.controllers[name] = r
	e.mx.Unlock()

	return nil
}

// Stop a controller.
func (e *ControllerEngine) Stop(ctx context.Context, name string) error {
	e.mx.RLock()
	c, running := e.controllers[name]
	e.mx.RUnlock()

	// Stop is a no-op if the controller isn't running.
	if !running {
		return nil
	}

	// First stop the controller's watches.
	if err := e.StopWatches(ctx, name); err != nil {
		return errors.Wrapf(err, "cannot stop watches for controller %q", name)
	}

	// Then stop any informers that only this controller was using.
	if err := e.RemoveUnwatchedInformers(ctx); err != nil {
		return errors.Wrapf(err, "cannot remove unwatched informers after stopping watches for controller %q", name)
	}

	// Finally, stop and delete the controller.
	e.mx.Lock()
	c.cancel()
	delete(e.controllers, name)
	e.mx.Unlock()

	e.log.Debug("Stopped controller", "controller", name)
	return nil
}

// IsRunning returns true if the named controller is running.
func (e *ControllerEngine) IsRunning(name string) bool {
	e.mx.RLock()
	_, running := e.controllers[name]
	e.mx.RUnlock()
	return running
}

// Watch an object.
type Watch struct {
	kind       client.Object
	handler    handler.EventHandler
	predicates []predicate.Predicate
}

// WatchFor returns a Watch for the supplied kind of object. Events will be
// handled by the supplied EventHandler, and may be filtered by the supplied
// predicates.
func WatchFor(kind client.Object, h handler.EventHandler, p ...predicate.Predicate) Watch {
	return Watch{kind: kind, handler: h, predicates: p}
}

// StartWatches instructs the named controller to start the supplied watches.
// The controller will only start a watch if it's not already watching the type
// of object specified by the supplied Watch. StartWatches blocks other
// operations on the same controller if and when it starts a watch.
func (e *ControllerEngine) StartWatches(name string, ws ...Watch) error {
	e.mx.RLock()
	c, ok := e.controllers[name]
	e.mx.RUnlock()

	if !ok {
		return errors.Errorf("controller %q is not running", name)
	}

	watchExists := make(map[schema.GroupVersionKind]bool)
	c.mx.RLock()
	for gvk := range c.sources {
		watchExists[gvk] = true
	}
	c.mx.RUnlock()

	// It's possible that we didn't explicitly stop a watch, but its backing
	// informer was removed. This implicitly stops the watch by deleting its
	// backing listener. If a watch exists but doesn't have an active informer,
	// we want to restart the watch (and, implicitly, the informer).
	a := e.infs.ActiveInformers()
	activeInformer := make(map[schema.GroupVersionKind]bool, len(a))
	for _, gvk := range a {
		activeInformer[gvk] = true
	}

	// Using a map here deduplicates watches by GVK. If we're asked to start
	// several watches for the same GVK at in the same call, we'll only start
	// the last one.
	start := make(map[schema.GroupVersionKind]Watch, len(ws))
	for _, w := range ws {
		gvk, err := apiutil.GVKForObject(w.kind, e.mgr.GetScheme())
		if err != nil {
			return errors.Wrapf(err, "cannot determine group, version, and kind for %T", w.kind)
		}

		// We've already created this watch and the informer backing it is still
		// running. We don't need to create a new watch.
		if watchExists[gvk] && activeInformer[gvk] {
			e.log.Debug("Watch exists for GVK, not starting a new one", "controller", name, "watched-gvk", gvk)
			continue
		}

		start[gvk] = w
	}

	// Don't take any write locks if there's no watches to start.
	if len(start) == 0 {
		return nil
	}

	// TODO(negz): If this blocks too much, we could alleviate it a bit by
	// reading watches to start from a buffered channel.

	// Take the write lock. This will block any other callers that want to
	// update the watches for the same controller.
	c.mx.Lock()
	defer c.mx.Unlock()

	// Start new sources.
	for gvk, w := range start {
		// The controller's Watch method just calls the StoppableSource's Start
		// method, passing in its private work queue as an argument. This
		// will start an informer for the watched kind if there isn't one
		// running already.
		//
		// The watch will stop sending events when either the source is stopped,
		// or its backing informer is stopped. The controller's work queue will
		// stop processing events when the controller is stopped.
		src := NewStoppableSource(e.infs, w.kind, w.handler, w.predicates...)
		if err := c.ctrl.Watch(src); err != nil {
			return errors.Wrapf(err, "cannot start watch for %q", gvk)
		}

		// Record that we're now running this source.
		c.sources[gvk] = src

		e.log.Debug("Started watching GVK", "controller", name, "watched-gvk", gvk)
	}

	return nil
}

// StopWatches stops all watches for the supplied controller, except watches for
// the GVKs supplied by the keep argument. StopWatches blocks other operations
// on the same controller if and when it stops a watch.
func (e *ControllerEngine) StopWatches(ctx context.Context, name string, keep ...schema.GroupVersionKind) error {
	e.mx.RLock()
	c, ok := e.controllers[name]
	e.mx.RUnlock()

	if !ok {
		return errors.Errorf("controller %q is not running", name)
	}

	c.mx.RLock()
	stop := sets.KeySet(c.sources).Difference(sets.New(keep...))
	c.mx.RUnlock()

	// Don't take the write lock if we actually want to keep all watches.
	if len(stop) == 0 {
		return nil
	}

	c.mx.Lock()
	defer c.mx.Unlock()

	for gvk := range stop {
		if err := c.sources[gvk].Stop(ctx); err != nil {
			return errors.Wrapf(err, "cannot stop watch for %q", gvk)
		}
		delete(c.sources, gvk)
		e.log.Debug("Stopped watching GVK", "controller", name, "watched-gvk", gvk)
	}

	return nil
}

// RemoveUnwatchedInformers removes all informers that don't power any
// controller's watches. This may include informers that weren't started by a
// watch, e.g. informers that were started by a Get or List call to a cache
// backed by the same informers as the Engine.
func (e *ControllerEngine) RemoveUnwatchedInformers(ctx context.Context) error {
	// Build the set of GVKs any controller is currently watching.
	e.mx.RLock()
	watching := make(map[schema.GroupVersionKind]bool, len(e.controllers))
	for _, c := range e.controllers {
		c.mx.RLock()
		for gvk := range c.sources {
			watching[gvk] = true
		}
		c.mx.RUnlock()
	}
	e.mx.RUnlock()

	for _, gvk := range e.infs.ActiveInformers() {
		// A controller is still watching this GVK. Don't remove its informer.
		if watching[gvk] {
			e.log.Debug("Not removing informer. At least one controller is still watching its GVK.", "watched-gvk", gvk)
			continue
		}

		// RemoveInformer only uses the supplied object to get its GVK.
		u := &unstructured.Unstructured{}
		u.SetAPIVersion(gvk.GroupVersion().String())
		u.SetKind(gvk.Kind)
		if err := e.infs.RemoveInformer(ctx, u); err != nil {
			return errors.Wrapf(err, "cannot remove informer for %q", gvk)
		}
		e.log.Debug("Removed informer. No controller watches its GVK.", "watched-gvk", gvk)
	}

	return nil
}
