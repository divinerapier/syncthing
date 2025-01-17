// Copyright (C) 2014 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package model

import (
	"io"
	"time"

	"github.com/pkg/errors"

	"github.com/syncthing/syncthing/lib/fs"
	"github.com/syncthing/syncthing/lib/protocol"
	"github.com/syncthing/syncthing/lib/sync"
)

// A sharedPullerState is kept for each file that is being synced and is kept
// updated along the way.
type sharedPullerState struct {
	// Immutable, does not require locking
	file        protocol.FileInfo // The new file (desired end state)
	fs          fs.Filesystem
	folder      string
	tempName    string
	realName    string
	reused      int // Number of blocks reused from temporary file
	ignorePerms bool
	hasCurFile  bool              // Whether curFile is set
	curFile     protocol.FileInfo // The file as it exists now in our database
	sparse      bool
	created     time.Time

	// Mutable, must be locked for access
	err               error           // The first error we hit
	writer            *lockedWriterAt // Wraps fd to prevent fd closing at the same time as writing
	copyTotal         int             // Total number of copy actions for the whole job
	pullTotal         int             // Total number of pull actions for the whole job
	copyOrigin        int             // Number of blocks copied from the original file
	copyOriginShifted int             // Number of blocks copied from the original file but shifted
	copyNeeded        int             // Number of copy actions still pending
	pullNeeded        int             // Number of block pulls still pending
	updated           time.Time       // Time when any of the counters above were last updated
	closed            bool            // True if the file has been finalClosed.
	available         []int32         // Indexes of the blocks that are available in the temporary file
	availableUpdated  time.Time       // Time when list of available blocks was last updated
	mut               sync.RWMutex    // Protects the above
}

// A momentary state representing the progress of the puller
type pullerProgress struct {
	Total                   int   `json:"total"`
	Reused                  int   `json:"reused"`
	CopiedFromOrigin        int   `json:"copiedFromOrigin"`
	CopiedFromOriginShifted int   `json:"copiedFromOriginShifted"`
	CopiedFromElsewhere     int   `json:"copiedFromElsewhere"`
	Pulled                  int   `json:"pulled"`
	Pulling                 int   `json:"pulling"`
	BytesDone               int64 `json:"bytesDone"`
	BytesTotal              int64 `json:"bytesTotal"`
}

// lockedWriterAt adds a lock to protect from closing the fd at the same time as writing.
// WriteAt() is goroutine safe by itself, but not against for example Close().
type lockedWriterAt struct {
	mut sync.RWMutex
	fd  fs.File
}

// WriteAt itself is goroutine safe, thus just needs to acquire a read-lock to
// prevent closing concurrently (see SyncClose).
func (w *lockedWriterAt) WriteAt(p []byte, off int64) (n int, err error) {
	w.mut.RLock()
	defer w.mut.RUnlock()
	return w.fd.WriteAt(p, off)
}

// SyncClose ensures that no more writes are happening before going ahead and
// syncing and closing the fd, thus needs to acquire a write-lock.
func (w *lockedWriterAt) SyncClose() error {
	w.mut.Lock()
	defer w.mut.Unlock()
	if err := w.fd.Sync(); err != nil {
		// Sync() is nice if it works but not worth failing the
		// operation over if it fails.
		l.Debugf("fsync failed: %v", err)
	}
	return w.fd.Close()
}

// tempFile returns the fd for the temporary file, reusing an open fd
// or creating the file as necessary.
func (s *sharedPullerState) tempFile() (io.WriterAt, error) {
	s.mut.Lock()
	defer s.mut.Unlock()

	// If we've already hit an error, return early
	if s.err != nil {
		return nil, s.err
	}

	// If the temp file is already open, return the file descriptor
	if s.writer != nil {
		return s.writer, nil
	}
	if err := inWritableDir(s.tempFileInWritableDir, s.fs, s.tempName, s.ignorePerms); err != nil {
		s.failLocked(err)
		return nil, err
	}
	return s.writer, nil
}

