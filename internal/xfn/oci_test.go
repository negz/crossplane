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

package xfn

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"io/fs"
	"testing"

	"github.com/google/go-cmp/cmp"
	ociv1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/fake"
	"github.com/spf13/afero"

	"github.com/crossplane/crossplane-runtime/pkg/errors"
	"github.com/crossplane/crossplane-runtime/pkg/test"
)

func TestStoreLookup(t *testing.T) {
	type args struct {
		ctx context.Context
		id  string
	}
	type want struct {
		cfg    *ociv1.ConfigFile
		fsPath string
		err    error
	}

	cases := map[string]struct {
		reason string
		s      *Store
		args   args
		want   want
	}{
		"ConfigNotFound": {
			reason: "We should return an error that satisfies IsNotFound if the config file was not found in the store.",
			s:      NewStore("/store", WithFS(&afero.MemMapFs{})),
			args: args{
				id: "coolfn",
			},
			want: want{
				err: errNotFound{errors.Wrap(errors.New("open /store/coolfn.json: file does not exist"), errOpenFile)},
			},
		},
		"ParseConfigError": {
			reason: "We should return an error that satisfies IsNotFound if the config file was not found in the store.",
			s: NewStore("/store", WithFS(func() *afero.MemMapFs {
				afs := &afero.MemMapFs{}
				afero.WriteFile(afs, "/store/coolfn"+configFileSuffix, []byte("I'm different!"), 0755)
				return afs
			}())),
			args: args{
				id: "coolfn",
			},
			want: want{
				err: errors.Wrap(errors.New("invalid character 'I' looking for beginning of value"), errReadFile),
			},
		},
		"FilesystemNotFound": {
			reason: "We should return an error that satisfies IsNotFound if the filesystem was not found in the store.",
			s: NewStore("/store", WithFS(func() *afero.MemMapFs {
				afs := &afero.MemMapFs{}
				afero.WriteFile(afs, "/store/coolfn"+configFileSuffix, []byte("{}"), 0755)
				return afs
			}())),
			args: args{
				id: "coolfn",
			},
			want: want{
				err: errNotFound{errors.Errorf(errFmtFsNotFound, "/store/coolfn")},
			},
		},
		"Successful": {
			reason: "We should return a config file and a filesystem path if they exist in the store.",
			s: NewStore("/store", WithFS(func() *afero.MemMapFs {
				afs := &afero.MemMapFs{}
				afs.MkdirAll("/store/coolfn", 0755)
				afero.WriteFile(afs, "/store/coolfn"+configFileSuffix, []byte(`{"author":"negz"}`), 0755)
				return afs
			}())),
			args: args{
				id: "coolfn",
			},
			want: want{
				cfg:    &ociv1.ConfigFile{Author: "negz"},
				fsPath: "/store/coolfn",
			},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			cfg, fsPath, err := tc.s.Lookup(tc.args.ctx, tc.args.id)

			if diff := cmp.Diff(tc.want.cfg, cfg); diff != "" {
				t.Errorf("%s\ns.Lookup(...): -want cfg, +got cfg:\n%s", tc.reason, diff)
			}

			if diff := cmp.Diff(tc.want.fsPath, fsPath); diff != "" {
				t.Errorf("%s\ns.Lookup(...): -want fsPath, +got fsPath:\n%s", tc.reason, diff)
			}

			if diff := cmp.Diff(tc.want.err, err, test.EquateErrors()); diff != "" {
				t.Errorf("%s\ns.Lookup(...): -want error, +got error:\n%s", tc.reason, diff)
			}
		})
	}
}

func TestStoreWrite(t *testing.T) {
	type args struct {
		ctx context.Context
		id  string
		img ociv1.Image
	}

	cases := map[string]struct {
		reason string
		s      *Store
		args   args
		want   error
	}{
		"TempDirError": {
			reason: "We should return an error if we can't create a temporary dir.",
			s:      NewStore("/store", WithFS(afero.NewReadOnlyFs(&afero.MemMapFs{}))),
			want:   errors.Wrap(errors.New("operation not permitted"), errMakeFnTmpDir),
		},
		// TODO(negz): Test that the config file and filesystem were written?
		"Successful": {
			reason: "We should not return an error if we successfully write the image filesystem and config file to the store.",
			s:      NewStore("/store", WithFS(&afero.MemMapFs{})),
			args: args{
				ctx: context.Background(),
				id:  "coolfn",
				img: &fake.FakeImage{},
			},
			want: nil,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := tc.s.Write(tc.args.ctx, tc.args.id, tc.args.img)
			if diff := cmp.Diff(tc.want, err, test.EquateErrors()); diff != "" {
				t.Errorf("%s\ns.Write(...): -want error, +got error:\n%s", tc.reason, diff)
			}
		})
	}
}

