package tcpserver

import (
	"os"
	"path/filepath"
	"testing"

	"entry_profile_creation/internal/config"
	"entry_profile_creation/internal/model"
)

// writeAvatarFile needs only the uploads config, so it can be tested without
// MySQL or Redis. It is worth testing directly because it is the one place the
// service touches the filesystem, and a mistake there is a broken image for a
// real user rather than a failed request.
func newFileTestServer(t *testing.T) *Server {
	t.Helper()
	return &Server{cfg: &config.Config{
		Uploads: config.UploadsConfig{Root: t.TempDir(), MaxBytes: 2 << 20},
	}}
}

func TestWriteAvatarFileShardsAndWrites(t *testing.T) {
	s := newFileTestServer(t)

	const (
		userID  = uint64(1_234_567)
		version = uint32(4)
		suffix  = "0f1e2d3c"
	)
	data := []byte("\xff\xd8\xff\xe0 pretend jpeg")

	if err := s.writeAvatarFile(userID, version, suffix, data); err != nil {
		t.Fatalf("writeAvatarFile: %v", err)
	}

	// The path must match what model derives, or the URL handed to the browser
	// points somewhere the file is not.
	want := filepath.Join(s.cfg.Uploads.Root,
		filepath.FromSlash(model.AvatarRelPath(userID, version, suffix)))

	got, err := os.ReadFile(want)
	if err != nil {
		t.Fatalf("read back %s: %v", want, err)
	}
	if string(got) != string(data) {
		t.Errorf("content = %q, want %q", got, data)
	}

	// Sharded by id>>14 so no directory has to hold 10M entries.
	if shard := filepath.Base(filepath.Dir(filepath.Dir(want))); shard != "75" {
		t.Errorf("shard directory = %q, want %q (1234567>>14)", shard, "75")
	}
}

// Readable by the static file server, not 0600 as os.CreateTemp leaves it.
func TestWriteAvatarFileIsWorldReadable(t *testing.T) {
	s := newFileTestServer(t)

	if err := s.writeAvatarFile(7, 1, "aabbccdd", []byte("x")); err != nil {
		t.Fatalf("writeAvatarFile: %v", err)
	}

	path := filepath.Join(s.cfg.Uploads.Root,
		filepath.FromSlash(model.AvatarRelPath(7, 1, "aabbccdd")))
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o644 {
		t.Errorf("mode = %v, want 0644", perm)
	}
}

// A second upload writes a new versioned file and leaves the old one in place.
// Those orphans are expected: the version bump is what busts browser caches,
// and a sweeper reclaims the old files later.
func TestWriteAvatarFileVersionsSideBySide(t *testing.T) {
	s := newFileTestServer(t)

	if err := s.writeAvatarFile(7, 1, "aaaaaaaa", []byte("v1")); err != nil {
		t.Fatalf("v1: %v", err)
	}
	if err := s.writeAvatarFile(7, 2, "bbbbbbbb", []byte("v2")); err != nil {
		t.Fatalf("v2: %v", err)
	}

	for _, tc := range []struct {
		version uint32
		suffix  string
		want    string
	}{{1, "aaaaaaaa", "v1"}, {2, "bbbbbbbb", "v2"}} {
		path := filepath.Join(s.cfg.Uploads.Root,
			filepath.FromSlash(model.AvatarRelPath(7, tc.version, tc.suffix)))
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read v%d: %v", tc.version, err)
		}
		if string(got) != tc.want {
			t.Errorf("v%d content = %q, want %q", tc.version, got, tc.want)
		}
	}
}

// No .tmp files may survive a successful write; under load they would be a
// slow disk leak inside the shard directories.
func TestWriteAvatarFileLeavesNoTempFiles(t *testing.T) {
	s := newFileTestServer(t)

	if err := s.writeAvatarFile(7, 1, "aabbccdd", []byte("x")); err != nil {
		t.Fatalf("writeAvatarFile: %v", err)
	}

	err := filepath.WalkDir(s.cfg.Uploads.Root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && filepath.Ext(path) == ".tmp" {
			t.Errorf("leftover temp file: %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
}