// tempFileInWritableDir should only be called from tempFile.
func (s *sharedPullerState) tempFileInWritableDir(_ string) error {
	// The permissions to use for the temporary file should be those of the
	// final file, except we need user read & write at minimum. The
	// permissions will be set to the final value later, but in the meantime
	// we don't want to have a temporary file with looser permissions than
	// the final outcome.
	mode := fs.FileMode(s.file.Permissions) | 0600
	if s.ignorePerms {
		// When ignorePerms is set we use a very permissive mode and let the
		// system umask filter it.
		mode = 0666
	}

	// Attempt to create the temp file
	// RDWR because of issue #2994.
	flags := fs.OptReadWrite
	if s.reused == 0 {
		flags |= fs.OptCreate | fs.OptExclusive
	} else if !s.ignorePerms {
		// With sufficiently bad luck when exiting or crashing, we may have
		// had time to chmod the temp file to read only state but not yet
		// moved it to its final name. This leaves us with a read only temp
		// file that we're going to try to reuse. To handle that, we need to
		// make sure we have write permissions on the file before opening it.
		//
		// When ignorePerms is set we trust that the permissions are fine
		// already and make no modification, as we would otherwise override
		// what the umask dictates.

		if err := s.fs.Chmod(s.tempName, mode); err != nil {
			return errors.Wrap(err, "setting perms on temp file")
		}
		if err := s.fs.Lchown(s.tempName, int(s.file.Uid), int(s.file.Gid)); err != nil {
			return errors.Wrap(err, "setting owner on temp file")
		}
	}
	fd, err := s.fs.OpenFile(s.tempName, flags, mode)
	if err != nil {
		return errors.Wrap(err, "opening temp file")
	}

	// Hide the temporary file
	s.fs.Hide(s.tempName)

	// Don't truncate symlink files, as that will mean that the path will
	// contain a bunch of nulls.
	if s.sparse && !s.file.IsSymlink() {
		// Truncate sets the size of the file. This creates a sparse file or a
		// space reservation, depending on the underlying filesystem.
		if err := fd.Truncate(s.file.Size); err != nil {
			// The truncate call failed. That can happen in some cases when
			// space reservation isn't possible or over some network
			// filesystems... This generally doesn't matter.

			if s.reused > 0 {
				// ... but if we are attempting to reuse a file we have a
				// corner case when the old file is larger than the new one
				// and we can't just overwrite blocks and let the old data
				// linger at the end. In this case we attempt a delete of
				// the file and hope for better luck next time, when we
				// should come around with s.reused == 0.

				fd.Close()

				if remErr := s.fs.Remove(s.tempName); remErr != nil {
					l.Debugln("failed to remove temporary file:", remErr)
				}

				return err
			}
		}
	}

	// Same fd will be used by all writers
	s.writer = &lockedWriterAt{sync.NewRWMutex(), fd}
	return nil
}

// fail sets the error on the puller state compose of error, and marks the
// sharedPullerState as failed. Is a no-op when called on an already failed state.
func (s *sharedPullerState) fail(err error) {
	s.mut.Lock()
	defer s.mut.Unlock()

	s.failLocked(err)
}

func (s *sharedPullerState) failLocked(err error) {
	if s.err != nil || err == nil {
		return
	}

	s.err = err
}

func (s *sharedPullerState) failed() error {
	s.mut.RLock()
	err := s.err
	s.mut.RUnlock()

	return err
}

func (s *sharedPullerState) copyDone(block protocol.BlockInfo) {
	s.mut.Lock()
	s.copyNeeded--
	s.updated = time.Now()
	s.available = append(s.available, int32(block.Offset/int64(s.file.BlockSize())))
	s.availableUpdated = time.Now()
	l.Debugln("sharedPullerState", s.folder, s.file.Name, "copyNeeded ->", s.copyNeeded)
	s.mut.Unlock()
}

