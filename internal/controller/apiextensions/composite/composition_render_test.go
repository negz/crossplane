/*
Copyright 2022 The Crossplane Authors.

Licensed under the Apache License, Version 2.0 (the "License"); you may not use
this file except in compliance with the License. You may obtain a copy of the
License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software distributed
under the License is distributed on an "AS IS" BASIS, WITHOUT WARRANTIES OR
CONDITIONS OF ANY KIND, either express or implied. See the License for the
specific language governing permissions and limitations under the License.
*/

package composite

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"github.com/crossplane/crossplane-runtime/pkg/errors"
	"github.com/crossplane/crossplane-runtime/pkg/meta"
	"github.com/crossplane/crossplane-runtime/pkg/resource"
	"github.com/crossplane/crossplane-runtime/pkg/resource/fake"
	"github.com/crossplane/crossplane-runtime/pkg/test"

	"github.com/crossplane/crossplane/internal/xcrd"
)

func TestRenderComposedResourceMetadata(t *testing.T) {
	controlled := &fake.Composed{
		ObjectMeta: metav1.ObjectMeta{
			OwnerReferences: []metav1.OwnerReference{{
				Controller: ptr.To(true),
				UID:        "very-random",
			}},
		},
	}
	errRef := meta.AddControllerReference(controlled, metav1.OwnerReference{UID: "not-very-random"})

	type args struct {
		xr resource.Composite
		cd resource.Composed
		rn ResourceName
	}
	type want struct {
		cd  resource.Composed
		err error
	}
	cases := map[string]struct {
		reason string
		args
		want
	}{
		"ConflictingControllerReference": {
			reason: "We should return an error if the composed resource has an existing (and different) controller reference",
			args: args{
				xr: &fake.Composite{
					ObjectMeta: metav1.ObjectMeta{
						UID: "somewhat-random",
						Labels: map[string]string{
							xcrd.LabelKeyNamePrefixForComposed: "prefix",
						},
					},
				},
				cd: &fake.Composed{
					ObjectMeta: metav1.ObjectMeta{
						OwnerReferences: []metav1.OwnerReference{{
							Controller: ptr.To(true),
							UID:        "very-random",
						}},
					},
				},
			},
			want: want{
				cd: &fake.Composed{
					ObjectMeta: metav1.ObjectMeta{
						GenerateName: "prefix-",
						OwnerReferences: []metav1.OwnerReference{{
							Controller: ptr.To(true),
							UID:        "very-random",
						}},
						Labels: map[string]string{
							xcrd.LabelKeyNamePrefixForComposed: "prefix",
						},
					},
				},
				err: errors.Wrap(errRef, errSetControllerRef),
			},
		},
		"CompatibleControllerReference": {
			reason: "We should not return an error if the composed resource has an existing (and matching) controller reference",
			args: args{
				xr: &fake.Composite{
					ObjectMeta: metav1.ObjectMeta{
						UID: "somewhat-random",
						Labels: map[string]string{
							xcrd.LabelKeyNamePrefixForComposed: "prefix",
						},
					},
				},
				cd: &fake.Composed{
					ObjectMeta: metav1.ObjectMeta{
						OwnerReferences: []metav1.OwnerReference{{
							Controller: ptr.To(true),
							UID:        "somewhat-random",
						}},
					},
				},
			},
			want: want{
				cd: &fake.Composed{
					ObjectMeta: metav1.ObjectMeta{
						GenerateName: "prefix-",
						OwnerReferences: []metav1.OwnerReference{{
							Controller:         ptr.To(true),
							BlockOwnerDeletion: ptr.To(true),
							UID:                "somewhat-random",
						}},
						Labels: map[string]string{
							xcrd.LabelKeyNamePrefixForComposed: "prefix",
						},
					},
				},
			},
		},
		"NoControllerReference": {
			reason: "We should not return an error if the composed resource has no controller reference",
			args: args{
				xr: &fake.Composite{
					ObjectMeta: metav1.ObjectMeta{
						Name: "cool-xr",
						UID:  "somewhat-random",
						Labels: map[string]string{
							xcrd.LabelKeyNamePrefixForComposed: "prefix",
						},
					},
				},
				cd: &fake.Composed{},
			},
			want: want{
				cd: &fake.Composed{
					ObjectMeta: metav1.ObjectMeta{
						GenerateName: "prefix-",
						OwnerReferences: []metav1.OwnerReference{{
							Controller:         ptr.To(true),
							BlockOwnerDeletion: ptr.To(true),
							UID:                "somewhat-random",
							Name:               "cool-xr",
						}},
						Labels: map[string]string{
							xcrd.LabelKeyNamePrefixForComposed: "prefix",
						},
					},
				},
			},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := RenderComposedResourceMetadata(tc.args.cd, tc.args.xr, tc.args.rn)
			if diff := cmp.Diff(tc.want.err, err, test.EquateErrors()); diff != "" {
				t.Errorf("\n%s\nRenderComposedResourceMetadata(...): -want error, +got error:\n%s", tc.reason, diff)
			}
			if diff := cmp.Diff(tc.want.cd, tc.args.cd); diff != "" {
				t.Errorf("\n%s\nRenderComposedResourceMetadata(...): -want, +got:\n%s", tc.reason, diff)
			}
		})
	}
}
