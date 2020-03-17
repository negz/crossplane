/*
Copyright 2020 The Crossplane Authors.

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

package experimental

import (
	"context"
	"strings"
	"time"

	"github.com/pkg/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/crossplane/crossplane-runtime/pkg/logging"
	"github.com/crossplane/crossplane-runtime/pkg/meta"
	"github.com/crossplane/crossplane-runtime/pkg/resource"
	"github.com/crossplane/crossplane/apis/experimental/v1alpha1"
)

const (
	reconcileTimeout = 1 * time.Minute
)

// Reconcile event reasons.
const ()

// Setup adds a controller that reconciles ApplicationConfigurations.
func Setup(mgr ctrl.Manager, l logging.Logger) error {
	name := "experimental/" + strings.ToLower(v1alpha1.ExperimentGroupKind)

	return ctrl.NewControllerManagedBy(mgr).
		Named(name).
		For(&v1alpha1.Experiment{}).
		Complete(NewReconciler(mgr,
			WithLogger(l.WithValues("controller", name))))
}

// A Reconciler reconciles experiments.
type Reconciler struct {
	mgr manager.Manager

	log logging.Logger

	experiments map[string]chan struct{}
}

// A ReconcilerOption configures a Reconciler.
type ReconcilerOption func(*Reconciler)

// WithLogger specifies how the Reconciler should log messages.
func WithLogger(l logging.Logger) ReconcilerOption {
	return func(r *Reconciler) {
		r.log = l
	}
}

// NewReconciler returns a Reconciler for experiments!
func NewReconciler(m ctrl.Manager, o ...ReconcilerOption) *Reconciler {

	r := &Reconciler{
		mgr:         m,
		log:         logging.NewNopLogger(),
		experiments: make(map[string]chan struct{}),
	}

	for _, ro := range o {
		ro(r)
	}

	return r
}

// Reconcile an experiment.
func (r *Reconciler) Reconcile(req reconcile.Request) (reconcile.Result, error) {
	log := r.log.WithValues("request", req)
	log.Debug("Reconciling")

	client := r.mgr.GetClient()

	ctx, cancel := context.WithTimeout(context.Background(), reconcileTimeout)
	defer cancel()

	e := &v1alpha1.Experiment{}
	if err := client.Get(ctx, req.NamespacedName, e); err != nil {
		return reconcile.Result{}, errors.Wrap(resource.IgnoreNotFound(err), "cannot get experiment")
	}

	log = log.WithValues(
		"experimental-kind", e.Spec.Watch.Kind,
		"experimental-version", e.Spec.Watch.APIVersion)

	if meta.WasDeleted(e) {
		// NOTE(negz): For the purposes of this example we assume experiment CRs
		// are cluster scoped and follow similar naming conventions to CRDs. One
		// experiment may exist per watched kind, named <plural>.<group>.
		if stop, running := r.experiments[e.GetName()]; running {
			// Shut down the controller that watches this experiment kind.
			close(stop)
			delete(r.experiments, e.GetName())
		}
		meta.RemoveFinalizer(e, "experiment")
		if err := client.Update(ctx, e); err != nil {
			log.Debug("cannot remove finalizer", "error", err)
			return reconcile.Result{}, errors.Wrap(err, "cannot remove finalizer")
		}
		return reconcile.Result{}, nil
	}

	meta.AddFinalizer(e, "experiment")
	if err := client.Update(ctx, e); err != nil {
		log.Debug("cannot add finalizer", "error", err)
		return reconcile.Result{}, errors.Wrap(err, "cannot add finalizer")
	}

	if _, running := r.experiments[e.GetName()]; running {
		// NOTE(negz): For the purposes of this example we assume experiments
		// are immutable. We're already running, so there's nothing to do.
		return reconcile.Result{}, nil
	}

	stop := make(chan struct{})
	if err := runExperiment(r.mgr, e.Spec.Watch, r.log, stop); err != nil {
		log.Debug("cannot run experiment", "error", err)
		return reconcile.Result{}, errors.Wrap(err, "cannot stop experiment")
	}
	r.experiments[e.GetName()] = stop

	return reconcile.Result{}, nil
}

func runExperiment(m ctrl.Manager, w v1alpha1.Watch, l logging.Logger, stop <-chan struct{}) error {
	o := controller.Options{
		Reconciler: &experimentReconciler{client: m.GetClient(), log: l, watching: w},
	}

	c, err := controller.Configure("experiment/"+w.Kind, m, o)
	if err != nil {
		return err
	}

	u := &unstructured.Unstructured{}
	u.SetAPIVersion(w.APIVersion)
	u.SetKind(w.Kind)
	if err := c.Watch(&source.Kind{Type: u}, &handler.EnqueueRequestForObject{}); err != nil {
		return err
	}

	go func() {
		<-m.Elected()
		if err := c.Start(stop); err != nil {
			l.Debug("cannot run experiment controller", "error", err)
		}
	}()

	return nil
}

type experimentReconciler struct {
	client client.Client
	log    logging.Logger

	watching v1alpha1.Watch
}

func (r *experimentReconciler) Reconcile(req reconcile.Request) (reconcile.Result, error) {
	log := r.log.WithValues("request", req)
	log.Debug("Reconciling")

	ctx, cancel := context.WithTimeout(context.Background(), reconcileTimeout)
	defer cancel()

	u := &unstructured.Unstructured{}
	u.SetAPIVersion(r.watching.APIVersion)
	u.SetKind(r.watching.Kind)
	if err := r.client.Get(ctx, req.NamespacedName, u); err != nil {
		return reconcile.Result{}, errors.Wrap(resource.IgnoreNotFound(err), "cannot get experimental ")
	}

	r.log.Debug("Encountered an experimental object!", "namespace", u.GetNamespace(), "name", u.GetName())
	return reconcile.Result{}, nil
}
