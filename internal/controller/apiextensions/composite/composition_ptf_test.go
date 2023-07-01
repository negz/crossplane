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
	"net"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/testing/protocmp"
	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	computeresource "k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kunstructured "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/json"
	"k8s.io/utils/pointer"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/crossplane/crossplane-runtime/pkg/errors"
	"github.com/crossplane/crossplane-runtime/pkg/meta"
	"github.com/crossplane/crossplane-runtime/pkg/reconciler/managed"
	"github.com/crossplane/crossplane-runtime/pkg/resource"
	"github.com/crossplane/crossplane-runtime/pkg/resource/fake"
	"github.com/crossplane/crossplane-runtime/pkg/resource/unstructured/composed"
	"github.com/crossplane/crossplane-runtime/pkg/resource/unstructured/composite"
	"github.com/crossplane/crossplane-runtime/pkg/test"

	iov1alpha1 "github.com/crossplane/crossplane/apis/apiextensions/fn/io/v1alpha1"
	fnpbv1alpha1 "github.com/crossplane/crossplane/apis/apiextensions/fn/proto/v1alpha1"
	v1 "github.com/crossplane/crossplane/apis/apiextensions/v1"
)

func TestPTFCompose(t *testing.T) {
	details := managed.ConnectionDetails{"a": []byte("b")}

	type params struct {
		kube client.Client
		o    []PTFComposerOption
	}
	type args struct {
		ctx context.Context
		xr  resource.Composite
		req CompositionRequest
	}
	type want struct {
		res CompositionResult
		err error
	}

	cases := map[string]struct {
		reason string
		params params
		args   args
		want   want
	}{
		"Success": {
			// TODO(negz): This!
			reason: "",
			params: params{
				kube: &test.MockClient{
					// These are both called by Apply.
					MockGet:   test.NewMockGetFn(nil),
					MockPatch: test.NewMockPatchFn(nil),
				},
				o: []PTFComposerOption{
					WithCompositeConnectionDetailsFetcher(ConnectionDetailsFetcherFn(func(ctx context.Context, o resource.ConnectionSecretOwner) (managed.ConnectionDetails, error) {
						return details, nil
					})),
					WithComposedResourceObserver(ComposedResourceObserverFn(func(ctx context.Context, xr resource.Composite) (ObservedResources, error) {
						ors := ObservedResources{"cool-resource": ObservedResource{
							Resource:          composed.New(),
							ConnectionDetails: managed.ConnectionDetails{},
						}}
						return ors, nil
					})),
					WithComposedResourceDeleter(ComposedResourceDeleterFn(func(ctx context.Context, owner metav1.Object, cds ObservedResources) error {
						return nil
					})),
				},
			},
			args: args{
				xr: &fake.Composite{},
				req: CompositionRequest{
					Revision: &v1.CompositionRevision{},
				},
			},
			want: want{
				res: CompositionResult{
					Composed: []ComposedResource{{
						ResourceName: "cool-resource",
					}},
					ConnectionDetails: details,
				},
				err: nil,
			},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {

			c := NewPTFComposer(tc.params.kube, tc.params.o...)
			res, err := c.Compose(tc.args.ctx, tc.args.xr, tc.args.req)

			if diff := cmp.Diff(tc.want.err, err, test.EquateErrors()); diff != "" {
				t.Errorf("\n%s\nCompose(...): -want, +got:\n%s", tc.reason, diff)
			}

			if diff := cmp.Diff(tc.want.res, res, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("\n%s\nCompose(...): -want, +got:\n%s", tc.reason, diff)
			}
		})
	}
}

