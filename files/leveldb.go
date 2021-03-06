// Copyright (C) 2014 Jakob Borg and Contributors (see the CONTRIBUTORS file).
// All rights reserved. Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package files

import (
	"bytes"
	"runtime"
	"sort"
	"sync"

	"github.com/syncthing/syncthing/lamport"
	"github.com/syncthing/syncthing/protocol"
	"github.com/syndtr/goleveldb/leveldb"
	"github.com/syndtr/goleveldb/leveldb/iterator"
	"github.com/syndtr/goleveldb/leveldb/opt"
	"github.com/syndtr/goleveldb/leveldb/util"
)

var (
	clockTick uint64
	clockMut  sync.Mutex
)

func clock(v uint64) uint64 {
	clockMut.Lock()
	defer clockMut.Unlock()
	if v > clockTick {
		clockTick = v + 1
	} else {
		clockTick++
	}
	return clockTick
}

const (
	keyTypeNode = iota
	keyTypeGlobal
)

type fileVersion struct {
	version uint64
	node    []byte
}

type versionList struct {
	versions []fileVersion
}

type fileList []protocol.FileInfo

func (l fileList) Len() int {
	return len(l)
}

func (l fileList) Swap(a, b int) {
	l[a], l[b] = l[b], l[a]
}

func (l fileList) Less(a, b int) bool {
	return l[a].Name < l[b].Name
}

type dbReader interface {
	Get([]byte, *opt.ReadOptions) ([]byte, error)
}

type dbWriter interface {
	Put([]byte, []byte)
	Delete([]byte)
}

/*

keyTypeNode (1 byte)
    repository (64 bytes)
        node (32 bytes)
            name (variable size)
            	|
            	scanner.File

keyTypeGlobal (1 byte)
	repository (64 bytes)
		name (variable size)
			|
			[]fileVersion (sorted)

*/

func nodeKey(repo, node, file []byte) []byte {
	k := make([]byte, 1+64+32+len(file))
	k[0] = keyTypeNode
	copy(k[1:], []byte(repo))
	copy(k[1+64:], node[:])
	copy(k[1+64+32:], []byte(file))
	return k
}

func globalKey(repo, file []byte) []byte {
	k := make([]byte, 1+64+len(file))
	k[0] = keyTypeGlobal
	copy(k[1:], []byte(repo))
	copy(k[1+64:], []byte(file))
	return k
}

func nodeKeyName(key []byte) []byte {
	return key[1+64+32:]
}
func nodeKeyRepo(key []byte) []byte {
	repo := key[1 : 1+64]
	izero := bytes.IndexByte(repo, 0)
	return repo[:izero]
}
func nodeKeyNode(key []byte) []byte {
	return key[1+64 : 1+64+32]
}

func globalKeyName(key []byte) []byte {
	return key[1+64:]
}

func globalKeyRepo(key []byte) []byte {
	repo := key[1 : 1+64]
	izero := bytes.IndexByte(repo, 0)
	return repo[:izero]
}

type deletionHandler func(db dbReader, batch dbWriter, repo, node, name []byte, dbi iterator.Iterator) uint64

type fileIterator func(f protocol.FileIntf) bool