func TestUntar(t *testing.T) {
	simple := tarball(t, func() *afero.MemMapFs {
		afs := &afero.MemMapFs{}
		afs.Mkdir("/empty", 0755)
		afs.Mkdir("/files", 0755)
		afero.WriteFile(afs, "/files/one", []byte("one!"), 0644)
		afero.WriteFile(afs, "/files/two", []byte("two!"), 0666)
		return afs
	}())

	empty := tarball(t, func() *afero.MemMapFs {
		afs := &afero.MemMapFs{}
		return afs
	}())

	type args struct {
		ctx context.Context
		tb  io.Reader
		fs  afero.Fs
		dir string
	}

	type want struct {
		tb  []byte
		err error
	}

	cases := map[string]struct {
		reason string
		args   args
		want   want
	}{
		"SimpleFilesystem": {
			reason: "It should be possible untar a tarball of a simple filesystem",
			args: args{
				ctx: context.Background(),
				tb:  simple,
				fs:  &afero.MemMapFs{},
				dir: "/",
			},
			want: want{
				tb: simple.Bytes(),
			},
		},
		"ContextCancelled": {
			reason: "We should return an error if the context is cancelled",
			args: args{
				ctx: func() context.Context {
					ctx, cancel := context.WithCancel(context.Background())
					cancel()
					return ctx
				}(),
				tb:  empty,
				fs:  &afero.MemMapFs{},
				dir: "/",
			},
			want: want{
				tb:  empty.Bytes(),
				err: errors.New("context canceled"),
			},
		},
		"NotATarball": {
			reason: "We should return an error if the passed reader is not a tarball",
			args: args{
				ctx: context.Background(),
				tb:  bytes.NewReader([]byte("I'm different!")),
				fs:  &afero.MemMapFs{},
				dir: "/",
			},
			want: want{
				tb:  empty.Bytes(),
				err: errors.Wrap(errors.New("unexpected EOF"), errAdvanceTarball),
			},
		},
		"InvalidPath": {
			reason: "We should return an error if the tarball contains an invalid path",
			args: args{
				ctx: context.Background(),
				tb: func() io.Reader {
					b := &bytes.Buffer{}
					tw := tar.NewWriter(b)
					tw.WriteHeader(&tar.Header{Name: "../escape"})
					return b
				}(),
				fs:  &afero.MemMapFs{},
				dir: "/",
			},
			want: want{
				tb:  empty.Bytes(),
				err: errors.Errorf(errFmtInvalidPath, "../escape"),
			},
		},
		"DirMkdirAllError": {
			reason: "We should return an error if we can't make a directory",
			args: args{
				ctx: context.Background(),
				tb: func() io.Reader {
					b := &bytes.Buffer{}
					tw := tar.NewWriter(b)
					tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: "/dir"})
					return b
				}(),
				fs:  afero.NewReadOnlyFs(&afero.MemMapFs{}),
				dir: "/",
			},
			want: want{
				tb:  empty.Bytes(),
				err: errors.Wrap(errors.New("operation not permitted"), errMkdir),
			},
		},
		"FileMkdirAllError": {
			reason: "We should return an error if we can't make a directory for a file",
			args: args{
				ctx: context.Background(),
				tb: func() io.Reader {
					b := &bytes.Buffer{}
					tw := tar.NewWriter(b)
					tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: "/dir/file"})
					return b
				}(),
				fs:  afero.NewReadOnlyFs(&afero.MemMapFs{}),
				dir: "/",
			},
			want: want{
				tb:  empty.Bytes(),
				err: errors.Wrap(errors.New("operation not permitted"), errMkdir),
			},
		},
		"UnsupportedMode": {
			reason: "We should return an error if we don't support the mode of a file in the tarball",
			args: args{
				ctx: context.Background(),
				tb: func() io.Reader {
					b := &bytes.Buffer{}
					tw := tar.NewWriter(b)
					tw.WriteHeader(&tar.Header{Typeflag: tar.TypeBlock, Name: "/dev/unsupported"})
					return b
				}(),
				fs:  afero.NewReadOnlyFs(&afero.MemMapFs{}),
				dir: "/",
			},
			want: want{
				tb:  empty.Bytes(),
				err: errors.Errorf(errFmtUnsupportedMode, "/dev/unsupported", fs.ModeDevice),
			},
		},
		// TODO(negz): Full coverage on untar? Some of the error cases relating
		// to opening and copying files are tough to trigger, even with afero.
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := untar(tc.args.ctx, tc.args.tb, tc.args.fs, tc.args.dir)
			if diff := cmp.Diff(tc.want.err, err, test.EquateErrors()); diff != "" {
				t.Errorf("%s\nuntar(...): -want error, +got error:\n%s", tc.reason, diff)
			}

			got := tarball(t, tc.args.fs).Bytes()
			if diff := cmp.Diff(tc.want.tb, got); diff != "" {
				t.Errorf("%s\nuntar(...): -want, +got:\n%s", tc.reason, diff)
			}
		})
	}

}

// tarball returns a tarball of all regular files and directories in the
// supplied filesystem. It's used to create tarballs to test the untar function.
func tarball(t *testing.T, afs afero.Fs) *bytes.Buffer {
	t.Helper()

	b := &bytes.Buffer{}
	tw := tar.NewWriter(b)

	afero.Walk(afs, "/", func(path string, info fs.FileInfo, err error) error {
		if err != nil {
			return err
		}

		switch {
		case info.Mode().IsDir():
			hdr := &tar.Header{
				Typeflag: tar.TypeDir,
				Name:     path,
			}
			if err := tw.WriteHeader(hdr); err != nil {
				t.Fatal(err)
			}
		case info.Mode().IsRegular():
			hdr := &tar.Header{
				Typeflag: tar.TypeReg,
				Name:     path,
				Mode:     int64(info.Mode()),
				Size:     info.Size(),
			}
			if err := tw.WriteHeader(hdr); err != nil {
				t.Fatal(err)
			}
			f, err := afs.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := io.Copy(tw, f); err != nil {
				t.Fatal(err)
			}
		}

		return nil
	})

	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	return b
}