func TestGetComposedResources(t *testing.T) {
	errBoom := errors.New("boom")
	details := managed.ConnectionDetails{"a": []byte("b")}

	type params struct {
		c client.Reader
		f managed.ConnectionDetailsFetcher
	}

	type args struct {
		ctx context.Context
		xr  resource.Composite
	}

	type want struct {
		ors ObservedResources
		err error
	}

	cases := map[string]struct {
		reason string
		params params
		args   args
		want   want
	}{
		"UnnamedRef": {
			reason: "We should skip any resource references without names.",
			params: params{
				c: &test.MockClient{
					// We should continue past the unnamed reference and not hit
					// this error.
					MockGet: test.NewMockGetFn(errBoom),
				},
			},
			args: args{
				xr: &fake.Composite{
					ComposedResourcesReferencer: fake.ComposedResourcesReferencer{
						Refs: []corev1.ObjectReference{
							{
								APIVersion: "example.org/v1",
								Kind:       "Unnamed",
							},
						},
					},
				},
			},
		},
		"ComposedResourceNotFound": {
			reason: "We should skip any resources that are not found.",
			params: params{
				c: &test.MockClient{
					MockGet: test.NewMockGetFn(kerrors.NewNotFound(schema.GroupResource{}, "")),
				},
			},
			args: args{
				xr: &fake.Composite{
					ComposedResourcesReferencer: fake.ComposedResourcesReferencer{
						Refs: []corev1.ObjectReference{
							{Name: "cool-resource"},
						},
					},
				},
			},
		},
		"GetComposedResourceError": {
			reason: "We should return any error we encounter while getting a composed resource.",
			params: params{
				c: &test.MockClient{
					MockGet: test.NewMockGetFn(errBoom),
				},
			},
			args: args{
				xr: &fake.Composite{
					ComposedResourcesReferencer: fake.ComposedResourcesReferencer{
						Refs: []corev1.ObjectReference{
							{Name: "cool-resource"},
						},
					},
				},
			},
			want: want{
				err: errors.Wrap(errBoom, errGetComposed),
			},
		},
		"UncontrolledComposedResource": {
			reason: "We should skip any composed resources our XR doesn't control.",
			params: params{
				c: &test.MockClient{
					MockGet: test.NewMockGetFn(nil, func(obj client.Object) error {
						_ = meta.AddControllerReference(obj, metav1.OwnerReference{
							UID:        types.UID("someone-else"),
							Controller: pointer.Bool(true),
						})

						return nil
					}),
				},
			},
			args: args{
				xr: &fake.Composite{
					ComposedResourcesReferencer: fake.ComposedResourcesReferencer{
						Refs: []corev1.ObjectReference{
							{Name: "cool-resource"},
						},
					},
				},
			},
			want: want{
				err: nil,
			},
		},
		"AnonymousComposedResourceError": {
			reason: "We should return an error if we encounter an (unsupported) anonymous composed resource.",
			params: params{
				c: &test.MockClient{
					// We 'return' an empty resource with no annotations.
					MockGet: test.NewMockGetFn(nil),
				},
			},
			args: args{
				xr: &fake.Composite{
					ComposedResourcesReferencer: fake.ComposedResourcesReferencer{
						Refs: []corev1.ObjectReference{
							{Name: "cool-resource"},
						},
					},
				},
			},
			want: want{
				err: errors.New(errAnonymousCD),
			},
		},
		"FetchConnectionDetailsError": {
			reason: "We should return an error if we can't fetch composed resource connection details.",
			params: params{
				c: &test.MockClient{
					MockGet: test.NewMockGetFn(nil, func(obj client.Object) error {
						obj.SetName("cool-resource-42")
						SetCompositionResourceName(obj, "cool-resource")
						return nil
					}),
				},
				f: ConnectionDetailsFetcherFn(func(ctx context.Context, o resource.ConnectionSecretOwner) (managed.ConnectionDetails, error) {
					return nil, errBoom
				}),
			},
			args: args{
				xr: &fake.Composite{
					ComposedResourcesReferencer: fake.ComposedResourcesReferencer{
						Refs: []corev1.ObjectReference{
							{
								Kind: "Broken",
								Name: "cool-resource-42",
							},
						},
					},
				},
			},
			want: want{
				err: errors.Wrapf(errBoom, errFmtFetchCDConnectionDetails, "cool-resource", "Broken", "cool-resource-42"),
			},
		},
		"Success": {
			reason: "We should return any composed resources and their connection details.",
			params: params{
				c: &test.MockClient{
					MockGet: test.NewMockGetFn(nil, func(obj client.Object) error {
						obj.SetName("cool-resource-42")
						SetCompositionResourceName(obj, "cool-resource")
						return nil
					}),
				},
				f: ConnectionDetailsFetcherFn(func(ctx context.Context, o resource.ConnectionSecretOwner) (managed.ConnectionDetails, error) {
					return details, nil
				}),
			},
			args: args{
				xr: &fake.Composite{
					ComposedResourcesReferencer: fake.ComposedResourcesReferencer{
						Refs: []corev1.ObjectReference{
							{
								APIVersion: "example.org/v1",
								Kind:       "Composed",
								Name:       "cool-resource-42",
							},
						},
					},
				},
			},
			want: want{
				ors: ObservedResources{"cool-resource": ObservedResource{
					ConnectionDetails: details,
					Resource: func() resource.Composed {
						cd := composed.New()
						cd.SetAPIVersion("example.org/v1")
						cd.SetKind("Composed")
						cd.SetName("cool-resource-42")
						SetCompositionResourceName(cd, "cool-resource")
						return cd
					}(),
				},
				},
			},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {

			g := NewExistingComposedResourceObserver(tc.params.c, tc.params.f)
			ors, err := g.ObserveComposedResources(tc.args.ctx, tc.args.xr)

			if diff := cmp.Diff(tc.want.err, err, test.EquateErrors()); diff != "" {
				t.Errorf("\n%s\nObserveComposedResources(...): -want, +got:\n%s", tc.reason, diff)
			}

			if diff := cmp.Diff(tc.want.ors, ors, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("\n%s\nObserveComposedResources(...): -want, +got:\n%s", tc.reason, diff)
			}
		})
	}
}