func ldbGenericReplace(db *leveldb.DB, repo, node []byte, fs []protocol.FileInfo, deleteFn deletionHandler) uint64 {
	defer runtime.GC()

	sort.Sort(fileList(fs)) // sort list on name, same as on disk

	start := nodeKey(repo, node, nil)                            // before all repo/node files
	limit := nodeKey(repo, node, []byte{0xff, 0xff, 0xff, 0xff}) // after all repo/node files

	batch := new(leveldb.Batch)
	snap, err := db.GetSnapshot()
	if err != nil {
		panic(err)
	}
	defer snap.Release()
	dbi := snap.NewIterator(&util.Range{Start: start, Limit: limit}, nil)
	defer dbi.Release()

	moreDb := dbi.Next()
	fsi := 0
	var maxLocalVer uint64

	for {
		var newName, oldName []byte
		moreFs := fsi < len(fs)

		if !moreDb && !moreFs {
			break
		}

		if !moreFs && deleteFn == nil {
			// We don't have any more updated files to process and deletion
			// has not been requested, so we can exit early
			break
		}

		if moreFs {
			newName = []byte(fs[fsi].Name)
		}

		if moreDb {
			oldName = nodeKeyName(dbi.Key())
		}

		cmp := bytes.Compare(newName, oldName)

		if debug {
			l.Debugf("generic replace; repo=%q node=%v moreFs=%v moreDb=%v cmp=%d newName=%q oldName=%q", repo, protocol.NodeIDFromBytes(node), moreFs, moreDb, cmp, newName, oldName)
		}

		switch {
		case moreFs && (!moreDb || cmp == -1):
			// Disk is missing this file. Insert it.
			if lv := ldbInsert(batch, repo, node, newName, fs[fsi]); lv > maxLocalVer {
				maxLocalVer = lv
			}
			if fs[fsi].IsInvalid() {
				ldbRemoveFromGlobal(snap, batch, repo, node, newName)
			} else {
				ldbUpdateGlobal(snap, batch, repo, node, newName, fs[fsi].Version)
			}
			fsi++

		case moreFs && moreDb && cmp == 0:
			// File exists on both sides - compare versions. We might get an
			// update with the same version and different flags if a node has
			// marked a file as invalid, so handle that too.
			var ef protocol.FileInfoTruncated
			ef.UnmarshalXDR(dbi.Value())
			if fs[fsi].Version > ef.Version || fs[fsi].Version != ef.Version {
				if lv := ldbInsert(batch, repo, node, newName, fs[fsi]); lv > maxLocalVer {
					maxLocalVer = lv
				}
				if fs[fsi].IsInvalid() {
					ldbRemoveFromGlobal(snap, batch, repo, node, newName)
				} else {
					ldbUpdateGlobal(snap, batch, repo, node, newName, fs[fsi].Version)
				}
			}
			// Iterate both sides.
			fsi++
			moreDb = dbi.Next()

		case moreDb && (!moreFs || cmp == 1):
			if deleteFn != nil {
				if lv := deleteFn(snap, batch, repo, node, oldName, dbi); lv > maxLocalVer {
					maxLocalVer = lv
				}
			}
			moreDb = dbi.Next()
		}
	}

	err = db.Write(batch, nil)
	if err != nil {
		panic(err)
	}

	return maxLocalVer
}

func ldbReplace(db *leveldb.DB, repo, node []byte, fs []protocol.FileInfo) uint64 {
	// TODO: Return the remaining maxLocalVer?
	return ldbGenericReplace(db, repo, node, fs, func(db dbReader, batch dbWriter, repo, node, name []byte, dbi iterator.Iterator) uint64 {
		// Disk has files that we are missing. Remove it.
		if debug {
			l.Debugf("delete; repo=%q node=%v name=%q", repo, protocol.NodeIDFromBytes(node), name)
		}
		ldbRemoveFromGlobal(db, batch, repo, node, name)
		batch.Delete(dbi.Key())
		return 0
	})
}

func ldbReplaceWithDelete(db *leveldb.DB, repo, node []byte, fs []protocol.FileInfo) uint64 {
	return ldbGenericReplace(db, repo, node, fs, func(db dbReader, batch dbWriter, repo, node, name []byte, dbi iterator.Iterator) uint64 {
		var tf protocol.FileInfoTruncated
		err := tf.UnmarshalXDR(dbi.Value())
		if err != nil {
			panic(err)
		}
		if !tf.IsDeleted() {
			if debug {
				l.Debugf("mark deleted; repo=%q node=%v name=%q", repo, protocol.NodeIDFromBytes(node), name)
			}
			ts := clock(tf.LocalVersion)
			f := protocol.FileInfo{
				Name:         tf.Name,
				Version:      lamport.Default.Tick(tf.Version),
				LocalVersion: ts,
				Flags:        tf.Flags | protocol.FlagDeleted,
				Modified:     tf.Modified,
			}
			batch.Put(dbi.Key(), f.MarshalXDR())
			ldbUpdateGlobal(db, batch, repo, node, nodeKeyName(dbi.Key()), f.Version)
			return ts
		}
		return 0
	})
}

