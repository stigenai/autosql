package migrate

import (
	"bytes"
	"os"

	"golang.org/x/sys/unix"
)

// InspectSnapshot captures a verified generation through an already opened
// directory. It never creates a lock, journal, or other file, so project input
// inspection can remain read-only. The caller retains ownership of directory.
// Concurrent publication refuses the capture; mutation still uses LoadSnapshot
// and its normal locking/CAS path.
func InspectSnapshot(directory *os.File) (Snapshot, error) {
	root, err := borrowDirectoryFD(directory)
	if err != nil {
		return Snapshot{}, err
	}
	defer unix.Close(root)
	snapshot, err := snapshotAt(root)
	if err != nil {
		return Snapshot{}, err
	}
	_, latest, err := loadAt(root)
	if err != nil {
		return Snapshot{}, err
	}
	if !bytes.Equal(latest, snapshot.ManifestBytes) {
		return Snapshot{}, conflict("snapshot_changed", ManifestFile, "retry inspection against the current published generation")
	}
	return snapshot, nil
}

// Open a fresh description without changing the caller's directory offset or
// resolving its pathname again. The returned descriptor belongs to the caller.
func borrowDirectoryFD(directory *os.File) (int, error) {
	if directory == nil {
		return -1, ErrInvalid
	}
	root, err := unix.Openat(int(directory.Fd()), ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return -1, err
	}
	if err := validateDirFD(root, "."); err != nil {
		unix.Close(root)
		return -1, err
	}
	return root, nil
}

// PrefixSnapshot reconstructs the exact input generation before a generated
// migration. Its digest can be bound before adding the newly signed artifact,
// avoiding a self-reference through that artifact's own migration entry.
// The complete snapshot is verified first, including entries after the prefix.
func PrefixSnapshot(snapshot Snapshot, count int) (Snapshot, error) {
	if count < 0 || count > len(snapshot.Manifest.Entries) {
		return Snapshot{}, ErrInvalid
	}
	files, err := validatedSnapshotFiles(snapshot)
	if err != nil {
		return Snapshot{}, err
	}
	prefix, err := build(files[:count])
	if err != nil {
		return Snapshot{}, err
	}
	result := Snapshot{Manifest: prefix, ManifestBytes: canonical(prefix), Files: map[string][]byte{}}
	for _, file := range files[:count] {
		result.Files[file.Name] = bytes.Clone(file.SQL)
		if file.ArtifactName != "" {
			result.Files[file.ArtifactName] = bytes.Clone(file.Artifact)
		}
	}
	return result, nil
}

func validatedSnapshotFiles(snapshot Snapshot) ([]File, error) {
	parsed, err := strictManifest(snapshot.ManifestBytes)
	if err != nil || !bytes.Equal(canonical(parsed), canonical(snapshot.Manifest)) {
		return nil, ErrInvalid
	}
	files := snapshotFiles(snapshot)
	full, err := build(files)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(canonical(full), snapshot.ManifestBytes) || !snapshotHasExactFiles(snapshot) {
		return nil, conflict("content_changed", ManifestFile, "restore the byte-exact migration generation")
	}
	return files, nil
}

func snapshotHasExactFiles(snapshot Snapshot) bool {
	expected := 0
	for _, entry := range snapshot.Manifest.Entries {
		if _, ok := snapshot.Files[entry.File]; !ok {
			return false
		}
		expected++
		if entry.ArtifactFile != "" {
			if _, ok := snapshot.Files[entry.ArtifactFile]; !ok {
				return false
			}
			expected++
		}
	}
	return len(snapshot.Files) == expected
}
