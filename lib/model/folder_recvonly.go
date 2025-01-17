// Copyright (C) 2018 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package model

import (
	"sort"
	"time"

	"github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/db"
	"github.com/syncthing/syncthing/lib/events"
	"github.com/syncthing/syncthing/lib/fs"
	"github.com/syncthing/syncthing/lib/ignore"
	"github.com/syncthing/syncthing/lib/protocol"
	"github.com/syncthing/syncthing/lib/versioner"
)

func init() {
	folderFactories[config.FolderTypeReceiveOnly] = newReceiveOnlyFolder
}

/*
receiveOnlyFolder is a folder that does not propagate local changes outward.
It does this by the following general mechanism (not all of which is
implemted in this file):

- Local changes are scanned and versioned as usual, but get the
  FlagLocalReceiveOnly bit set.

- When changes are sent to the cluster this bit gets converted to the
  Invalid bit (like all other local flags, currently) and also the Version
  gets set to the empty version. The reason for clearing the Version is to
  ensure that other devices will not consider themselves out of date due to
  our change.

- The database layer accounts sizes per flag bit, so we can know how many
  files have been changed locally. We use this to trigger a "Revert" option
  on the folder when the amount of locally changed data is nonzero.

- To revert we take the files which have changed and reset their version
  counter down to zero. The next pull will replace our changed version with
  the globally latest. As this is a user-initiated operation we do not cause
  conflict copies when reverting.

- When pulling normally (i.e., not in the revert case) with local changes,
  normal conflict resolution will apply. Conflict copies will be created,
  but not propagated outwards (because receive only, right).

Implementation wise a receiveOnlyFolder is just a sendReceiveFolder that
sets an extra bit on local changes and has a Revert method.
*/
type receiveOnlyFolder struct {
	*sendReceiveFolder
}

func newReceiveOnlyFolder(model *model, fset *db.FileSet, ignores *ignore.Matcher, cfg config.FolderConfiguration, ver versioner.Versioner, fs fs.Filesystem, evLogger events.Logger) service {
	sr := newSendReceiveFolder(model, fset, ignores, cfg, ver, fs, evLogger).(*sendReceiveFolder)
	sr.localFlags = protocol.FlagLocalReceiveOnly // gets propagated to the scanner, and set on locally changed files
	return &receiveOnlyFolder{sr}
}

func (f *receiveOnlyFolder) Revert() {
	f.setState(FolderScanning)
	defer f.setState(FolderIdle)

	scanChan := make(chan string)
	go f.pullScannerRoutine(scanChan)
	defer close(scanChan)

	delQueue := &deleteQueue{
		handler:  f, // for the deleteItemOnDisk and deleteDirOnDisk methods
		ignores:  f.ignores,
		scanChan: scanChan,
	}

	batch := make([]protocol.FileInfo, 0, maxBatchSizeFiles)
	batchSizeBytes := 0
	f.fset.WithHave(protocol.LocalDeviceID, func(intf db.FileIntf) bool {
		fi := intf.(protocol.FileInfo)
		if !fi.IsReceiveOnlyChanged() {
			// We're only interested in files that have changed locally in
			// receive only mode.
			return true
		}

		if len(fi.Version.Counters) == 1 && fi.Version.Counters[0].ID == f.shortID {
			// We are the only device mentioned in the version vector so the
			// file must originate here. A revert then means to delete it.
			// We'll delete files directly, directories get queued and
			// handled below.

			handled, err := delQueue.handle(fi)
			if err != nil {
				l.Infof("Revert: deleting %s: %v\n", fi.Name, err)
				return true // continue
			}
			if !handled {
				return true // continue
			}

			fi = protocol.FileInfo{
				Name:        fi.Name,
				Type:        fi.Type,
				ModifiedS:   fi.ModifiedS,
				ModifiedNs:  fi.ModifiedNs,
				ModifiedBy:  f.shortID,
				Deleted:     true,
				Version:     protocol.Vector{}, // if this file ever resurfaces anywhere we want our delete to be strictly older
				Gid:         fi.Gid,
				Uid:         fi.Uid,
				Permissions: fi.Permissions,
			}
		} else {
			// Revert means to throw away our local changes. We reset the
			// version to the empty vector, which is strictly older than any
			// other existing version. It is not in conflict with anything,
			// either, so we will not create a conflict copy of our local
			// changes.
			fi.Version = protocol.Vector{}
			fi.LocalFlags &^= protocol.FlagLocalReceiveOnly
		}

		batch = append(batch, fi)
		batchSizeBytes += fi.ProtoSize()

		if len(batch) >= maxBatchSizeFiles || batchSizeBytes >= maxBatchSizeBytes {
			f.updateLocalsFromScanning(batch)
			batch = batch[:0]
			batchSizeBytes = 0
		}
		return true
	})
	if len(batch) > 0 {
		f.updateLocalsFromScanning(batch)
	}
	batch = batch[:0]
	batchSizeBytes = 0

	// Handle any queued directories
	deleted, err := delQueue.flush()
	if err != nil {
		l.Infoln("Revert:", err)
	}
	now := time.Now()
	for _, dir := range deleted {
		batch = append(batch, protocol.FileInfo{
			Name:       dir,
			Type:       protocol.FileInfoTypeDirectory,
			ModifiedS:  now.Unix(),
			ModifiedBy: f.shortID,
			Deleted:    true,
			Version:    protocol.Vector{},
		})
	}
	if len(batch) > 0 {
		f.updateLocalsFromScanning(batch)
	}

	// We will likely have changed our local index, but that won't trigger a
	// pull by itself. Make sure we schedule one so that we start
	// downloading files.
	f.SchedulePull()
}

// deleteQueue handles deletes by delegating to a handler and queuing
// directories for last.
type deleteQueue struct {
	handler interface {
		deleteItemOnDisk(item protocol.FileInfo, scanChan chan<- string) error
		deleteDirOnDisk(dir string, scanChan chan<- string) error
	}
	ignores  *ignore.Matcher
	dirs     []string
	scanChan chan<- string
}

func (q *deleteQueue) handle(fi protocol.FileInfo) (bool, error) {
	// Things that are ignored but not marked deletable are not processed.
	ign := q.ignores.Match(fi.Name)
	if ign.IsIgnored() && !ign.IsDeletable() {
		return false, nil
	}

	// Directories are queued for later processing.
	if fi.IsDirectory() {
		q.dirs = append(q.dirs, fi.Name)
		return false, nil
	}

	// Kill it.
	err := q.handler.deleteItemOnDisk(fi, q.scanChan)
	return true, err
}

func (q *deleteQueue) flush() ([]string, error) {
	// Process directories from the leaves inward.
	sort.Sort(sort.Reverse(sort.StringSlice(q.dirs)))

	var firstError error
	var deleted []string

	for _, dir := range q.dirs {
		if err := q.handler.deleteDirOnDisk(dir, q.scanChan); err == nil {
			deleted = append(deleted, dir)
		} else if err != nil && firstError == nil {
			firstError = err
		}
	}

	return deleted, firstError
}
