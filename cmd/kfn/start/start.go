/*
Copyright 2022 The Crossplane Authors.

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

// Package start implements the Kubernetes Composition Function runner.
// It exposes a gRPC API that may be used to run Composition Functions.
package start

import (
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/crossplane/crossplane-runtime/pkg/errors"
	"github.com/crossplane/crossplane-runtime/pkg/logging"

	"github.com/crossplane/crossplane/internal/kfn"
)

// Error strings
const (
	errListenAndServe = "cannot listen for and serve gRPC API"
)

// Command starts a gRPC API to run Composition Functions.
type Command struct {
	CacheDir  string `short:"c" help:"Directory used for caching function images and containers." default:"/xfn"`
	Network   string `help:"Network on which to listen for gRPC connections." default:"unix"`
	Address   string `help:"Address at which to listen for gRPC connections." default:"@crossplane/fn/default.sock"`
	Namespace string `help:"Kubernetes namespace in which to run functions." default:"default"`
}

// Run a Composition Function gRPC API.
func (c *Command) Run(log logging.Logger) error {
	cfg, err := ctrl.GetConfig()
	if err != nil {
		return errors.Wrap(err, "cannot get Kubernetes client config")
	}

	// TODO(negz): Expose a healthz endpoint and otel metrics.
	f := kfn.NewContainerRunner(cfg, kfn.WithLogger(log), kfn.WithNamespace(c.Namespace))
	return errors.Wrap(f.ListenAndServe(c.Network, c.Address), errListenAndServe)
}