func ldbUpdate(db *leveldb.DB, repo, node []byte, fs []protocol.FileInfo) uint64 {
	defer runtime.GC()

	batch := new(leveldb.Batch)
	snap, err := db.GetSnapshot()
	if err != nil {
		panic(err)
	}
	defer snap.Release()

	var maxLocalVer uint64
	for _, f := range fs {
		name := []byte(f.Name)
		fk := nodeKey(repo, node, name)
		bs, err := snap.Get(fk, nil)
		if err == leveldb.ErrNotFound {
			if lv := ldbInsert(batch, repo, node, name, f); lv > maxLocalVer {
				maxLocalVer = lv
			}
			if f.IsInvalid() {
				ldbRemoveFromGlobal(snap, batch, repo, node, name)
			} else {
				ldbUpdateGlobal(snap, batch, repo, node, name, f.Version)
			}
			continue
		}

		var ef protocol.FileInfoTruncated
		err = ef.UnmarshalXDR(bs)
		if err != nil {
			panic(err)
		}
		// Flags might change without the version being bumped when we set the
		// invalid flag on an existing file.
		if ef.Version != f.Version || ef.Flags != f.Flags {
			if lv := ldbInsert(batch, repo, node, name, f); lv > maxLocalVer {
				maxLocalVer = lv
			}
			if f.IsInvalid() {
				ldbRemoveFromGlobal(snap, batch, repo, node, name)
			} else {
				ldbUpdateGlobal(snap, batch, repo, node, name, f.Version)
			}
		}
	}

	err = db.Write(batch, nil)
	if err != nil {
		panic(err)
	}

	return maxLocalVer
}

func ldbInsert(batch dbWriter, repo, node, name []byte, file protocol.FileInfo) uint64 {
	if debug {
		l.Debugf("insert; repo=%q node=%v %v", repo, protocol.NodeIDFromBytes(node), file)
	}

	if file.LocalVersion == 0 {
		file.LocalVersion = clock(0)
	}

	nk := nodeKey(repo, node, name)
	batch.Put(nk, file.MarshalXDR())

	return file.LocalVersion
}

// ldbUpdateGlobal adds this node+version to the version list for the given
// file. If the node is already present in the list, the version is updated.
// If the file does not have an entry in the global list, it is created.
func ldbUpdateGlobal(db dbReader, batch dbWriter, repo, node, file []byte, version uint64) bool {
	if debug {
		l.Debugf("update global; repo=%q node=%v file=%q version=%d", repo, protocol.NodeIDFromBytes(node), file, version)
	}
	gk := globalKey(repo, file)
	svl, err := db.Get(gk, nil)
	if err != nil && err != leveldb.ErrNotFound {
		panic(err)
	}

	var fl versionList
	nv := fileVersion{
		node:    node,
		version: version,
	}
	if svl != nil {
		err = fl.UnmarshalXDR(svl)
		if err != nil {
			panic(err)
		}

		for i := range fl.versions {
			if bytes.Compare(fl.versions[i].node, node) == 0 {
				if fl.versions[i].version == version {
					// No need to do anything
					return false
				}
				fl.versions = append(fl.versions[:i], fl.versions[i+1:]...)
				break
			}
		}
	}

	for i := range fl.versions {
		if fl.versions[i].version <= version {
			t := append(fl.versions, fileVersion{})
			copy(t[i+1:], t[i:])
			t[i] = nv
			fl.versions = t
			goto done
		}
	}

	fl.versions = append(fl.versions, nv)

done:
	batch.Put(gk, fl.MarshalXDR())

	return true
}

// ldbRemoveFromGlobal removes the node from the global version list for the
// given file. If the version list is empty after this, the file entry is
// removed entirely.
func ldbRemoveFromGlobal(db dbReader, batch dbWriter, repo, node, file []byte) {
	if debug {
		l.Debugf("remove from global; repo=%q node=%v file=%q", repo, protocol.NodeIDFromBytes(node), file)
	}

	gk := globalKey(repo, file)
	svl, err := db.Get(gk, nil)
	if err != nil {
		// We might be called to "remove" a global version that doesn't exist
		// if the first update for the file is already marked invalid.
		return
	}

	var fl versionList
	err = fl.UnmarshalXDR(svl)
	if err != nil {
		panic(err)
	}

	for i := range fl.versions {
		if bytes.Compare(fl.versions[i].node, node) == 0 {
			fl.versions = append(fl.versions[:i], fl.versions[i+1:]...)
			break
		}
	}

	if len(fl.versions) == 0 {
		batch.Delete(gk)
	} else {
		batch.Put(gk, fl.MarshalXDR())
	}
}

func ldbWithHave(db *leveldb.DB, repo, node []byte, truncate bool, fn fileIterator) {
	start := nodeKey(repo, node, nil)                            // before all repo/node files
	limit := nodeKey(repo, node, []byte{0xff, 0xff, 0xff, 0xff}) // after all repo/node files
	snap, err := db.GetSnapshot()
	if err != nil {
		panic(err)
	}
	defer snap.Release()
	dbi := snap.NewIterator(&util.Range{Start: start, Limit: limit}, nil)
	defer dbi.Release()

	for dbi.Next() {
		f, err := unmarshalTrunc(dbi.Value(), truncate)
		if err != nil {
			panic(err)
		}
		if cont := fn(f); !cont {
			return
		}
	}
}

