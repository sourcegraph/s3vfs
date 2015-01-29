package s3vfs

import (
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"net/url"
	"os"
	pathpkg "path"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"

	"sourcegraph.com/sourcegraph/rwvfs"
)

func TestS3VFS(t *testing.T) {
	// Requires the test bucket to exist.
	//   export S3_TEST_BUCKET_URL=https://rwvfs-test-sqs.s3-us-west-2.amazonaws.com
	s3URL, _ := url.Parse(os.Getenv("S3_TEST_BUCKET_URL"))

	tests := []struct {
		fs   rwvfs.FileSystem
		path string
	}{
		{S3(s3URL, nil), "/foo2"},
	}
	for _, test := range tests {
		testWrite(t, test.fs, test.path)
		testStat(t, test.fs, "/qux")
		testGlob(t, test.fs)
	}
}

func testGlob(t *testing.T, fs rwvfs.FileSystem) {
	label := fmt.Sprintf("%T", fs)

	files := []string{"x/y/0.txt", "x/y/1.txt", "x/2.txt"}
	for _, file := range files {
		err := rwvfs.MkdirAll(fs, filepath.Dir(file))
		if err != nil {
			t.Fatalf("%s: MkdirAll: %s", label, err)
		}
		w, err := fs.Create(file)
		if err != nil {
			t.Errorf("%s: Create(%q): %s", label, file, err)
			return
		}
		_, err = w.Write([]byte("x"))
		if err != nil {
			t.Fatal(err)
		}
		err = w.Close()
		if err != nil {
			t.Fatal(err)
		}
	}

	globTests := []struct {
		prefix  string
		pattern string
		matches []string
	}{
		{"", "x/y/*.txt", []string{"x/y/0.txt", "x/y/1.txt"}},
		{"x/y", "x/y/*.txt", []string{"x/y/0.txt", "x/y/1.txt"}},
		{"", "x/*", []string{"x/y", "x/2.txt"}},
	}
	for _, test := range globTests {
		matches, err := rwvfs.Glob(walkableFileSystem{fs}, test.prefix, test.pattern)
		if err != nil {
			t.Errorf("%s: Glob(prefix=%q, pattern=%q): %s", label, test.prefix, test.pattern, err)
			continue
		}
		sort.Strings(test.matches)
		sort.Strings(matches)
		if !reflect.DeepEqual(matches, test.matches) {
			t.Errorf("%s: Glob(prefix=%q, pattern=%q): got %v, want %v", label, test.prefix, test.pattern, matches, test.matches)
		}
	}
}