func TestFunctionIOObserved(t *testing.T) {
	unmarshalable := kunstructured.Unstructured{Object: map[string]interface{}{
		"you-cant-marshal-a-channel": make(chan<- string),
	}}
	_, errUnmarshalableXR := json.Marshal(&composite.Unstructured{Unstructured: unmarshalable})
	_, errUnmarshalableCD := json.Marshal(&composed.Unstructured{Unstructured: unmarshalable})

	type args struct {
		xr  resource.Composite
		xc  managed.ConnectionDetails
		ors ObservedResources
	}
	type want struct {
		o   iov1alpha1.Observed
		err error
	}

	cases := map[string]struct {
		reason string
		args   args
		want   want
	}{
		"MarshalCompositeError": {
			reason: "We should return an error if we can't marshal the XR.",
			args: args{
				xr: &composite.Unstructured{Unstructured: unmarshalable},
			},
			want: want{
				err: errors.Wrap(errUnmarshalableXR, errMarshalXR),
			},
		},
		"MarshalComposedError": {
			reason: "We should return an error if we can't marshal a composed resource.",
			args: args{
				xr: composite.New(),
				ors: ObservedResources{
					"cool-resource": ObservedResource{
						Resource: &composed.Unstructured{Unstructured: unmarshalable},
					},
				},
			},
			want: want{
				err: errors.Wrap(errUnmarshalableCD, errMarshalCD),
			},
		},
		"Success": {
			reason: "We should successfully build a FunctionIO Observed struct from our state.",
			args: args{
				xr: composite.New(composite.WithGroupVersionKind(schema.GroupVersionKind{
					Group:   "example.org",
					Version: "v1",
					Kind:    "Composite",
				})),
				xc: managed.ConnectionDetails{"a": []byte("b")},
				ors: ObservedResources{
					"cool-resource": ObservedResource{
						Resource: composed.New(composed.FromReference(corev1.ObjectReference{
							APIVersion: "example.org/v2",
							Kind:       "Composed",
							Name:       "cool-resource-42",
						})),
						ConnectionDetails: managed.ConnectionDetails{"c": []byte("d")},
					},
				},
			},
			want: want{
				o: iov1alpha1.Observed{
					Composite: iov1alpha1.ObservedComposite{
						Resource: runtime.RawExtension{
							Raw: []byte(`{"apiVersion":"example.org/v1","kind":"Composite"}`),
						},
						ConnectionDetails: []iov1alpha1.ExplicitConnectionDetail{{
							Name:  "a",
							Value: "b",
						}},
					},
					Resources: []iov1alpha1.ObservedResource{{
						Name: "cool-resource",
						Resource: runtime.RawExtension{
							Raw: []byte(`{"apiVersion":"example.org/v2","kind":"Composed","metadata":{"name":"cool-resource-42"}}`),
						},
						ConnectionDetails: []iov1alpha1.ExplicitConnectionDetail{{
							Name:  "c",
							Value: "d",
						}},
					}},
				},
			},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			o, err := FunctionIOObserved(tc.args.xr, tc.args.xc, tc.args.ors)

			if diff := cmp.Diff(tc.want.err, err, test.EquateErrors()); diff != "" {
				t.Errorf("\n%s\nFunctionIOObserved(...): -want, +got:\n%s", tc.reason, diff)
			}

			if diff := cmp.Diff(tc.want.o, o, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("\n%s\nFunctionIOObserved(...): -want, +got:\n%s", tc.reason, diff)
			}
		})
	}
}