func (s *sharedPullerState) copiedFromOrigin() {
	s.mut.Lock()
	s.copyOrigin++
	s.updated = time.Now()
	s.mut.Unlock()
}

func (s *sharedPullerState) copiedFromOriginShifted() {
	s.mut.Lock()
	s.copyOrigin++
	s.copyOriginShifted++
	s.updated = time.Now()
	s.mut.Unlock()
}

func (s *sharedPullerState) pullStarted() {
	s.mut.Lock()
	s.copyTotal--
	s.copyNeeded--
	s.pullTotal++
	s.pullNeeded++
	s.updated = time.Now()
	l.Debugln("sharedPullerState", s.folder, s.file.Name, "pullNeeded start ->", s.pullNeeded)
	s.mut.Unlock()
}

func (s *sharedPullerState) pullDone(block protocol.BlockInfo) {
	s.mut.Lock()
	s.pullNeeded--
	s.updated = time.Now()
	s.available = append(s.available, int32(block.Offset/int64(s.file.BlockSize())))
	s.availableUpdated = time.Now()
	l.Debugln("sharedPullerState", s.folder, s.file.Name, "pullNeeded done ->", s.pullNeeded)
	s.mut.Unlock()
}

// finalClose atomically closes and returns closed status of a file. A true
// first return value means the file was closed and should be finished, with
// the error indicating the success or failure of the close. A false first
// return value indicates the file is not ready to be closed, or is already
// closed and should in either case not be finished off now.
func (s *sharedPullerState) finalClose() (bool, error) {
	s.mut.Lock()
	defer s.mut.Unlock()

	if s.closed {
		// Already closed
		return false, nil
	}

	if s.pullNeeded+s.copyNeeded != 0 && s.err == nil {
		// Not done yet, and not errored
		return false, nil
	}

	if s.writer != nil {
		if err := s.writer.SyncClose(); err != nil && s.err == nil {
			// This is our error as we weren't errored before.
			s.err = err
		}
		s.writer = nil
	}

	s.closed = true

	// Unhide the temporary file when we close it, as it's likely to
	// immediately be renamed to the final name. If this is a failed temp
	// file we will also unhide it, but I'm fine with that as we're now
	// leaving it around for potentially quite a while.
	s.fs.Unhide(s.tempName)

	return true, s.err
}

// Progress returns the momentarily progress for the puller
func (s *sharedPullerState) Progress() *pullerProgress {
	s.mut.RLock()
	defer s.mut.RUnlock()
	total := s.reused + s.copyTotal + s.pullTotal
	done := total - s.copyNeeded - s.pullNeeded
	return &pullerProgress{
		Total:               total,
		Reused:              s.reused,
		CopiedFromOrigin:    s.copyOrigin,
		CopiedFromElsewhere: s.copyTotal - s.copyNeeded - s.copyOrigin,
		Pulled:              s.pullTotal - s.pullNeeded,
		Pulling:             s.pullNeeded,
		BytesTotal:          blocksToSize(s.file.BlockSize(), total),
		BytesDone:           blocksToSize(s.file.BlockSize(), done),
	}
}

// Updated returns the time when any of the progress related counters was last updated.
func (s *sharedPullerState) Updated() time.Time {
	s.mut.RLock()
	t := s.updated
	s.mut.RUnlock()
	return t
}

// AvailableUpdated returns the time last time list of available blocks was updated
func (s *sharedPullerState) AvailableUpdated() time.Time {
	s.mut.RLock()
	t := s.availableUpdated
	s.mut.RUnlock()
	return t
}

// Available returns blocks available in the current temporary file
func (s *sharedPullerState) Available() []int32 {
	s.mut.RLock()
	blocks := s.available
	s.mut.RUnlock()
	return blocks
}

func blocksToSize(size int, num int) int64 {
	if num < 2 {
		return int64(size / 2)
	}
	return int64(num-1)*int64(size) + int64(size/2)
}