func testStat(t *testing.T, fs rwvfs.FileSystem, path string) {
	label := fmt.Sprintf("Stat %T", fs)

	cases := []struct {
		parent, child string
		checkDirs     []string
	}{
		{pathpkg.Join(path, "p"), pathpkg.Join(path, "p/c"), nil},
		{pathpkg.Join(path, "."), pathpkg.Join(path, "c"), nil},
		{pathpkg.Join(path, "p1/p2"), pathpkg.Join(path, "p1/p2/p3/c"), []string{"p1", "p1/p2/p3"}},
		{pathpkg.Join(path, "p1"), pathpkg.Join(path, "p1/p2/p3/c"), []string{"p1/p2", "p1/p2/p3"}},
	}

	// Clean out bucket.
	for _, x := range cases {
		removeFile(t, fs, x.parent)
		removeFile(t, fs, x.child)
	}
	removeFile(t, fs, path)

	if path != "." {
		if _, err := fs.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("%s: Stat(%s): got error %v, want os.IsNotExist-satisfying", label, path, err)
		}
	}
	if _, err := fs.Stat(path + "/z"); !os.IsNotExist(err) {
		t.Fatalf("%s: Stat(%s): got error %v, want os.IsNotExist-satisfying", label, path+"/z", err)
	}

	createFile := func(path string) {
		w, err := fs.Create(path)
		if err != nil {
			t.Fatalf("%s: Create(%s): %s", label, path, err)
		}
		if _, err := w.Write([]byte("x")); err != nil {
			t.Fatalf("%s: Write(%s): %s", label, path, err)
		}
		if err := w.Close(); err != nil {
			t.Fatalf("%s: w.Close(): %s", label, err)
		}
	}

	// Not sure of the best way to treat S3 keys that are
	// delimiter-prefixes of other keys, since they can either be like
	// dirs or files. But let's just choose a way and add a test so we
	// can change the behavior easily later.

	for _, x := range cases {
		t.Logf("# parent %q, child %q", x.parent, x.child)

		createFile(x.parent)
		createFile(x.child)

		parentFI, err := fs.Stat(x.parent)
		if err != nil {
			t.Fatalf("%s: Stat(%s): %s", label, x.parent, err)
		}
		if !parentFI.Mode().IsDir() {
			t.Fatalf("%s: Stat(%s) got Mode().IsDir() == false, want true", label, x.parent)
		}

		childFI, err := fs.Stat(x.child)
		if err != nil {
			t.Fatalf("%s: Stat(%s): %s", label, x.child, err)
		}
		if !childFI.Mode().IsRegular() {
			t.Fatalf("%s: Stat(%s) got Mode().IsRegular() == false, want true", label, x.child)
		}

		// Should not exist.
		doesntExist := pathpkg.Join(x.child, "doesntexist")
		if _, err := fs.Stat(doesntExist); !os.IsNotExist(err) {
			t.Fatalf("%s: Stat(%s): got error %v, want os.IsNotExist-satisfying", label, doesntExist, err)
		}

		for _, dir := range x.checkDirs {
			dir = pathpkg.Join(path, dir)
			fi, err := fs.Stat(dir)
			if err != nil {
				t.Fatalf("%s: Stat(%s): %s", label, dir, err)
			}
			if !fi.Mode().IsDir() {
				t.Fatalf("%s: Stat(%s): not dir, want dir", label, dir)
			}
		}

		if x.parent != "." {
			// Check that the parent file can be opened like a file.
			f, err := fs.Open(x.parent)
			if err != nil {
				t.Fatalf("%s: Open(%s): %s", label, x.parent, err)
			}
			f.Close()
		}

		// Clean up
		if err := fs.Remove(x.parent); err != nil {
			t.Errorf("%s: Remove(%q): %s", label, x.parent, err)
		}
		if err := fs.Remove(x.child); err != nil {
			t.Errorf("%s: Remove(%q): %s", label, x.child, err)
		}
	}
}

func testWrite(t *testing.T, fs rwvfs.FileSystem, path string) {
	label := fmt.Sprintf("%T", fs)

	w, err := fs.Create(path)
	if err != nil {
		t.Fatalf("%s: WriterOpen: %s", label, err)
	}

	input := []byte("qux")
	_, err = w.Write(input)
	if err != nil {
		t.Fatalf("%s: Write: %s", label, err)
	}

	err = w.Close()
	if err != nil {
		t.Fatalf("%s: w.Close: %s", label, err)
	}

	var r io.ReadCloser
	r, err = fs.Open(path)
	if err != nil {
		t.Fatalf("%s: Open: %s", label, err)
	}
	var output []byte
	output, err = ioutil.ReadAll(r)
	if err != nil {
		t.Fatalf("%s: ReadAll: %s", label, err)
	}
	if !bytes.Equal(output, input) {
		t.Errorf("%s: got output %q, want %q", label, output, input)
	}

	r, err = fs.Open(path)
	if err != nil {
		t.Fatalf("%s: Open: %s", label, err)
	}
	output, err = ioutil.ReadAll(r)
	if err != nil {
		t.Fatalf("%s: ReadAll: %s", label, err)
	}
	if !bytes.Equal(output, input) {
		t.Errorf("%s: got output %q, want %q", label, output, input)
	}

	if err := fs.Remove(path); err != nil {
		t.Errorf("%s: Remove(%q): %s", label, path, err)
	}
	time.Sleep(time.Second)

	fi, err := fs.Stat(path)
	if err != nil && !os.IsNotExist(err) {
		t.Errorf("%s: Stat(%q): want os.IsNotExist-satisfying error, got %q", label, path, err)
	} else if err == nil {
		t.Errorf("%s: Stat(%q): want file to not exist, got existing file with FileInfo %+v", label, path, fi)
	}
}

func removeFile(t *testing.T, fs rwvfs.FileSystem, path string) {
	if err := fs.Remove(path); err != nil {
		t.Fatalf("removeFile(%q): %s", path, err)
	}
}

type walkableFileSystem struct{ rwvfs.FileSystem }

func (_ walkableFileSystem) Join(elem ...string) string { return filepath.Join(elem...) }
