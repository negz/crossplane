package oci

import (
	"context"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// A BasicFetcher fetches OCI images. It doesn't support private registries.
type BasicFetcher struct{}

// Fetch fetches a package image.
func (i *BasicFetcher) Fetch(ctx context.Context, ref name.Reference, _ ...string) (v1.Image, error) {
	return remote.Image(ref, remote.WithContext(ctx))
}

// Head fetches a package descriptor.
func (i *BasicFetcher) Head(ctx context.Context, ref name.Reference, _ ...string) (*v1.Descriptor, error) {
	return remote.Head(ref, remote.WithContext(ctx))
}

// Tags fetches a package's tags.
func (i *BasicFetcher) Tags(ctx context.Context, ref name.Reference, _ ...string) ([]string, error) {
	return remote.List(ref.Context(), remote.WithContext(ctx))
}
