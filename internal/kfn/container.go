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

package kfn

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"

	"google.golang.org/grpc"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/utils/pointer"

	"github.com/crossplane/crossplane-runtime/pkg/errors"
	"github.com/crossplane/crossplane-runtime/pkg/logging"
	"github.com/crossplane/crossplane/apis/apiextensions/fn/proto/v1alpha1"
)

// Error strings.
const (
	errListen = "cannot listen for gRPC connections"
	errServe  = "cannot serve gRPC API"
)

const containerName = "kfn"

// An ContainerRunner runs a Composition Function packaged as an OCI image by
// extracting it and running it as a 'rootless' container.
type ContainerRunner struct {
	v1alpha1.UnimplementedContainerizedFunctionRunnerServiceServer

	cfg   *rest.Config
	codec runtime.ParameterCodec

	// Namespace in which to run Jobs.
	namespace string

	log logging.Logger
}

// A ContainerRunnerOption configures a new ContainerRunner.
type ContainerRunnerOption func(*ContainerRunner)

// WithNamespace configures which namespace Jobs will be run in. It defaults to
// 'default'.
func WithNamespace(namespace string) ContainerRunnerOption {
	return func(cr *ContainerRunner) {
		cr.namespace = namespace
	}
}

// WithLogger configures which logger the container runner should use. Logging
// is disabled by default.
func WithLogger(l logging.Logger) ContainerRunnerOption {
	return func(cr *ContainerRunner) {
		cr.log = l
	}
}

// NewContainerRunner returns a new Runner that runs functions as rootless
// containers.
func NewContainerRunner(cfg *rest.Config, o ...ContainerRunnerOption) *ContainerRunner {

	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	c := runtime.NewParameterCodec(s)

	r := &ContainerRunner{cfg: cfg, codec: c, namespace: corev1.NamespaceDefault, log: logging.NewNopLogger()}
	for _, fn := range o {
		fn(r)
	}

	return r
}

// ListenAndServe gRPC connections at the supplied address.
func (r *ContainerRunner) ListenAndServe(network, address string) error {
	r.log.Debug("Listening", "network", network, "address", address)
	lis, err := net.Listen(network, address)
	if err != nil {
		return errors.Wrap(err, errListen)
	}

	// TODO(negz): Limit concurrent function runs?
	srv := grpc.NewServer()
	v1alpha1.RegisterContainerizedFunctionRunnerServiceServer(srv, r)
	return errors.Wrap(srv.Serve(lis), errServe)
}

// Stdio can be used to read and write a command's standard I/O.
type Stdio struct {
	Stdin  io.WriteCloser
	Stdout io.ReadCloser
	Stderr io.ReadCloser
}

// RunFunction runs a function as a rootless OCI container. Functions that
// return non-zero, or that cannot be executed in the first place (e.g. because
// they cannot be fetched from the registry) will return an error.
func (r *ContainerRunner) RunFunction(ctx context.Context, req *v1alpha1.RunFunctionRequest) (*v1alpha1.RunFunctionResponse, error) {
	r.log.Debug("Running function", "image", req.Image)

	client, err := kubernetes.NewForConfig(r.cfg)
	if err != nil {
		return nil, errors.Wrap(err, "cannot create Kubernetes client")
	}

	job, err := client.BatchV1().Jobs(r.namespace).Create(ctx, AsKubernetesJob(req), metav1.CreateOptions{})
	if err != nil {
		return nil, errors.Wrap(err, "cannot run function")
	}

	w, err := client.BatchV1().Jobs(r.namespace).Watch(ctx, metav1.ListOptions{
		Watch:           true,
		ResourceVersion: job.ResourceVersion,
		FieldSelector:   fields.Set{"metadata.name": job.GetName()}.String(),
	})
	if err != nil {
		return nil, errors.Wrap(err, "cannot wait for pod to start")
	}
	if err := WaitForReady(ctx, w); err != nil {
		return nil, errors.Wrap(err, "pod did not start")
	}

	l, err := client.CoreV1().Pods(r.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: labels.Set{"job-name": job.GetName()}.String(),
	})
	if err != nil || len(l.Items) != 1 {
		return nil, errors.Wrap(err, "cannot get pods")
	}
	pod := l.Items[0]

	ar := client.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(pod.GetName()).
		Namespace(pod.GetNamespace()).
		SubResource("attach").
		VersionedParams(&corev1.PodAttachOptions{
			Container: containerName,
			Stdin:     true,
			Stdout:    true,
		}, r.codec)

	attach, err := remotecommand.NewSPDYExecutor(r.cfg, http.MethodPost, ar.URL())
	if err != nil {
		return nil, errors.Wrap(err, "cannot create FunctionIO executor")
	}

	stdin := bytes.NewReader(req.Input)
	stdout := &bytes.Buffer{}
	if err := attach.StreamWithContext(ctx, remotecommand.StreamOptions{Stdin: stdin, Stdout: stdout}); err != nil {
		// TODO(negz): Presumably the container will terminate soon after it
		// finishes reading its stdin. Will that trigger this error?
		return nil, errors.Wrap(err, "cannot read and write FunctionIO")
	}

	return &v1alpha1.RunFunctionResponse{Output: stdout.Bytes()}, nil
}

