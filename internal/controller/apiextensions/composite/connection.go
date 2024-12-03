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

package composite

import (
	"context"

	"github.com/crossplane/crossplane-runtime/pkg/connection/store"
	"github.com/crossplane/crossplane-runtime/pkg/reconciler/managed"
)

// A ConnectionDetailsFetcherFn fetches the connection details of the supplied
// resource, if any.
type ConnectionDetailsFetcherFn func(ctx context.Context, o store.SecretOwner) (managed.ConnectionDetails, error)

// FetchConnection calls the FetchConnectionDetailsFn.
func (f ConnectionDetailsFetcherFn) FetchConnection(ctx context.Context, o store.SecretOwner) (managed.ConnectionDetails, error) {
	return f(ctx, o)
}

// A ConnectionDetailsFetcherChain chains multiple ConnectionDetailsFetchers.
type ConnectionDetailsFetcherChain []managed.ConnectionDetailsFetcher

// FetchConnection details of the supplied composed resource, if any.
func (fc ConnectionDetailsFetcherChain) FetchConnection(ctx context.Context, o store.SecretOwner) (managed.ConnectionDetails, error) {
	all := make(managed.ConnectionDetails)
	for _, p := range fc {
		conn, err := p.FetchConnection(ctx, o)
		if err != nil {
			return nil, err
		}
		for k, v := range conn {
			all[k] = v
		}
	}
	return all, nil
}

// SecretStoreConnectionPublisher is a ConnectionPublisher that stores
// connection details on the configured SecretStore.
type SecretStoreConnectionPublisher struct {
	publisher managed.ConnectionPublisher
	filter    []string
}

// NewSecretStoreConnectionPublisher returns a SecretStoreConnectionPublisher.
func NewSecretStoreConnectionPublisher(p managed.ConnectionPublisher, filter []string) *SecretStoreConnectionPublisher {
	return &SecretStoreConnectionPublisher{
		publisher: p,
		filter:    filter,
	}
}

// PublishConnection details for the supplied resource.
func (p *SecretStoreConnectionPublisher) PublishConnection(ctx context.Context, o store.SecretOwner, c managed.ConnectionDetails) (published bool, err error) {
	// This resource does not want to expose a connection secret.
	if o.GetPublishConnectionDetailsTo() == nil {
		return false, nil
	}

	data := map[string][]byte{}
	m := map[string]bool{}
	for _, key := range p.filter {
		m[key] = true
	}

	for key, val := range c {
		// If the filter does not have any keys, we allow all given keys to be
		// published.
		if len(m) == 0 || m[key] {
			data[key] = val
		}
	}

	return p.publisher.PublishConnection(ctx, o, data)
}

// UnpublishConnection details for the supplied resource.
func (p *SecretStoreConnectionPublisher) UnpublishConnection(ctx context.Context, o store.SecretOwner, c managed.ConnectionDetails) error {
	return p.publisher.UnpublishConnection(ctx, o, c)
}