func TestFunctionIODesired(t *testing.T) {
	unmarshalable := kunstructured.Unstructured{Object: map[string]interface{}{
		"you-cant-marshal-a-channel": make(chan<- string),
	}}
	_, errUnmarshalableXR := json.Marshal(&composite.Unstructured{Unstructured: unmarshalable})
	_, errUnmarshalableCD := json.Marshal(&composed.Unstructured{Unstructured: unmarshalable})

	type args struct {
		xr  resource.Composite
		xc  managed.ConnectionDetails
		drs DesiredResources
	}
	type want struct {
		o   iov1alpha1.Desired
		err error
	}

	cases := map[string]struct {
		reason string
		args   args
		want   want
	}{
		"MarshalCompositeError": {
			reason: "We should return an error if we can't marshal the XR.",
			args: args{
				xr: &composite.Unstructured{Unstructured: unmarshalable},
			},
			want: want{
				err: errors.Wrap(errUnmarshalableXR, errMarshalXR),
			},
		},
		"MarshalComposedError": {
			reason: "We should return an error if we can't marshal a composed resource.",
			args: args{
				xr: composite.New(),
				drs: DesiredResources{
					"cool-resource": DesiredResource{
						Resource: &composed.Unstructured{Unstructured: unmarshalable},
					},
				},
			},
			want: want{
				err: errors.Wrap(errUnmarshalableCD, errMarshalCD),
			},
		},
		"Success": {
			reason: "We should successfully build a FunctionIO Desired struct from our state.",
			args: args{
				xr: composite.New(composite.WithGroupVersionKind(schema.GroupVersionKind{
					Group:   "example.org",
					Version: "v1",
					Kind:    "Composite",
				})),
				xc: managed.ConnectionDetails{"a": []byte("b")},
				drs: DesiredResources{
					"cool-resource": DesiredResource{
						Resource: composed.New(composed.FromReference(corev1.ObjectReference{
							APIVersion: "example.org/v2",
							Kind:       "Composed",
							Name:       "cool-resource-42",
						})),
					},
				},
			},
			want: want{
				o: iov1alpha1.Desired{
					Composite: iov1alpha1.DesiredComposite{
						Resource: runtime.RawExtension{
							Raw: []byte(`{"apiVersion":"example.org/v1","kind":"Composite"}`),
						},
						ConnectionDetails: []iov1alpha1.ExplicitConnectionDetail{{
							Name:  "a",
							Value: "b",
						}},
					},
					Resources: []iov1alpha1.DesiredResource{{
						Name: "cool-resource",
						Resource: runtime.RawExtension{
							Raw: []byte(`{"apiVersion":"example.org/v2","kind":"Composed","metadata":{"name":"cool-resource-42"}}`),
						},
					}},
				},
			},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			d, err := FunctionIODesired(tc.args.xr, tc.args.xc, tc.args.drs)

			if diff := cmp.Diff(tc.want.err, err, test.EquateErrors()); diff != "" {
				t.Errorf("\n%s\nFunctionIODesired(...): -want, +got:\n%s", tc.reason, diff)
			}

			if diff := cmp.Diff(tc.want.o, d, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("\n%s\nFunctionIODesired(...): -want, +got:\n%s", tc.reason, diff)
			}
		})
	}
}

