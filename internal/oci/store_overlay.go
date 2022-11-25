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
	"path/filepath"

	ociv1 "github.com/google/go-containerregistry/pkg/v1"
	runtime "github.com/opencontainers/runtime-spec/specs-go"

	"github.com/crossplane/crossplane-runtime/pkg/errors"
)

// Common overlayfs directories.
const (
	overlayDirTmpfs  = "tmpfs"
	overlayDirUpper  = "upper"
	overlayDirWork   = "work"
	overlayDirLower  = "lower"  // Only used when there are no parent layers.
	overlayDirMerged = "merged" // Only used when generating diff layers.
)

// SupportsOverlay returns true if the supplied cacheRoot supports the overlay
// filesystem. Notably overlayfs was not supported in unprivileged user
// namespaces until Linux kernel 5.11. It's also not possible to create an
// overlayfs where the upper dir is itself on an overlayfs (i.e. is on a
// container's root filesystem).
// https://github.com/torvalds/linux/commit/459c7c565ac36ba09ffbf
func SupportsOverlay(cacheRoot string) bool {
	// We use NewLayerWorkdir to test because it needs to create an upper dir on
	// the same filesystem as the supplied cacheRoot in order to be able to move
	// it into place as a cached layer. NewOverlayBundle creates an upper dir on
	// a tmpfs, and is thus supported in some cases where NewLayerWorkdir isn't.
	w, err := NewLayerWorkdir(cacheRoot, "supports-overlay-test", []string{})
	if err != nil {
		return false
	}
	if err := w.Cleanup(); err != nil {
		return false
	}
	return true
}

// TODO(negz): Consider caching the result of SupportsOverlay (e.g. by writing a
// .cachetype file) to avoid invoking it every time we run a function.

// An OverlayStore is a ContainerStore that stores OCI containers, images, and
// layers. When asked to bundle a container for a new image the OverlayStore
// will extract and cache the image's layers as files on disk. The container's
// root filesystem is then created as an overlay atop the image's layers. The
// upper layer of this overlay is stored in memory on a tmpfs, and discarded
// once the container has finished running.
type OverlayStore struct {
	root string
	e    LayerExtractor
}

// NewOverlayStore returns a ContainerStore that creates container filesystems
// as overlays on their image's layers.
func NewOverlayStore(root string) (*OverlayStore, error) {
	for _, p := range []string{
		filepath.Join(root, layers),
		filepath.Join(root, images),
		filepath.Join(root, containers),
	} {
		if err := os.MkdirAll(p, 0700); err != nil {
			return nil, errors.Wrap(err, errMkdir)
		}
	}

	s := &OverlayStore{
		root: root,
		e:    NewStackingLayerExtractor(NewWhiteoutHandler(NewExtractHandler())),
	}
	return s, nil
}

// BundleContainer returns an OCI bundle ready for use by an OCI runtime. The
// supplied image will be fetched and cached in the store if it does not already
// exist there.
func (s *OverlayStore) BundleContainer(ctx context.Context, i ociv1.Image, id string) (Bundle, error) {
	if err := s.cacheImage(ctx, i); err != nil {
		return nil, errors.Wrap(err, "cannot cache OCI image")
	}

	d, err := i.Digest()
	if err != nil {
		return nil, errors.Wrap(err, "cannot get image digest")
	}
	// The image's manifest should already be populated; this won't pull.
	m, err := i.Manifest()
	if err != nil {
		return nil, errors.Wrap(err, "cannot get OCI image manifest")
	}

	lowerPaths := make([]string, len(m.Layers))
	for i := range m.Layers {
		lowerPaths[i] = s.layerPath(m.Layers[i].Digest.Hex)
	}

	// Create an OCI runtime config file from our cached OCI image config file.
	// We do this every time we run the function because in future it's likely
	// that we'll want to derive the OCI runtime config file from both the OCI
	// image config file and user supplied input (i.e. from the functions array
	// of a Composition). Using the i.Config() or i.RawConfig() methods would
	// cause the config file to be pulled from a remote registry; we use the
	// config file we cached to disk instead.
	cfg, err := ReadRuntimeConfig(filepath.Join(s.imagePath(d.Hex), config))
	if err != nil {
		return nil, errors.Wrap(err, "cannot create OCI runtime config")
	}

	b, err := NewOverlayBundle(s.containerPath(id), cfg, lowerPaths)
	return b, errors.Wrap(err, "cannot create bundle for container")
}

