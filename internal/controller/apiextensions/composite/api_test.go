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
package composite

import (
	"context"
	"testing"

	"github.com/google/go-cmp/cmp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	xpv1 "github.com/crossplane/crossplane-runtime/apis/common/v1"
	"github.com/crossplane/crossplane-runtime/pkg/errors"
	"github.com/crossplane/crossplane-runtime/pkg/reconciler/managed"
	"github.com/crossplane/crossplane-runtime/pkg/resource"
	"github.com/crossplane/crossplane-runtime/pkg/resource/fake"
	"github.com/crossplane/crossplane-runtime/pkg/test"

	v1 "github.com/crossplane/crossplane/apis/apiextensions/v1"
	"github.com/crossplane/crossplane/internal/xcrd"
)

func TestPublishConnection(t *testing.T) {
	errBoom := errors.New("boom")

	owner := &fake.MockConnectionSecretOwner{
		WriterTo: &xpv1.SecretReference{
			Namespace: "coolnamespace",
			Name:      "coolsecret",
		},
	}

	type args struct {
		applicator resource.Applicator
		o          resource.ConnectionSecretOwner
		filter     []string
		c          managed.ConnectionDetails
	}
	type want struct {
		published bool
		err       error
	}

	cases := map[string]struct {
		reason string
		args   args
		want   want
	}{
		"ResourceDoesNotPublishSecret": {
			reason: "A managed resource with a nil GetWriteConnectionSecretToReference should not publish a secret",
			args: args{
				o: &fake.MockConnectionSecretOwner{},
			},
		},
		"ApplyError": {
			reason: "An error applying the connection secret should be returned",
			args: args{
				applicator: resource.ApplyFn(func(_ context.Context, _ client.Object, _ ...resource.ApplyOption) error { return errBoom }),
				o:          owner,
			},
			want: want{
				err: errors.Wrap(errBoom, errApplySecret),
			},
		},
		"SuccessfulNoOp": {
			reason: "If application would be a no-op we should not publish a secret.",
			args: args{
				applicator: resource.ApplyFn(func(ctx context.Context, o client.Object, _ ...resource.ApplyOption) error {
					// Simulate a no-op change by not allowing the update.
					return resource.AllowUpdateIf(func(_, _ runtime.Object) bool { return false })(ctx, o, o)
				}),
				o:      owner,
				c:      managed.ConnectionDetails{"cool": {42}, "onlyme": {41}},
				filter: []string{"onlyme"},
			},
			want: want{
				published: false,
			},
		},
		"SuccessfulPublish": {
			reason: "If the secret changed we should publish it.",
			args: args{
				applicator: resource.ApplyFn(func(_ context.Context, o client.Object, _ ...resource.ApplyOption) error {
					want := resource.ConnectionSecretFor(owner, owner.GetObjectKind().GroupVersionKind())
					want.Data = managed.ConnectionDetails{"onlyme": {41}}
					if diff := cmp.Diff(want, o); diff != "" {
						t.Errorf("-want, +got:\n%s", diff)
					}
					return nil
				}),
				o:      owner,
				c:      managed.ConnectionDetails{"cool": {42}, "onlyme": {41}},
				filter: []string{"onlyme"},
			},
			want: want{
				published: true,
			},
		},
		"SuccessfulPublishAllWithEmptyList": {
			reason: "We should publish all keys if the filter is empty.",
			args: args{
				applicator: resource.ApplyFn(func(_ context.Context, o client.Object, _ ...resource.ApplyOption) error {
					want := resource.ConnectionSecretFor(owner, owner.GetObjectKind().GroupVersionKind())
					want.Data = managed.ConnectionDetails{"cool": {42}, "onlyme": {41}}
					if diff := cmp.Diff(want, o); diff != "" {
						t.Errorf("-want, +got:\n%s", diff)
					}
					return nil
				}),
				o:      owner,
				c:      managed.ConnectionDetails{"cool": {42}, "onlyme": {41}},
				filter: []string{},
			},
			want: want{
				published: true,
			},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			a := &APIFilteredSecretPublisher{tc.args.applicator, tc.args.filter}
			got, err := a.PublishConnection(context.Background(), tc.args.o, tc.args.c)
			if diff := cmp.Diff(tc.want.published, got); diff != "" {
				t.Errorf("\n%s\nPublish(...): -want, +got:\n%s", tc.reason, diff)
			}
			if diff := cmp.Diff(tc.want.err, err, test.EquateErrors()); diff != "" {
				t.Errorf("\n%s\nPublish(...): -want error, +got error:\n%s", tc.reason, diff)
			}
		})
	}
}

