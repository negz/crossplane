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
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/crossplane/crossplane-runtime/pkg/errors"
	"github.com/crossplane/crossplane-runtime/pkg/reconciler/managed"
	"github.com/crossplane/crossplane-runtime/pkg/resource"
	"github.com/crossplane/crossplane-runtime/pkg/resource/fake"
	"github.com/crossplane/crossplane-runtime/pkg/test"
)

var _ managed.ConnectionDetailsFetcher = ConnectionDetailsFetcherChain{}

func TestConnectionDetailsFetcherChain(t *testing.T) {
	errBoom := errors.New("boom")

	type args struct {
		ctx context.Context
		o   resource.ConnectionSecretOwner
	}
	type want struct {
		conn managed.ConnectionDetails
		err  error
	}

	cases := map[string]struct {
		reason string
		c      ConnectionDetailsFetcherChain
		args   args
		want   want
	}{
		"EmptyChain": {
			reason: "An empty chain should return empty connection details.",
			c:      ConnectionDetailsFetcherChain{},
			args: args{
				o: &fake.Composed{},
			},
			want: want{
				conn: managed.ConnectionDetails{},
			},
		},
		"SingleFetcherChain": {
			reason: "A chain of one fetcher should return only its connection details.",
			c: ConnectionDetailsFetcherChain{
				ConnectionDetailsFetcherFn(func(_ context.Context, _ resource.ConnectionSecretOwner) (managed.ConnectionDetails, error) {
					return managed.ConnectionDetails{"a": []byte("b")}, nil
				}),
			},
			args: args{
				o: &fake.Composed{},
			},
			want: want{
				conn: managed.ConnectionDetails{"a": []byte("b")},
			},
		},
		"FetcherError": {
			reason: "We should return errors from a chained fetcher.",
			c: ConnectionDetailsFetcherChain{
				ConnectionDetailsFetcherFn(func(_ context.Context, _ resource.ConnectionSecretOwner) (managed.ConnectionDetails, error) {
					return nil, errBoom
				}),
			},
			args: args{
				o: &fake.Composed{},
			},
			want: want{
				err: errBoom,
			},
		},
		"MultipleFetcherChain": {
			reason: "A chain of multiple fetchers should return all of their connection details, with later fetchers winning if there are duplicates.",
			c: ConnectionDetailsFetcherChain{
				ConnectionDetailsFetcherFn(func(_ context.Context, _ resource.ConnectionSecretOwner) (managed.ConnectionDetails, error) {
					return managed.ConnectionDetails{
						"a": []byte("a"),
						"b": []byte("b"),
						"c": []byte("c"),
					}, nil
				}),
				ConnectionDetailsFetcherFn(func(_ context.Context, _ resource.ConnectionSecretOwner) (managed.ConnectionDetails, error) {
					return managed.ConnectionDetails{
						"a": []byte("A"),
					}, nil
				}),
			},
			args: args{
				o: &fake.Composed{},
			},
			want: want{
				conn: managed.ConnectionDetails{
					"a": []byte("A"),
					"b": []byte("b"),
					"c": []byte("c"),
				},
			},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			conn, err := tc.c.FetchConnection(tc.args.ctx, tc.args.o)
			if diff := cmp.Diff(tc.want.err, err, test.EquateErrors()); diff != "" {
				t.Errorf("\n%s\nFetchConnection(...): -want, +got:\n%s", tc.reason, diff)
			}
			if diff := cmp.Diff(tc.want.conn, conn, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("\n%s\nFetchFetchConnection(...): -want, +got:\n%s", tc.reason, diff)
			}
		})
	}
}