func TestWithKubernetesAuthentication(t *testing.T) {
	errBoom := errors.New("boom")

	type params struct {
		c              client.Reader
		namespace      string
		serviceAccount string
		registry       string
	}
	type args struct {
		ctx context.Context
		fn  *v1.ContainerFunction
		r   *fnpbv1alpha1.RunFunctionRequest
	}
	type want struct {
		r   *fnpbv1alpha1.RunFunctionRequest
		err error
	}

	cases := map[string]struct {
		reason string
		params params
		args   args
		want   want
	}{
		"GetServiceAccountError": {
			reason: "We should return an error if we can't get the service account.",
			params: params{
				c: &test.MockClient{
					MockGet: test.NewMockGetFn(errBoom),
				},
			},
			want: want{
				err: errors.Wrap(errBoom, errGetServiceAccount),
			},
		},
		"GetSecretError": {
			reason: "We should return an error if we can't get an image pull secret.",
			params: params{
				c: &test.MockClient{
					MockGet: test.NewMockGetFn(nil, func(obj client.Object) error {
						if _, ok := obj.(*corev1.Secret); ok {
							return errBoom
						}
						return nil
					},
					),
				},
			},
			args: args{
				fn: &v1.ContainerFunction{
					ImagePullSecrets: []corev1.LocalObjectReference{{Name: "cool-secret"}},
				},
			},
			want: want{
				err: errors.Wrap(errBoom, errGetImagePullSecret),
			},
		},
		"Success": {
			reason: "We should successfully parse OCI registry authentication credentials from an image pull secret.",
			params: params{
				namespace: "cool-namespace",
				c: &test.MockClient{
					MockGet: func(ctx context.Context, key client.ObjectKey, obj client.Object) error {
						if s, ok := obj.(*corev1.Secret); ok {
							want := client.ObjectKey{Namespace: "cool-namespace", Name: "cool-secret"}

							if diff := cmp.Diff(want, key); diff != "" {
								t.Errorf("\nclient.Get(...): -want key, +got key:\n%s", diff)
							}

							s.Type = corev1.SecretTypeDockerConfigJson
							s.Data = map[string][]byte{
								corev1.DockerConfigJsonKey: []byte(`{"auths":{"xpkg.example.org":{"username":"cool-user","password":"cool-pass"}}}`),
							}
						}
						return nil
					},
				},
				registry: "index.docker.io",
			},
			args: args{
				fn: &v1.ContainerFunction{
					Image:            "xpkg.example.org/cool-image:v1.0.0",
					ImagePullSecrets: []corev1.LocalObjectReference{{Name: "cool-secret"}},
				},
				r: &fnpbv1alpha1.RunFunctionRequest{},
			},
			want: want{
				r: &fnpbv1alpha1.RunFunctionRequest{
					ImagePullConfig: &fnpbv1alpha1.ImagePullConfig{
						Auth: &fnpbv1alpha1.ImagePullAuth{
							Username: "cool-user",
							Password: "cool-pass",
						},
					},
				},
			},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := WithKubernetesAuthentication(tc.params.c, tc.params.namespace, tc.params.serviceAccount, tc.params.registry)(tc.args.ctx, tc.args.fn, tc.args.r)

			if diff := cmp.Diff(tc.want.err, err, test.EquateErrors()); diff != "" {
				t.Errorf("\n%s\nWithKubernetesAuthentication(...): -want error, +got error:\n%s", tc.reason, diff)
			}

			if diff := cmp.Diff(tc.want.r, tc.args.r, protocmp.Transform()); diff != "" {
				t.Errorf("\n%s\nWithKubernetesAuthentication(...): -want, +got:\n%s", tc.reason, diff)
			}
		})
	}
}

type MockFunctionServer struct {
	fnpbv1alpha1.UnimplementedContainerizedFunctionRunnerServiceServer

	rsp *fnpbv1alpha1.RunFunctionResponse
	err error
}

func (s *MockFunctionServer) RunFunction(context.Context, *fnpbv1alpha1.RunFunctionRequest) (*fnpbv1alpha1.RunFunctionResponse, error) {
	return s.rsp, s.err
}