func (s *OverlayStore) containersPath() string {
	return filepath.Join(s.root, containers)
}

func (s *OverlayStore) containerPath(id string) string {
	return filepath.Join(s.containersPath(), id)
}

func (s *OverlayStore) cacheImage(ctx context.Context, i ociv1.Image) error { //nolint:gocyclo // Only a touch over (12) at the time of writing.
	d, err := i.Digest()
	if err != nil {
		return errors.Wrap(err, "cannot get image digest")
	}

	// Check whether all of the image's layers appear to exist in the cache. If
	// the image is in the cache there's a good chance its layers are, but it's
	// cheap to double-check.
	layers, err := i.Layers()
	if err != nil {
		return errors.Wrap(err, "cannot get image layers")
	}

	for i := range layers {
		if err := s.cacheLayer(ctx, layers[i], layers[:i]...); err != nil {
			return errors.Wrap(err, "cannot cache image layer")
		}
	}

	exists, err := s.imageExists(d.Hex)
	if err != nil {
		return errors.Wrap(err, "cannot determine whether image exists in cache")
	}
	if exists {
		// Image is already in the cache; nothing to do.
		return nil
	}

	w, err := NewImageWorkdir(s.imagesPath(), d.Hex)
	if err != nil {
		return errors.Wrap(err, "cannot create image work dir")
	}
	defer w.Cleanup() //nolint:errcheck // Not much we can do if this fails.

	m, err := i.RawManifest()
	if err != nil {
		return errors.Wrap(err, "cannot get image manifest")
	}

	if err := os.WriteFile(filepath.Join(w.Path, manifest), m, 0600); err != nil {
		return errors.Wrap(err, "cannot write image manifest")
	}

	c, err := i.RawConfigFile()
	if err != nil {
		return errors.Wrap(err, "cannot get image config")
	}

	if err := os.WriteFile(filepath.Join(w.Path, config), c, 0600); err != nil {
		return errors.Wrap(err, "cannot write image config")
	}

	return errors.Wrap(os.Rename(w.Path, s.imagePath(d.Hex)), "cannot move image into place")
}

func (s *OverlayStore) imagesPath() string {
	return filepath.Join(s.root, images)
}

func (s *OverlayStore) imagePath(digest string) string {
	return filepath.Join(s.imagesPath(), digest)
}

func (s *OverlayStore) imageExists(digest string) (bool, error) {
	path := s.imagePath(digest)
	fi, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, errors.Wrapf(err, "cannot stat path %q", path)
	}
	if !fi.IsDir() {
		return false, errors.Errorf("path %q exists but is not a directory", path)
	}
	// TODO(negz): Check whether this image has a config.json and manifest.json
	// at the expected paths?
	return true, nil
}

