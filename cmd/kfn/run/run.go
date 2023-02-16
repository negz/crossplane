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

// Package run implements a convenience CLI to run and test Composition Functions.
package run

import (
	"context"
	"os"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/crossplane/crossplane-runtime/pkg/errors"

	"github.com/crossplane/crossplane/apis/apiextensions/fn/proto/v1alpha1"
	"github.com/crossplane/crossplane/internal/kfn"
)

// Error strings
const (
	errWriteFIO    = "cannot write FunctionIO YAML to stdout"
	errRunFunction = "cannot run function"
)

// Command runs a Composition function.
type Command struct {
	CacheDir        string        `short:"c" help:"Directory used for caching function images and containers." default:"/xfn"`
	Timeout         time.Duration `help:"Maximum time for which the function may run before being killed." default:"30s"`
	ImagePullPolicy string        `help:"Whether the image may be pulled from a remote registry." enum:"Always,Never,IfNotPresent" default:"IfNotPresent"`
	NetworkPolicy   string        `help:"Whether the function may access the network." enum:"Runner,Isolated" default:"Isolated"`
	Namespace       string        `help:"Kubernetes namespace in which to run functions." default:"default"`
	Kubecfg         string        `help:"Kubernetes client config file." default:"$HOME/.kube/config"`

	// TODO(negz): filecontent appears to take multiple args when it does not.
	// Bump kong once https://github.com/alecthomas/kong/issues/346 is fixed.

	Image      string `arg:"" help:"OCI image to run."`
	FunctionIO []byte `arg:"" help:"YAML encoded FunctionIO to pass to the function." type:"filecontent"`
}

// Run a Composition container function.
func (c *Command) Run() error {

	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		&clientcmd.ClientConfigLoadingRules{ExplicitPath: os.ExpandEnv(c.Kubecfg)},
		&clientcmd.ConfigOverrides{},
	).ClientConfig()
	if err != nil {
		return errors.Wrapf(err, "cannot load %s", c.Kubecfg)
	}

	f := kfn.NewContainerRunner(cfg, kfn.WithNamespace(c.Namespace))
	rsp, err := f.RunFunction(context.Background(), &v1alpha1.RunFunctionRequest{
		Image: c.Image,
		Input: c.FunctionIO,
		ImagePullConfig: &v1alpha1.ImagePullConfig{
			PullPolicy: pullPolicy(c.ImagePullPolicy),
		},
		RunFunctionConfig: &v1alpha1.RunFunctionConfig{
			Timeout: durationpb.New(c.Timeout),
			Network: &v1alpha1.NetworkConfig{
				Policy: networkPolicy(c.NetworkPolicy),
			},
		},
	})
	if err != nil {
		return errors.Wrap(err, errRunFunction)
	}

	_, err = os.Stdout.Write(rsp.GetOutput())
	return errors.Wrap(err, errWriteFIO)
}

func pullPolicy(p string) v1alpha1.ImagePullPolicy {
	switch p {
	case "Always":
		return v1alpha1.ImagePullPolicy_IMAGE_PULL_POLICY_ALWAYS
	case "Never":
		return v1alpha1.ImagePullPolicy_IMAGE_PULL_POLICY_NEVER
	case "IfNotPresent":
		fallthrough
	default:
		return v1alpha1.ImagePullPolicy_IMAGE_PULL_POLICY_IF_NOT_PRESENT
	}
}

func networkPolicy(p string) v1alpha1.NetworkPolicy {
	switch p {
	case "Runner":
		return v1alpha1.NetworkPolicy_NETWORK_POLICY_RUNNER
	case "Isolated":
		fallthrough
	default:
		return v1alpha1.NetworkPolicy_NETWORK_POLICY_ISOLATED
	}
}
