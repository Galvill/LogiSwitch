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
