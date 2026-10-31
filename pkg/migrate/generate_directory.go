package migrate

import "golang.org/x/sys/unix"

func generationSnapshot(r GenerateRequest) (Snapshot, error) {
	if r.DirectoryHandle == nil {
		return LoadSnapshot(r.Directory)
	}
	root, err := borrowDirectoryFD(r.DirectoryHandle)
	if err != nil {
		return Snapshot{}, err
	}
	defer unix.Close(root)
	lock, err := openLock(root, false)
	if err != nil {
		return Snapshot{}, err
	}
	defer closeLock(lock)
	return snapshotAt(root)
}

func (s GenerateService) publishGeneration(r GenerateRequest, update UpdateRequest) (Manifest, error) {
	if r.DirectoryHandle == nil {
		return UpdateWithOps(r.Directory, update, s.Ops)
	}
	root, err := borrowDirectoryFD(r.DirectoryHandle)
	if err != nil {
		return Manifest{}, err
	}
	defer unix.Close(root)
	lock, err := openLockWithOps(root, true, s.Ops)
	if err != nil {
		return Manifest{}, err
	}
	defer closeLockWithOps(lock, s.Ops)
	return updateLocked(root, r.Directory, update, s.Ops, false, nil)
}