func ldbWithAllRepoTruncated(db *leveldb.DB, repo []byte, fn func(node []byte, f protocol.FileInfoTruncated) bool) {
	defer runtime.GC()

	start := nodeKey(repo, nil, nil)                                                // before all repo/node files
	limit := nodeKey(repo, protocol.LocalNodeID[:], []byte{0xff, 0xff, 0xff, 0xff}) // after all repo/node files
	snap, err := db.GetSnapshot()
	if err != nil {
		panic(err)
	}
	defer snap.Release()
	dbi := snap.NewIterator(&util.Range{Start: start, Limit: limit}, nil)
	defer dbi.Release()

	for dbi.Next() {
		node := nodeKeyNode(dbi.Key())
		var f protocol.FileInfoTruncated
		err := f.UnmarshalXDR(dbi.Value())
		if err != nil {
			panic(err)
		}
		if cont := fn(node, f); !cont {
			return
		}
	}
}

func ldbGet(db *leveldb.DB, repo, node, file []byte) protocol.FileInfo {
	nk := nodeKey(repo, node, file)
	bs, err := db.Get(nk, nil)
	if err == leveldb.ErrNotFound {
		return protocol.FileInfo{}
	}
	if err != nil {
		panic(err)
	}

	var f protocol.FileInfo
	err = f.UnmarshalXDR(bs)
	if err != nil {
		panic(err)
	}
	return f
}

func ldbGetGlobal(db *leveldb.DB, repo, file []byte) protocol.FileInfo {
	k := globalKey(repo, file)
	snap, err := db.GetSnapshot()
	if err != nil {
		panic(err)
	}
	defer snap.Release()

	bs, err := snap.Get(k, nil)
	if err == leveldb.ErrNotFound {
		return protocol.FileInfo{}
	}
	if err != nil {
		panic(err)
	}

	var vl versionList
	err = vl.UnmarshalXDR(bs)
	if err != nil {
		panic(err)
	}
	if len(vl.versions) == 0 {
		l.Debugln(k)
		panic("no versions?")
	}

	k = nodeKey(repo, vl.versions[0].node, file)
	bs, err = snap.Get(k, nil)
	if err != nil {
		panic(err)
	}

	var f protocol.FileInfo
	err = f.UnmarshalXDR(bs)
	if err != nil {
		panic(err)
	}
	return f
}

func ldbWithGlobal(db *leveldb.DB, repo []byte, truncate bool, fn fileIterator) {
	defer runtime.GC()

	start := globalKey(repo, nil)
	limit := globalKey(repo, []byte{0xff, 0xff, 0xff, 0xff})
	snap, err := db.GetSnapshot()
	if err != nil {
		panic(err)
	}
	defer snap.Release()
	dbi := snap.NewIterator(&util.Range{Start: start, Limit: limit}, nil)
	defer dbi.Release()

	for dbi.Next() {
		var vl versionList
		err := vl.UnmarshalXDR(dbi.Value())
		if err != nil {
			panic(err)
		}
		if len(vl.versions) == 0 {
			l.Debugln(dbi.Key())
			panic("no versions?")
		}
		fk := nodeKey(repo, vl.versions[0].node, globalKeyName(dbi.Key()))
		bs, err := snap.Get(fk, nil)
		if err != nil {
			panic(err)
		}

		f, err := unmarshalTrunc(bs, truncate)
		if err != nil {
			panic(err)
		}

		if cont := fn(f); !cont {
			return
		}
	}
}

func ldbAvailability(db *leveldb.DB, repo, file []byte) []protocol.NodeID {
	k := globalKey(repo, file)
	bs, err := db.Get(k, nil)
	if err == leveldb.ErrNotFound {
		return nil
	}
	if err != nil {
		panic(err)
	}

	var vl versionList
	err = vl.UnmarshalXDR(bs)
	if err != nil {
		panic(err)
	}

	var nodes []protocol.NodeID
	for _, v := range vl.versions {
		if v.version != vl.versions[0].version {
			break
		}
		n := protocol.NodeIDFromBytes(v.node)
		nodes = append(nodes, n)
	}

	return nodes
}