func TestRunFunction(t *testing.T) {
	errBoom := errors.New("boom")

	fnio := &iov1alpha1.FunctionIO{
		Desired: iov1alpha1.Desired{
			Resources: []iov1alpha1.DesiredResource{
				{Name: "cool-resource"},
			},
		},
	}
	fnioyaml, _ := yaml.Marshal(fnio)

	type params struct {
		server fnpbv1alpha1.ContainerizedFunctionRunnerServiceServer
	}

	type args struct {
		ctx  context.Context
		fnio *iov1alpha1.FunctionIO
		fn   *v1.ContainerFunction
	}

	type want struct {
		fnio *iov1alpha1.FunctionIO
		err  error
	}

	cases := map[string]struct {
		reason string
		params params
		args   args
		want   want
	}{
		"RunFunctionError": {
			reason: "We should return an error if we can't make an RPC call to run the function.",
			params: params{
				server: &MockFunctionServer{err: errBoom},
			},
			args: args{
				ctx:  context.Background(),
				fnio: &iov1alpha1.FunctionIO{},
				fn:   &v1.ContainerFunction{},
			},
			want: want{
				err: errors.Wrap(status.Errorf(codes.Unknown, errBoom.Error()), errRunFnContainer),
			},
		},
		"RunFunctionSuccess": {
			reason: "We should return the same FunctionIO our server returned.",
			params: params{
				server: &MockFunctionServer{
					rsp: &fnpbv1alpha1.RunFunctionResponse{
						Output: fnioyaml,
					},
				},
			},
			args: args{
				ctx:  context.Background(),
				fnio: &iov1alpha1.FunctionIO{},
				fn:   &v1.ContainerFunction{},
			},
			want: want{
				fnio: fnio,
			},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {

			// Listen on a random port.
			lis, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}

			wg := &sync.WaitGroup{}
			wg.Add(1)
			go func() {
				s := grpc.NewServer()
				fnpbv1alpha1.RegisterContainerizedFunctionRunnerServiceServer(s, tc.params.server)
				_ = s.Serve(lis)
				wg.Done()
			}()

			// Tell the function to connect to our mock server.
			tc.args.fn.Runner = &v1.ContainerFunctionRunner{
				Endpoint: pointer.String(lis.Addr().String()),
			}

			fnio, err := RunFunction(tc.args.ctx, tc.args.fnio, tc.args.fn)

			_ = lis.Close() // This should terminate the goroutine above.
			wg.Wait()

			if diff := cmp.Diff(tc.want.err, err, test.EquateErrors()); diff != "" {
				t.Errorf("\n%s\nRunFunction(...): -want, +got:\n%s", tc.reason, diff)
			}

			if diff := cmp.Diff(tc.want.fnio, fnio); diff != "" {
				t.Errorf("\n%s\nRunFunction(...): -want, +got:\n%s", tc.reason, diff)
			}

		})
	}
}

func TestImagePullConfig(t *testing.T) {

	always := corev1.PullAlways
	never := corev1.PullNever
	ifNotPresent := corev1.PullIfNotPresent

	cases := map[string]struct {
		reason string
		fn     *v1.ContainerFunction
		want   *fnpbv1alpha1.ImagePullConfig
	}{
		"NoImagePullPolicy": {
			reason: "We should return an empty config if there's no ImagePullPolicy.",
			fn:     &v1.ContainerFunction{},
			want:   &fnpbv1alpha1.ImagePullConfig{},
		},
		"PullAlways": {
			reason: "We should correctly map PullAlways.",
			fn: &v1.ContainerFunction{
				ImagePullPolicy: &always,
			},
			want: &fnpbv1alpha1.ImagePullConfig{
				PullPolicy: fnpbv1alpha1.ImagePullPolicy_IMAGE_PULL_POLICY_ALWAYS,
			},
		},
		"PullNever": {
			reason: "We should correctly map PullNever.",
			fn: &v1.ContainerFunction{
				ImagePullPolicy: &never,
			},
			want: &fnpbv1alpha1.ImagePullConfig{
				PullPolicy: fnpbv1alpha1.ImagePullPolicy_IMAGE_PULL_POLICY_NEVER,
			},
		},
		"PullIfNotPresent": {
			reason: "We should correctly map PullIfNotPresent.",
			fn: &v1.ContainerFunction{
				ImagePullPolicy: &ifNotPresent,
			},
			want: &fnpbv1alpha1.ImagePullConfig{
				PullPolicy: fnpbv1alpha1.ImagePullPolicy_IMAGE_PULL_POLICY_IF_NOT_PRESENT,
			},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := ImagePullConfig(tc.fn)

			if diff := cmp.Diff(tc.want, got, protocmp.Transform()); diff != "" {
				t.Errorf("\n%s\nImagePullConfig(...): -want, +got:\n%s", tc.reason, diff)
			}
		})
	}
}