// WaitForReady returns nil when the watched Job has one ready Pod. It stops the
// watch and returns an error if the supplied context is done. It also returns
// an error if the watch channel is closed before the context is done.
func WaitForReady(ctx context.Context, w watch.Interface) error {
	for {
		select {
		case <-ctx.Done():
			w.Stop()
			return ctx.Err()
		case e, ok := <-w.ResultChan():
			if !ok {
				return errors.New("result channel closed unexpectedly")
			}
			j := e.Object.(*batchv1.Job)
			if pointer.Int32Deref(j.Status.Ready, 0) == 1 {
				return nil
			}
		}
	}
}

// AsKubernetesJob returns the supplied RunFunctionRequest as a Kubernetes job.
func AsKubernetesJob(req *v1alpha1.RunFunctionRequest) *batchv1.Job {
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "kfn-",
		},
		Spec: batchv1.JobSpec{
			Completions:             pointer.Int32(1),  // Run one pod.
			BackoffLimit:            pointer.Int32(0),  // Don't retry.
			TTLSecondsAfterFinished: pointer.Int32(60), // Give us time to read stdout.
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						// TODO(negz): Create NetworkPolicy resources to enforce
						// these.
						"kfn.crossplane.io/network-policy": req.GetRunFunctionConfig().GetNetwork().GetPolicy().String(),
					},
				},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{{
						Name:  containerName,
						Image: req.GetImage(),
						SecurityContext: &corev1.SecurityContext{
							// TODO(negz): Run as non-root?
							ReadOnlyRootFilesystem: pointer.Bool(true),
							Capabilities: &corev1.Capabilities{
								Drop: []corev1.Capability{"ALL"},
							},
							SeccompProfile: &corev1.SeccompProfile{
								Type: corev1.SeccompProfileTypeRuntimeDefault,
							},
						},
						Stdin:     true,
						StdinOnce: true,
					}},
				},
			},
		},
	}

	if cfg := req.GetImagePullConfig(); cfg != nil {
		switch cfg.GetPullPolicy() {
		case v1alpha1.ImagePullPolicy_IMAGE_PULL_POLICY_ALWAYS:
			job.Spec.Template.Spec.Containers[0].ImagePullPolicy = corev1.PullAlways
		case v1alpha1.ImagePullPolicy_IMAGE_PULL_POLICY_NEVER:
			job.Spec.Template.Spec.Containers[0].ImagePullPolicy = corev1.PullNever
		case v1alpha1.ImagePullPolicy_IMAGE_PULL_POLICY_IF_NOT_PRESENT:
			job.Spec.Template.Spec.Containers[0].ImagePullPolicy = corev1.PullIfNotPresent
		case v1alpha1.ImagePullPolicy_IMAGE_PULL_POLICY_UNSPECIFIED:
			// Do nothing.
		}

		// TODO(negz): Handle ImagePullAuth.
	}

	if cfg := req.GetRunFunctionConfig(); cfg != nil {
		if t := int64(cfg.GetTimeout().AsDuration().Seconds()); t != 0 {
			job.Spec.ActiveDeadlineSeconds = &t
			job.Spec.Template.Spec.ActiveDeadlineSeconds = &t
		}

		if cpu, err := resource.ParseQuantity(cfg.GetResources().GetLimits().GetCpu()); err == nil {
			job.Spec.Template.Spec.Containers[0].Resources.Limits[corev1.ResourceCPU] = cpu
		}

		if memory, err := resource.ParseQuantity(cfg.GetResources().GetLimits().GetMemory()); err == nil {
			job.Spec.Template.Spec.Containers[0].Resources.Limits[corev1.ResourceMemory] = memory
		}
	}

	return job
}