// cacheLayer caches the supplied layer as a directory of files suitable for use
// as an overlayfs lower layer. In order to do this it must convert OCI whiteout
// files to overlay whiteout files. It's not possible to _directly_ create
// overlay whiteout files in an unprivileged user namespace because doing so
// requires CAP_MKNOD in the 'root' or 'initial' user namespace - whiteout files
// are actually character devices per "whiteouts and opaque directories" at
// https://www.kernel.org/doc/Documentation/filesystems/overlayfs.txt
//
// We can however indirectly create overlay whiteout files in an unprivileged
// user namespace by creating an overlay where the parent OCI layers are the
// lower overlayfs layers, and applying the layer to be cached to said fs. Doing
// so will produce an upper overlayfs layer that we can cache. This layer will
// be a valid lower layer (complete with overlay whiteout files) for either
// subsequent layers from the OCI image, or the final container root filesystem
// layer.
func (s *OverlayStore) cacheLayer(ctx context.Context, l ociv1.Layer, parents ...ociv1.Layer) error {
	d, err := l.Digest()
	if err != nil {
		return errors.Wrap(err, "cannot get layer digest")
	}

	exists, err := s.layerExists(d.Hex)
	if err != nil {
		return errors.Wrap(err, "cannot determine whether layer exists in cache")
	}
	if exists {
		// No need to cache this layer; we already did.
		return nil
	}

	// This call to Uncompressed is what actually pulls the layer.
	tarball, err := l.Uncompressed()
	if err != nil {
		return errors.Wrap(err, "cannot fetch and extract layer")
	}

	parentPaths := make([]string, len(parents))
	for i := range parents {
		d, err := parents[i].Digest()
		if err != nil {
			return errors.Wrap(err, "cannot get parent layer digest")
		}
		parentPaths[i] = s.layerPath(d.Hex)
	}

	lw, err := NewLayerWorkdir(s.layersPath(), d.Hex, parentPaths)
	if err != nil {
		return errors.Wrap(err, "cannot create layer work dir")
	}

	if err := s.e.Apply(ctx, tarball, lw.ApplyPath()); err != nil {
		_ = lw.Cleanup()
		return errors.Wrap(err, "cannot untar layer")
	}

	if err := os.Rename(lw.ResultPath(), s.layerPath(d.Hex)); err != nil {
		_ = lw.Cleanup()
		return errors.Wrap(err, "cannot move layer into place")
	}

	return errors.Wrap(lw.Cleanup(), "cannot cleanup layer work dir")
}

func (s *OverlayStore) layersPath() string {
	return filepath.Join(s.root, layers)
}

func (s *OverlayStore) layerPath(digest string) string {
	return filepath.Join(s.layersPath(), digest)
}

func (s *OverlayStore) layerExists(digest string) (bool, error) {
	path := s.layerPath(digest)
	fi, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, errors.Wrapf(err, "cannot stat path %q", path)
	}
	if !fi.IsDir() {
		return false, errors.Errorf("path %q exists but is not a directory", path)
	}
	// TODO(negz): Is there anything else we can do to validate that this is
	// really a layer? Currently it contains an extracted tarball at its root.
	return true, nil
}

// An OverlayBundle is an OCI runtime bundle. Its root filesystem is a
// temporary overlay atop its image's cached layers.
type OverlayBundle struct {
	path    string
	tmpfs   TmpFSMount
	overlay OverlayMount
}

// NewOverlayBundle creates and returns an OCI runtime bundle with a root
// filesystem backed by a temporary (tmpfs) overlay atop the supplied lower
// layer paths.
func NewOverlayBundle(dir string, cfg *runtime.Spec, parentLayerPaths []string) (OverlayBundle, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return OverlayBundle{}, errors.Wrap(err, "cannot create bundle dir")
	}

	if err := os.Mkdir(filepath.Join(dir, overlayDirTmpfs), 0700); err != nil {
		_ = os.RemoveAll(dir)
		return OverlayBundle{}, errors.Wrap(err, errMkdir)
	}

	tm := TmpFSMount{Mountpoint: filepath.Join(dir, overlayDirTmpfs)}
	if err := tm.Mount(); err != nil {
		_ = os.RemoveAll(dir)
		return OverlayBundle{}, errors.Wrap(err, "cannot mount workdir tmpfs")
	}

	for _, p := range []string{
		filepath.Join(dir, overlayDirTmpfs, overlayDirUpper),
		filepath.Join(dir, overlayDirTmpfs, overlayDirWork),
		filepath.Join(dir, rootfs),
	} {
		if err := os.Mkdir(p, 0700); err != nil {
			_ = os.RemoveAll(dir)
			return OverlayBundle{}, errors.Wrapf(err, "cannot create %s dir", p)
		}
	}

	om := OverlayMount{
		Lower:      parentLayerPaths,
		Upper:      filepath.Join(dir, overlayDirTmpfs, overlayDirUpper),
		Work:       filepath.Join(dir, overlayDirTmpfs, overlayDirWork),
		Mountpoint: filepath.Join(dir, rootfs),
	}
	if err := om.Mount(); err != nil {
		_ = os.RemoveAll(dir)
		return OverlayBundle{}, errors.Wrap(err, "cannot mount workdir overlayfs")
	}

	if err := WriteRuntimeConfig(filepath.Join(dir, config), cfg); err != nil {
		_ = os.RemoveAll(dir)
		return OverlayBundle{}, errors.Wrap(err, "cannot write OCI runtime config")
	}

	return OverlayBundle{tmpfs: tm, overlay: om, path: dir}, nil
}