func TestRunFunctionConfig(t *testing.T) {

	cpu := computeresource.MustParse("1")
	mem := computeresource.MustParse("256Mi")

	isolated := v1.ContainerFunctionNetworkPolicyIsolated
	runner := v1.ContainerFunctionNetworkPolicyRunner

	cases := map[string]struct {
		reason string
		fn     *v1.ContainerFunction
		want   *fnpbv1alpha1.RunFunctionConfig
	}{
		"EmptyConfig": {
			reason: "An empty config should be returned when there is no run-related configuration.",
			fn:     &v1.ContainerFunction{},
			want:   &fnpbv1alpha1.RunFunctionConfig{},
		},
		"Resources": {
			reason: "All resource quantities should be included in the RunFunctionConfig",
			fn: &v1.ContainerFunction{
				Resources: &v1.ContainerFunctionResources{
					Limits: &v1.ContainerFunctionResourceLimits{
						CPU:    &cpu,
						Memory: &mem,
					},
				},
			},
			want: &fnpbv1alpha1.RunFunctionConfig{
				Resources: &fnpbv1alpha1.ResourceConfig{
					Limits: &fnpbv1alpha1.ResourceLimits{
						Cpu:    cpu.String(),
						Memory: mem.String(),
					},
				},
			},
		},
		"IsolatedNetwork": {
			reason: "The isolated network policy should be returned.",
			fn: &v1.ContainerFunction{
				Network: &v1.ContainerFunctionNetwork{
					Policy: &isolated,
				},
			},
			want: &fnpbv1alpha1.RunFunctionConfig{
				Network: &fnpbv1alpha1.NetworkConfig{
					Policy: fnpbv1alpha1.NetworkPolicy_NETWORK_POLICY_ISOLATED,
				},
			},
		},
		"RunnerNetwork": {
			reason: "The runner network policy should be returned.",
			fn: &v1.ContainerFunction{
				Network: &v1.ContainerFunctionNetwork{
					Policy: &runner,
				},
			},
			want: &fnpbv1alpha1.RunFunctionConfig{
				Network: &fnpbv1alpha1.NetworkConfig{
					Policy: fnpbv1alpha1.NetworkPolicy_NETWORK_POLICY_RUNNER,
				},
			},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {

			got := RunFunctionConfig(tc.fn)

			if diff := cmp.Diff(tc.want, got, protocmp.Transform()); diff != "" {
				t.Errorf("\n%s\nRunFunctionConfig(...): -want, +got:\n%s", tc.reason, diff)
			}
		})
	}
}