func TestConfigure(t *testing.T) {
	errBoom := errors.New("boom")

	cs := fake.ConnectionSecretWriterTo{Ref: &xpv1.SecretReference{
		Name:      "foo",
		Namespace: "bar",
	}}
	cp := &fake.Composite{
		ObjectMeta:               metav1.ObjectMeta{UID: types.UID(cs.Ref.Name)},
		ConnectionSecretWriterTo: cs,
	}

	type args struct {
		kube client.Client
		cp   resource.Composite
		rev  *v1.CompositionRevision
	}
	type want struct {
		cp  resource.Composite
		err error
	}
	cases := map[string]struct {
		reason string
		args
		want
	}{
		"NotCompatible": {
			reason: "Should return error if given composition is not compatible",
			args: args{
				rev: &v1.CompositionRevision{
					Spec: v1.CompositionRevisionSpec{
						CompositeTypeRef: v1.TypeReference{APIVersion: "ola/crossplane.io", Kind: "olala"},
					},
				},
				cp: &fake.Composite{},
			},
			want: want{
				cp:  &fake.Composite{},
				err: errors.New(errCompositionNotCompatible),
			},
		},
		"AlreadyFilled": {
			reason: "Should be no-op if connection secret namespace is already filled",
			args:   args{cp: cp, rev: &v1.CompositionRevision{}},
			want:   want{cp: cp},
		},
		"ConnectionSecretRefMissing": {
			reason: "Should fill connection secret ref if missing",
			args: args{
				kube: &test.MockClient{MockUpdate: test.NewMockUpdateFn(nil)},
				cp: &fake.Composite{
					ObjectMeta: metav1.ObjectMeta{UID: types.UID(cs.Ref.Name)},
				},
				rev: &v1.CompositionRevision{
					Spec: v1.CompositionRevisionSpec{WriteConnectionSecretsToNamespace: &cs.Ref.Namespace},
				},
			},
			want: want{cp: cp},
		},
		"NilWriteConnectionSecretsToNamespace": {
			reason: "Should not fill connection secret ref if composition does not have WriteConnectionSecretsToNamespace",
			args: args{
				kube: &test.MockClient{MockUpdate: test.NewMockUpdateFn(nil)},
				cp: &fake.Composite{
					ObjectMeta: metav1.ObjectMeta{UID: types.UID(cs.Ref.Name)},
				},
				rev: &v1.CompositionRevision{
					Spec: v1.CompositionRevisionSpec{},
				},
			},
			want: want{cp: &fake.Composite{
				ObjectMeta: metav1.ObjectMeta{UID: types.UID(cs.Ref.Name)},
			}},
		},
		"UpdateFailed": {
			reason: "Should fail if kube update failed",
			args: args{
				kube: &test.MockClient{MockUpdate: test.NewMockUpdateFn(errBoom)},
				cp: &fake.Composite{
					ObjectMeta: metav1.ObjectMeta{UID: types.UID(cs.Ref.Name)},
				},
				rev: &v1.CompositionRevision{
					Spec: v1.CompositionRevisionSpec{
						WriteConnectionSecretsToNamespace: &cs.Ref.Namespace,
					},
				},
			},
			want: want{
				cp:  cp,
				err: errors.Wrap(errBoom, errUpdateComposite),
			},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c := &APIConfigurator{client: tc.args.kube}
			err := c.Configure(context.Background(), tc.args.cp, tc.args.rev)
			if diff := cmp.Diff(tc.want.err, err, test.EquateErrors()); diff != "" {
				t.Errorf("\n%s\nConfigure(...): -want, +got:\n%s", tc.reason, diff)
			}
			if diff := cmp.Diff(tc.want.cp, tc.args.cp); diff != "" {
				t.Errorf("\n%s\nConfigure(...): -want, +got:\n%s", tc.reason, diff)
			}
		})
	}
}

func TestAPINamingConfigurator(t *testing.T) {
	type args struct {
		kube client.Client
		cp   resource.Composite
	}
	type want struct {
		cp  resource.Composite
		err error
	}

	cases := map[string]struct {
		reason string
		args
		want
	}{
		"LabelAlreadyExists": {
			reason: "No operation should be done if the name prefix is already given",
			args: args{
				cp: &fake.Composite{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{xcrd.LabelKeyNamePrefixForComposed: "given"}}},
			},
			want: want{
				cp: &fake.Composite{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{xcrd.LabelKeyNamePrefixForComposed: "given"}}},
			},
		},
		"AssignedName": {
			reason: "Its own name should be used as name prefix if it is not given",
			args: args{
				kube: &test.MockClient{
					MockUpdate: test.NewMockUpdateFn(nil),
				},
				cp: &fake.Composite{ObjectMeta: metav1.ObjectMeta{Name: "cp"}},
			},
			want: want{
				cp: &fake.Composite{ObjectMeta: metav1.ObjectMeta{Name: "cp", Labels: map[string]string{xcrd.LabelKeyNamePrefixForComposed: "cp"}}},
			},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c := NewAPINamingConfigurator(tc.args.kube)
			err := c.Configure(context.Background(), tc.args.cp, nil)
			if diff := cmp.Diff(tc.want.err, err, test.EquateErrors()); diff != "" {
				t.Errorf("\n%s\nConfigure(...): -want, +got:\n%s", tc.reason, diff)
			}
			if diff := cmp.Diff(tc.want.cp, tc.args.cp); diff != "" {
				t.Errorf("\n%s\nConfigure(...): -want, +got:\n%s", tc.reason, diff)
			}
		})
	}
}