func ldbWithNeed(db *leveldb.DB, repo, node []byte, truncate bool, fn fileIterator) {
	defer runtime.GC()

	start := globalKey(repo, nil)
	limit := globalKey(repo, []byte{0xff, 0xff, 0xff, 0xff})
	snap, err := db.GetSnapshot()
	if err != nil {
		panic(err)
	}
	defer snap.Release()
	dbi := snap.NewIterator(&util.Range{Start: start, Limit: limit}, nil)
	defer dbi.Release()

outer:
	for dbi.Next() {
		var vl versionList
		err := vl.UnmarshalXDR(dbi.Value())
		if err != nil {
			panic(err)
		}
		if len(vl.versions) == 0 {
			l.Debugln(dbi.Key())
			panic("no versions?")
		}

		have := false // If we have the file, any version
		need := false // If we have a lower version of the file
		var haveVersion uint64
		for _, v := range vl.versions {
			if bytes.Compare(v.node, node) == 0 {
				have = true
				haveVersion = v.version
				need = v.version < vl.versions[0].version
				break
			}
		}

		if need || !have {
			name := globalKeyName(dbi.Key())
			needVersion := vl.versions[0].version
		inner:
			for i := range vl.versions {
				if vl.versions[i].version != needVersion {
					// We haven't found a valid copy of the file with the needed version.
					continue outer
				}
				fk := nodeKey(repo, vl.versions[i].node, name)
				bs, err := snap.Get(fk, nil)
				if err != nil {
					panic(err)
				}

				gf, err := unmarshalTrunc(bs, truncate)
				if err != nil {
					panic(err)
				}

				if gf.IsInvalid() {
					// The file is marked invalid for whatever reason, don't use it.
					continue inner
				}

				if gf.IsDeleted() && !have {
					// We don't need deleted files that we don't have
					continue outer
				}

				if debug {
					l.Debugf("need repo=%q node=%v name=%q need=%v have=%v haveV=%d globalV=%d", repo, protocol.NodeIDFromBytes(node), name, need, have, haveVersion, vl.versions[0].version)
				}

				if cont := fn(gf); !cont {
					return
				}

				// This file is handled, no need to look further in the version list
				continue outer
			}
		}
	}
}

func ldbListRepos(db *leveldb.DB) []string {
	defer runtime.GC()

	start := []byte{keyTypeGlobal}
	limit := []byte{keyTypeGlobal + 1}
	snap, err := db.GetSnapshot()
	if err != nil {
		panic(err)
	}
	defer snap.Release()
	dbi := snap.NewIterator(&util.Range{Start: start, Limit: limit}, nil)
	defer dbi.Release()

	repoExists := make(map[string]bool)
	for dbi.Next() {
		repo := string(globalKeyRepo(dbi.Key()))
		if !repoExists[repo] {
			repoExists[repo] = true
		}
	}

	repos := make([]string, 0, len(repoExists))
	for k := range repoExists {
		repos = append(repos, k)
	}

	sort.Strings(repos)
	return repos
}

func ldbDropRepo(db *leveldb.DB, repo []byte) {
	defer runtime.GC()

	snap, err := db.GetSnapshot()
	if err != nil {
		panic(err)
	}
	defer snap.Release()

	// Remove all items related to the given repo from the node->file bucket
	start := []byte{keyTypeNode}
	limit := []byte{keyTypeNode + 1}
	dbi := snap.NewIterator(&util.Range{Start: start, Limit: limit}, nil)
	for dbi.Next() {
		itemRepo := nodeKeyRepo(dbi.Key())
		if bytes.Compare(repo, itemRepo) == 0 {
			db.Delete(dbi.Key(), nil)
		}
	}
	dbi.Release()

	// Remove all items related to the given repo from the global bucket
	start = []byte{keyTypeGlobal}
	limit = []byte{keyTypeGlobal + 1}
	dbi = snap.NewIterator(&util.Range{Start: start, Limit: limit}, nil)
	for dbi.Next() {
		itemRepo := globalKeyRepo(dbi.Key())
		if bytes.Compare(repo, itemRepo) == 0 {
			db.Delete(dbi.Key(), nil)
		}
	}
	dbi.Release()
}

func unmarshalTrunc(bs []byte, truncate bool) (protocol.FileIntf, error) {
	if truncate {
		var tf protocol.FileInfoTruncated
		err := tf.UnmarshalXDR(bs)
		return tf, err
	} else {
		var tf protocol.FileInfo
		err := tf.UnmarshalXDR(bs)
		return tf, err
	}
}
