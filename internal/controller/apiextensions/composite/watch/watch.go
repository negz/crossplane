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

package watch

import (
	"context"
	"time"

	kunstructured "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/crossplane/crossplane-runtime/pkg/errors"
	"github.com/crossplane/crossplane-runtime/pkg/logging"
	"github.com/crossplane/crossplane-runtime/pkg/resource"
	"github.com/crossplane/crossplane-runtime/pkg/resource/unstructured/composite"
)

// A ControllerEngine can stop watches for a controller, and remove informers.
type ControllerEngine interface {
	StopWatches(ctx context.Context, name string, keep ...schema.GroupVersionKind) error
	RemoveUnwatchedInformers(ctx context.Context) error
}

// A GarbageCollector garbage collects watches for a composite resource
// controller.
type GarbageCollector struct {
	controllerName string
	xrGVK          schema.GroupVersionKind

	c  client.Reader
	ce ControllerEngine

	alwaysKeep []schema.GroupVersionKind

	log logging.Logger
}

// A GarbageCollectorOption configures a GarbageCollector.
type GarbageCollectorOption func(gc *GarbageCollector)

// WithLogger configures how a GarbageCollector should log.
func WithLogger(l logging.Logger) GarbageCollectorOption {
	return func(gc *GarbageCollector) {
		gc.log = l
	}
}

// AlwaysKeep specifies a set of GVKs that the garbage collector should always
// keep - i.e. should never garbage collect. Typically this will be the XR's
// GVK, and that of a CompositionRevision.
func AlwaysKeep(gvks ...schema.GroupVersionKind) GarbageCollectorOption {
	return func(gc *GarbageCollector) {
		gc.alwaysKeep = gvks
	}
}

// NewGarbageCollector creates a new watch garbage collector for a controller.
func NewGarbageCollector(name string, of resource.CompositeKind, c client.Reader, ce ControllerEngine, o ...GarbageCollectorOption) *GarbageCollector {
	gc := &GarbageCollector{
		controllerName: name,
		xrGVK:          schema.GroupVersionKind(of),
		c:              c,
		ce:             ce,
		log:            logging.NewNopLogger(),
	}
	for _, fn := range o {
		fn(gc)
	}
	return gc
}

// GarbageCollectWatches runs garbage collection at the specified interval,
// until the supplied context is cancelled. It stops any watches for resource
// types that are no longer composed by any composite resource (XR). It also
// removed any informers that no loner power any watches.
func (gc *GarbageCollector) GarbageCollectWatches(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			gc.log.Debug("Stopping composite controller watch garbage collector", "error", ctx.Err())
			return
		case <-t.C:
			if err := gc.GarbageCollectWatchesNow(ctx); err != nil {
				gc.log.Info("Cannot garbage collect composite controller watches", "error", err)
			}
		}
	}
}

// GarbageCollectWatchesNow stops any watches for resource types that are no
// longer composed by any composite resource (XR). It also removed any informers
// that no longer power any watches.
func (gc *GarbageCollector) GarbageCollectWatchesNow(ctx context.Context) error {
	// List all XRs of the type we're interested in.
	l := &kunstructured.UnstructuredList{}
	l.SetAPIVersion(gc.xrGVK.GroupVersion().String())
	l.SetKind(gc.xrGVK.Kind + "List")
	if err := gc.c.List(ctx, l); err != nil {
		return errors.Wrap(err, "cannot list composite resources")
	}

	// Build the set of GVKs they still reference.
	gvks := make(map[schema.GroupVersionKind]bool)
	for _, u := range l.Items {
		xr := &composite.Unstructured{Unstructured: u}
		for _, ref := range xr.GetResourceReferences() {
			gvks[schema.FromAPIVersionAndKind(ref.APIVersion, ref.Kind)] = true
		}
	}

	keep := make([]schema.GroupVersionKind, 0, len(gc.alwaysKeep)+len(gvks))
	keep = append(keep, gc.alwaysKeep...)
	for gvk := range gvks {
		keep = append(keep, gvk)
	}

	// Tell our controller to stop watching any other types of resource.
	if err := gc.ce.StopWatches(ctx, gc.controllerName, keep...); err != nil {
		return errors.Wrap(err, "cannot stop watches for composed resource types that are no longer referenced by any composite resource")
	}

	// TODO(negz): We really need another garbage collector that removes
	// informers when a CRD is deleted. Otherwise the informer will be sad and
	// log a lot until it's shutdown.

	// Tell our engine to remove any informers that are no longer used by any
	// controller.
	return errors.Wrap(gc.ce.RemoveUnwatchedInformers(ctx), "cannot remove informers that are no longer backing a watch by any controller")
}