func TestDeleteComposedResources(t *testing.T) {
	errBoom := errors.New("boom")

	type params struct {
		client client.Writer
	}

	type args struct {
		ctx   context.Context
		owner metav1.Object
		ors   ObservedResources
	}

	type want struct {
		err error
	}

	cases := map[string]struct {
		reason string
		params params
		args   args
		want   want
	}{
		"NeverCreated": {
			reason: "Resources that were never created should be deleted from state, but not the API server.",
			params: params{
				client: &test.MockClient{
					// We know Delete wasn't called because it's a nil function
					// and would thus panic if it was.
				},
			},
			args: args{
				ors: ObservedResources{
					"undesired-resource": ObservedResource{
						Resource: &fake.Composed{},
					},
				},
			},
			want: want{
				err: nil,
			},
		},
		"UncontrolledResource": {
			reason: "Resources the XR doesn't control should be deleted from state, but not the API server.",
			params: params{
				client: &test.MockClient{
					// We know Delete wasn't called because it's a nil function
					// and would thus panic if it was.
				},
			},
			args: args{
				owner: &fake.Composite{
					ObjectMeta: metav1.ObjectMeta{
						UID: "cool-xr",
					},
				},
				ors: ObservedResources{
					"undesired-resource": ObservedResource{
						Resource: &fake.Composed{
							ObjectMeta: metav1.ObjectMeta{
								// This resource exists in the API server.
								CreationTimestamp: metav1.Now(),
							},
						},
					},
				},
			},
			want: want{
				err: nil,
			},
		},
		"DeleteError": {
			reason: "We should return any error encountered deleting the resource.",
			params: params{
				client: &test.MockClient{
					MockDelete: test.NewMockDeleteFn(errBoom),
				},
			},
			args: args{
				owner: &fake.Composite{
					ObjectMeta: metav1.ObjectMeta{
						UID: "cool-xr",
					},
				},
				ors: ObservedResources{
					"undesired-resource": ObservedResource{
						Resource: &fake.Composed{
							ObjectMeta: metav1.ObjectMeta{
								// This resource exists in the API server.
								CreationTimestamp: metav1.Now(),

								// This resource is controlled by the XR.
								OwnerReferences: []metav1.OwnerReference{{
									Controller: pointer.Bool(true),
									UID:        "cool-xr",
								}},
							},
						},
					},
				},
			},
			want: want{
				err: errors.Wrapf(errBoom, errFmtDeleteCD, "undesired-resource", "", ""),
			},
		},
		"Success": {
			reason: "We should successfully delete the resource from the API server and state.",
			params: params{
				client: &test.MockClient{
					MockDelete: test.NewMockDeleteFn(nil),
				},
			},
			args: args{
				owner: &fake.Composite{
					ObjectMeta: metav1.ObjectMeta{
						UID: "cool-xr",
					},
				},
				ors: ObservedResources{
					"undesired-resource": ObservedResource{
						Resource: &fake.Composed{
							ObjectMeta: metav1.ObjectMeta{
								// This resource exists in the API server.
								CreationTimestamp: metav1.Now(),

								// This resource is controlled by the XR.
								OwnerReferences: []metav1.OwnerReference{{
									Controller: pointer.Bool(true),
									UID:        "cool-xr",
								}},
							},
						},
					},
				},
			},
			want: want{
				err: nil,
			},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {

			d := NewUndesiredComposedResourceDeleter(tc.params.client)
			err := d.DeleteComposedResources(tc.args.ctx, tc.args.owner, tc.args.ors)

			if diff := cmp.Diff(tc.want.err, err, test.EquateErrors()); diff != "" {
				t.Errorf("\n%s\nDeleteComposedResources(...): -want, +got:\n%s", tc.reason, diff)
			}
		})
	}
}

func TestUpdateResourceRefs(t *testing.T) {
	type args struct {
		xr  resource.ComposedResourcesReferencer
		drs DesiredResources
	}

	type want struct {
		xr resource.ComposedResourcesReferencer
	}

	cases := map[string]struct {
		reason string
		args   args
		want   want
	}{
		"Success": {
			reason: "We should return a consistently ordered set of references.",
			args: args{
				xr: &fake.Composite{},
				drs: DesiredResources{
					"never-created-c": DesiredResource{
						Resource: &fake.Composed{
							ObjectMeta: metav1.ObjectMeta{
								Name: "never-created-c-42",
							},
						},
					},
					"never-created-b": DesiredResource{
						Resource: &fake.Composed{
							ObjectMeta: metav1.ObjectMeta{
								Name: "never-created-b-42",
							},
						},
					},
					"never-created-a": DesiredResource{
						Resource: &fake.Composed{
							ObjectMeta: metav1.ObjectMeta{
								Name: "never-created-a-42",
							},
						},
					},
				},
			},
			want: want{
				xr: &fake.Composite{
					ComposedResourcesReferencer: fake.ComposedResourcesReferencer{
						Refs: []corev1.ObjectReference{
							{Name: "never-created-a-42"},
							{Name: "never-created-b-42"},
							{Name: "never-created-c-42"},
						},
					},
				},
			},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {

			UpdateResourceRefs(tc.args.xr, tc.args.drs)

			if diff := cmp.Diff(tc.want.xr, tc.args.xr); diff != "" {
				t.Errorf("\n%s\nUpdateResourceRefs(...): -want, +got:\n%s", tc.reason, diff)
			}

		})
	}
}
