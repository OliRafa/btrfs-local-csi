package driver

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestResolveHandle(t *testing.T) {
	tests := []struct {
		name      string
		namespace string
		pvcName   string
		override  string
		want      string
		wantErr   bool
	}{
		{name: "defaults to the pvc name", namespace: "localflix", pvcName: "localflix-library", want: "localflix/localflix-library"},
		{name: "override replaces the leaf", namespace: "localflix", pvcName: "localflix-library", override: "media", want: "localflix/media"},
		{name: "dots are allowed", namespace: "localflix", pvcName: "my.volume", want: "localflix/my.volume"},

		{name: "override cannot traverse", namespace: "localflix", pvcName: "x", override: "../pirate-bay/loot", wantErr: true},
		{name: "override cannot be dotdot", namespace: "localflix", pvcName: "x", override: "..", wantErr: true},
		{name: "override cannot be dot", namespace: "localflix", pvcName: "x", override: ".", wantErr: true},
		{name: "override cannot nest", namespace: "localflix", pvcName: "x", override: "a/b", wantErr: true},
		{name: "override cannot be absolute", namespace: "localflix", pvcName: "x", override: "/etc/shadow", wantErr: true},
		{name: "override cannot be the trash dir", namespace: "localflix", pvcName: "x", override: trashDir, wantErr: true},
		{name: "override cannot be uppercase", namespace: "localflix", pvcName: "x", override: "Media", wantErr: true},
		{name: "namespace cannot traverse", namespace: "..", pvcName: "x", wantErr: true},
		{name: "namespace cannot be empty", namespace: "", pvcName: "x", wantErr: true},
		{name: "pvc name cannot be empty", namespace: "localflix", pvcName: "", wantErr: true},
		{name: "leaf cannot exceed the length limit", namespace: "localflix", pvcName: strings.Repeat("a", maxSegmentLen+1), wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolveHandle(tc.namespace, tc.pvcName, tc.override)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ResolveHandle(%q, %q, %q) = %q, want an error", tc.namespace, tc.pvcName, tc.override, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveHandle: %v", err)
			}
			if got != tc.want {
				t.Errorf("handle = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestValidateHandleRejectsMalformed(t *testing.T) {
	for _, handle := range []string{
		"",
		"no-slash",
		"/leading",
		"trailing/",
		"../escape/x",
		"ns/../../etc",
		"ns/sub/dir",
		"ns//name",
	} {
		if err := ValidateHandle(handle); err == nil {
			t.Errorf("ValidateHandle(%q) = nil, want an error", handle)
		}
	}
}

func TestVolumePath(t *testing.T) {
	got, err := VolumePath("/pool", "localflix/library")
	if err != nil {
		t.Fatalf("VolumePath: %v", err)
	}
	if want := "/pool/localflix/library"; got != want {
		t.Errorf("VolumePath = %q, want %q", got, want)
	}

	if _, err := VolumePath("/pool", "../etc/shadow"); err == nil {
		t.Error("VolumePath accepted a traversing handle")
	}
}

// A symlink inside the pool is the one way a syntactically valid handle can
// still land outside it, so the guard that catches it gets a real filesystem.
func TestResolvedVolumePathRejectsSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	pool := filepath.Join(root, "pool")
	outside := filepath.Join(root, "outside")
	for _, dir := range []string{filepath.Join(pool, "localflix"), outside} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}

	escape := filepath.Join(pool, "localflix", "escape")
	if err := os.Symlink(outside, escape); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	if _, err := ResolvedVolumePath(pool, "localflix/escape"); err == nil {
		t.Fatal("ResolvedVolumePath followed a symlink out of the pool")
	}

	honest := filepath.Join(pool, "localflix", "library")
	if err := os.Mkdir(honest, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	got, err := ResolvedVolumePath(pool, "localflix/library")
	if err != nil {
		t.Fatalf("ResolvedVolumePath on an ordinary directory: %v", err)
	}
	if want, _ := filepath.EvalSymlinks(honest); got != want {
		t.Errorf("ResolvedVolumePath = %q, want %q", got, want)
	}
}

func TestTrashPathIsFlatAndTimestamped(t *testing.T) {
	at := time.Date(2026, 8, 3, 22, 45, 1, 0, time.UTC)
	got := TrashPath("/pool", "localflix/library", at)

	if want := "/pool/.trash/localflix-library-20260803T224501Z"; got != want {
		t.Errorf("TrashPath = %q, want %q", got, want)
	}
}
