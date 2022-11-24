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

// Package spark runs a Composition Function. It is designed to be run as root
// inside an unprivileged user namespace.
package spark

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/uuid"

	"github.com/crossplane/crossplane-runtime/pkg/errors"
	"github.com/crossplane/crossplane/internal/oci"
)

// Error strings.
const (
	errOpenConfigFile   = "cannot open OCI config file"
	errDecodeConfigFile = "cannot decode OCI config file"
	errCloseConfigFile  = "cannot close OCI config file"
	errMkdir            = "cannot make directory"
	errReadlink         = "cannot read symlink"
	errRuntime          = "cannot invoke OCI runtime"
	errCopySource       = "cannot copy source rootfs"
	errOpenDst          = "cannot open destination file"
	errOpenSrc          = "cannot open source file"
	errCopy             = "cannot copy file"
	errCloseDst         = "cannot close destination file"
	errCloseSrc         = "cannot close source file"
	errChownDst         = "cannot change owner of destination file"
	errInvalidInput     = "invalid function input"
	errInvalidOutput    = "invalid function output"
	errBadReference     = "OCI tag is not a valid reference"
	errHeadImg          = "cannot fetch OCI image descriptor"
	errExecFn           = "cannot execute function"
	errFetchFn          = "cannot fetch function from registry"
	errLookupFn         = "cannot lookup function in store"
	errWriteFn          = "cannot write function to store"
	errDeleteBundle     = "cannot delete OCI bundle"
	errChownFd          = "cannot chown file descriptor"
)

// The path within the cache dir that the OCI runtime should use for its
// '--root' cache.
const ociRuntimeRoot = "runtime"

// Command runs a containerized Composition Function.
type Command struct {
	CacheDir string        `short:"c" help:"Directory used for caching function images and containers." default:"/xfn"`
	Config   string        `help:"OCI config file, relative to root of the bundle." default:"config.json"`
	Runtime  string        `help:"OCI runtime binary to invoke." default:"crun"`
	Timeout  time.Duration `help:"Maximum time for which the function may run before being killed." default:"25s"`

	Image string `arg:"" help:"Function OCI image to run."`
}

// Run a Composition Function inside an unprivileged user namespace.
func (c *Command) Run() error {
	ctx, cancel := context.WithTimeout(context.Background(), c.Timeout)
	defer cancel()

	ref, err := name.ParseReference(c.Image)
	if err != nil {
		return errors.Wrap(err, errBadReference)
	}

	r := &oci.BasicFetcher{}
	runID := uuid.NewString()

	// We prefer to use an overlayfs based store where possible. Both stores use
	// approximately the same amount of disk to store images, but the overlay
	// store uses less space (and disk I/O) when creating a filesystem for a
	// container. This is because the overlay store can create a container
	// filesystem by creating an overlay atop its image layers, while the
	// uncompressed store must extract said layers to create a new filesystem.
	var s oci.ContainerStore
	s, err = oci.NewUncompressedStore(c.CacheDir)
	if oci.SupportsOverlay(c.CacheDir) {
		s, err = oci.NewOverlayStore(c.CacheDir)
	}
	if err != nil {
		return errors.Wrap(err, "cannot setup overlay store")
	}

	// TODO(negz): Is it worth using r.Head to determine whether the image is
	// already in the store? This would be cheaper than using r.Fetch, but
	// r.Fetch is only grabbing the manifest (not the layers) and is thus
	// presumably not super expensive.
	// TODO(negz): Respect the ImagePullPolicy.
	img, ferr := r.Fetch(ctx, ref)
	if ferr != nil {
		return errors.Wrap(ferr, errFetchFn)
	}
	b, err := s.BundleContainer(ctx, img, runID)
	if err != nil {
		return errors.Wrap(err, errLookupFn)
	}

	root := filepath.Join(c.CacheDir, ociRuntimeRoot)
	if err := os.MkdirAll(root, 0700); err != nil {
		_ = b.Cleanup()
		return errors.Wrap(err, errMkdir)
	}

	// TODO(negz): Consider using the OCI runtimes lifecycle management commands
	// (i.e create, start, and delete) rather than run. This would allow spark
	// to return without sitting in-between xfn and crun.

	//nolint:gosec // Executing with user-supplied input is intentional.
	cmd := exec.CommandContext(ctx, c.Runtime, "--root="+root, "run", "--bundle="+b.Path(), runID)
	cmd.Stdin = os.Stdin
	cmd.Stderr = os.Stderr
	cmd.Stdout = os.Stdout

	if err := cmd.Run(); err != nil {
		_ = b.Cleanup()
		return errors.Wrap(err, errRuntime)
	}

	return errors.Wrap(b.Cleanup(), errDeleteBundle)
}