// Path to the OCI bundle.
func (b OverlayBundle) Path() string { return b.path }

// Cleanup the OCI bundle.
func (b OverlayBundle) Cleanup() error {
	if err := b.overlay.Unmount(); err != nil {
		return errors.Wrap(err, "cannot unmount bundle overlayfs")
	}
	if err := b.tmpfs.Unmount(); err != nil {
		return errors.Wrap(err, "cannot unmount bundle tmpfs")
	}
	return errors.Wrap(os.RemoveAll(b.path), "cannot remove bundle")
}

// A TmpFSMount represents a mount of type tmpfs.
type TmpFSMount struct {
	Mountpoint string
}

// An OverlayMount represents a mount of type overlay.
type OverlayMount struct {
	Mountpoint string
	Lower      []string
	Upper      string
	Work       string
}

// A LayerWorkdir is a temporary directory used to produce an overlayfs layer
// from an OCI layer by applying the OCI layer to a temporary overlay mount.
type LayerWorkdir struct {
	OverlayMount

	path string
}

// NewLayerWorkdir returns a temporary directory used to produce an overlayfs
// layer from an OCI layer.
func NewLayerWorkdir(dir, digest string, parentLayerPaths []string) (LayerWorkdir, error) {
	tmp, err := os.MkdirTemp(dir, fmt.Sprintf("wrk-%s-", digest))
	if err != nil {
		return LayerWorkdir{}, errors.Wrap(err, "cannot create temp dir")
	}

	for _, d := range []string{overlayDirMerged, overlayDirUpper, overlayDirLower, overlayDirWork} {
		if err := os.Mkdir(filepath.Join(tmp, d), 0700); err != nil {
			_ = os.RemoveAll(tmp)
			return LayerWorkdir{}, errors.Wrapf(err, "cannot create %s dir", d)
		}
	}

	w := LayerWorkdir{
		OverlayMount: OverlayMount{
			Lower:      []string{filepath.Join(tmp, overlayDirLower)},
			Upper:      filepath.Join(tmp, overlayDirUpper),
			Work:       filepath.Join(tmp, overlayDirWork),
			Mountpoint: filepath.Join(tmp, overlayDirMerged),
		},
		path: tmp,
	}

	if len(parentLayerPaths) != 0 {
		w.Lower = parentLayerPaths
	}

	if err := w.Mount(); err != nil {
		_ = os.RemoveAll(tmp)
		return LayerWorkdir{}, errors.Wrap(err, "cannot mount workdir overlayfs")
	}

	return w, nil
}

// ApplyPath returns the path an OCI layer should be applied (i.e. extracted) to
// in order to create an overlayfs layer.
func (d LayerWorkdir) ApplyPath() string {
	return filepath.Join(d.path, overlayDirMerged)
}

// ResultPath returns the path of the resulting overlayfs layer.
func (d LayerWorkdir) ResultPath() string {
	return filepath.Join(d.path, overlayDirUpper)
}

// Cleanup the temporary directory.
func (d LayerWorkdir) Cleanup() error {
	if err := d.Unmount(); err != nil {
		return errors.Wrap(err, "cannot unmount workdir overlayfs")
	}
	return errors.Wrap(os.RemoveAll(d.path), "cannot remove workdir")
}
