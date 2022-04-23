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

package oci

import (
	"context"
	"fmt"
	"os"

	ociv1 "github.com/google/go-containerregistry/pkg/v1"

	"github.com/crossplane/crossplane-runtime/pkg/errors"
)

// Store paths.
// Shorter is better, to avoid passing too much data to the mount syscall.
const (
	layers     = "l" // TODO(negz): Maybe diffs, to distinguish from uncompressed layer tarballs?
	images     = "i"
	containers = "c"
)

// Store paths.
const (
	rootfs   = "rootfs"
	tmpfs    = "tmpfs"
	manifest = "manifest.json"
	config   = "config.json"
)

// A ContainerStore stores OCI containers
type ContainerStore interface {
	BundleContainer(ctx context.Context, i ociv1.Image, id string) (Bundle, error)
}

// A Bundle for use by an OCI runtime.
type Bundle interface {
	Path() string
	Cleanup() error
}

// NOTE(negz): ImageWorkdir is a very thin abstraction os.MkTempdir; it only
// exists for symmetry with LayerWorkDir, which does a few more things.

type ImageWorkdir struct {
	Path string
}

func NewImageWorkdir(dir, digest string) (ImageWorkdir, error) {
	tmp, err := os.MkdirTemp(dir, fmt.Sprintf("xfn-wrk-%s-%s-", images, digest))
	return ImageWorkdir{Path: tmp}, errors.Wrap(err, "cannot create temp dir")
}

func (d ImageWorkdir) Cleanup() error {
	return errors.Wrap(os.RemoveAll(d.Path), "cannot remove workdir")
}
