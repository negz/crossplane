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

	"github.com/crossplane/crossplane-runtime/pkg/errors"
)

// An UncompressedStore is a ContainerStore that stores OCI containers, images,
// and layers. When asked to bundle a container for a new image the OverlayStore
// will cache the image's layers as uncompressed tarballs on disk. The
// container's root filesystem is then created by extracting each layer in
// sequence.
type UncompressedStore struct {
	root string

	e LayerExtractor
}

// NewUncompressedStore returns a ContainerStore that creates container
// filesystems by extracting uncompressed image layers.
func NewUncompressedStore(root string) (*UncompressedStore, error) {
	for _, p := range []string{
		filepath.Join(root, layers),
		filepath.Join(root, images),
		filepath.Join(root, containers),
	} {
		if err := os.MkdirAll(p, 0700); err != nil {
			return nil, errors.Wrap(err, errMkdir)
		}
	}

	s := &UncompressedStore{
		root: root,
		e:    NewStackingLayerExtractor(NewWhiteoutHandler(NewExtractHandler())),
	}
	return s, nil
}

// BundleContainer returns an OCI bundle ready for use by an OCI runtime. The
// supplied image will be fetched and cached in the store if it does not already
// exist there.
func (s *UncompressedStore) BundleContainer(ctx context.Context, i ociv1.Image, id string) (Bundle, error) { //nolint:gocyclo // Only at 11 right now.
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

	path := s.containerPath(d.Hex)
	if err := os.MkdirAll(filepath.Join(path, rootfs), 0700); err != nil {
		return UncompressedBundle{}, errors.Wrap(err, "cannot create rootfs dir")
	}

	if err := WriteRuntimeConfig(filepath.Join(path, config), cfg); err != nil {
		_ = os.RemoveAll(path)
		return UncompressedBundle{}, errors.Wrap(err, "cannot write OCI runtime config")
	}

	for _, l := range m.Layers {
		tb, err := os.Open(s.layerPath(l.Digest.Hex))
		if err != nil {
			_ = os.RemoveAll(path)
			return UncompressedBundle{}, errors.Wrap(err, "cannot open OCI image layer")
		}
		if err := s.e.Apply(ctx, tb, filepath.Join(path, rootfs)); err != nil {
			_ = tb.Close()
			_ = os.RemoveAll(path)
			return UncompressedBundle{}, errors.Wrap(err, "cannot apply OCI image layer")
		}
		if err := tb.Close(); err != nil {
			return UncompressedBundle{}, errors.Wrap(err, "cannot close OCI image layer")
		}
	}

	return UncompressedBundle{path: path}, nil
}

func (s *UncompressedStore) containersPath() string {
	return filepath.Join(s.root, containers)
}

func (s *UncompressedStore) containerPath(id string) string {
	return filepath.Join(s.containersPath(), id)
}

func (s *UncompressedStore) imagesPath() string {
	return filepath.Join(s.root, images)
}

func (s *UncompressedStore) imagePath(digest string) string {
	return filepath.Join(s.imagesPath(), digest)
}

func (s *UncompressedStore) layersPath() string {
	return filepath.Join(s.root, layers)
}

func (s *UncompressedStore) layerPath(digest string) string {
	return filepath.Join(s.layersPath(), digest)
}

func (s *UncompressedStore) layerExists(digest string) (bool, error) {
	path := s.layerPath(digest)
	fi, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, errors.Wrapf(err, "cannot stat path %q", path)
	}
	if !fi.Mode().IsRegular() {
		return false, errors.Errorf("path %q exists but is not a regular file", path)
	}
	// TODO(negz): Validate that the file is a tarball?
	return true, nil
}

func (s *UncompressedStore) cacheImage(ctx context.Context, i ociv1.Image) error { //nolint:gocyclo // Only a touch over (12) at the time of writing.
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
		if err := s.cacheLayer(ctx, layers[i]); err != nil {
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

func (s *UncompressedStore) imageExists(digest string) (bool, error) {
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

func (s *UncompressedStore) cacheLayer(_ context.Context, l ociv1.Layer) error {
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

	// CreateTemp creates a file with permission mode 0600.
	tmp, err := os.CreateTemp(s.layersPath(), fmt.Sprintf("wrk-%s-", d.Hex))
	if err != nil {
		return errors.Wrap(err, "cannot create temporary layer file")
	}

	if _, err := copyChunks(tmp, tarball, 1024*1024); err != nil { // Copy 1MB chunks.
		return errors.Wrap(err, "cannot write temporary layer file")
	}

	return errors.Wrap(os.Rename(tmp.Name(), s.layerPath(d.Hex)), "cannot move uncompressed layer into place")
}

// An UncompressedBundle is an OCI runtime bundle. Its root filesystem is a
// temporary extraction of its image's cached layers.
type UncompressedBundle struct {
	path string
}

// Path to the OCI bundle.
func (b UncompressedBundle) Path() string { return b.path }

// Cleanup the OCI bundle.
func (b UncompressedBundle) Cleanup() error {
	return errors.Wrap(os.RemoveAll(b.path), "cannot cleanup bundle")
}
