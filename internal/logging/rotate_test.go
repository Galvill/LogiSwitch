package logging

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestRotateKeepsOneOld(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "x.log")
	w, err := OpenRotating(path, 10)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	writes := []string{"aaaaaaaa\n", "bbbbbbbb\n", "cccccccc\n"}
	for _, s := range writes {
		if _, err := w.Write([]byte(s)); err != nil {
			t.Fatal(err)
		}
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 2 {
		t.Fatalf("want 2 files, got %d", len(entries))
	}
	cur, _ := os.ReadFile(path)
	if string(cur) != writes[2] {
		t.Fatalf("current = %q", cur)
	}
	old, err := os.ReadFile(path + ".1")
	if err != nil || string(old) != writes[1] {
		t.Fatalf("old = %q err=%v", old, err)
	}
}

func TestAppendsAcrossOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.log")
	for _, s := range []string{"one\n", "two\n"} {
		w, err := OpenRotating(path, 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(s)); err != nil {
			t.Fatal(err)
		}
		w.Close()
	}
	b, _ := os.ReadFile(path)
	if string(b) != "one\ntwo\n" {
		t.Fatalf("got %q", b)
	}
}

func TestOversizedSingleWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.log")
	w, err := OpenRotating(path, 5)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if _, err := w.Write([]byte("hi\n")); err != nil {
		t.Fatal(err)
	}
	big := strings.Repeat("z", 50)
	if _, err := w.Write([]byte(big)); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	if string(b) != big {
		t.Fatalf("got %q", b)
	}
	o, _ := os.ReadFile(path + ".1")
	if string(o) != "hi\n" {
		t.Fatalf("old %q", o)
	}
}

func TestWriteAfterClose(t *testing.T) {
	w, err := OpenRotating(filepath.Join(t.TempDir(), "x.log"), 100)
	if err != nil {
		t.Fatal(err)
	}
	w.Close()
	if _, err := w.Write([]byte("x")); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("got %v", err)
	}
}

func TestConcurrentWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.log")
	w, err := OpenRotating(path, 100)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				if _, err := w.Write([]byte("line of text\n")); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()
}

func TestDefaultPath(t *testing.T) {
	t.Setenv("ProgramData", "/pd")
	if got, want := DefaultPath(), filepath.Join("/pd", "LogiSwitch", "logiswitch.log"); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestRotationFailureStaysBounded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.log")
	if err := os.MkdirAll(filepath.Join(path+".1", "keep"), 0o700); err != nil {
		t.Fatal(err)
	}
	w, err := OpenRotating(path, 100)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	line := []byte(strings.Repeat("a", 19) + "\n")
	for i := 0; i < 200; i++ {
		if _, err := w.Write(line); err != nil {
			t.Fatal(err)
		}
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Size() > 2*100+int64(len(line)) {
			t.Fatalf("file grew to %d", fi.Size())
		}
	}
}

func TestSymlinkReplaced(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "x.log")
	if err := os.Symlink(target, path); err != nil {
		t.Skip(err)
	}
	w, err := OpenRotating(path, 100)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if _, err := w.Write([]byte("log\n")); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(target); string(b) != "secret" {
		t.Fatalf("target modified: %q", b)
	}
	fi, err := os.Lstat(path)
	if err != nil || !fi.Mode().IsRegular() {
		t.Fatalf("path not regular: %v %v", fi, err)
	}
}
