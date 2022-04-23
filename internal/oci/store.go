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
	"io"
	"os"

	ociv1 "github.com/google/go-containerregistry/pkg/v1"

	"github.com/crossplane/crossplane-runtime/pkg/errors"
)

// Store paths.
// Shorter is better, to avoid passing too much data to the mount syscall when
// creating an overlay mount with many layers as lower directories.
const (
	layers     = "l" // TODO(negz): Maybe diffs, to distinguish from uncompressed layer tarballs?
	images     = "i"
	containers = "c"
)

// Store paths.
const (
	rootfs   = "rootfs"
	manifest = "manifest.json"
	config   = "config.json"
)

// A ContainerStore stores OCI containers.
type ContainerStore interface {
	// BundleContainer returns an OCI bundle ready for use by an OCI runtime.
	BundleContainer(ctx context.Context, i ociv1.Image, id string) (Bundle, error)
}

// A Bundle for use by an OCI runtime.
type Bundle interface {
	// Path of the OCI bundle.
	Path() string

	// Cleanup the OCI bundle after the container has finished running.
	Cleanup() error
}

// A LayerExtractor extracts an OCI layer.
// https://github.com/opencontainers/image-spec/blob/v1.0/layer.md
type LayerExtractor interface {
	// Apply the supplied tarball - an OCI filesystem layer - to the supplied
	// root directory. Applying all of an image's layers, in the correct order,
	// should produce the image's "flattened" filesystem.
	Apply(ctx context.Context, tb io.Reader, root string) error
}

// ImageWorkdir is a 'work' directory that may be used to cache an OCI image.
// The work directory should be moved into place in the store only when it has
// been successfully populated.
type ImageWorkdir struct {
	// Path of the work dir.
	Path string
}

// NewImageWorkdir creates a 'work' directory that may be used to cache an OCI
// image.
func NewImageWorkdir(dir, digest string) (ImageWorkdir, error) {
	tmp, err := os.MkdirTemp(dir, fmt.Sprintf("xfn-wrk-%s-%s-", images, digest))
	return ImageWorkdir{Path: tmp}, errors.Wrap(err, "cannot create temp dir")
}

// Cleanup deletes the work directory.
func (d ImageWorkdir) Cleanup() error {
	return errors.Wrap(os.RemoveAll(d.Path), "cannot remove workdir")
}
