// Copyright 2015 CoreOS, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package wal

import (
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"log"
	"os"
	"path"
	"reflect"

	"github.com/coreos/etcd/pkg/fileutil"
	"github.com/coreos/etcd/pkg/pbutil"
	"github.com/coreos/etcd/raft"
	"github.com/coreos/etcd/raft/raftpb"
	"github.com/coreos/etcd/wal/walpb"
)

const (
	metadataType int64 = iota + 1
	entryType
	stateType
	crcType
	snapshotType

	// the owner can make/remove files inside the directory
	privateDirMode = 0700
)

var (
	ErrMetadataConflict = errors.New("wal: conflicting metadata found")
	ErrFileNotFound     = errors.New("wal: file not found")
	ErrCRCMismatch      = errors.New("wal: crc mismatch")
	ErrSnapshotMismatch = errors.New("wal: snapshot mismatch")
	ErrSnapshotNotFound = errors.New("wal: snapshot not found")
	crcTable            = crc32.MakeTable(crc32.Castagnoli)
)

// WAL is a logical repersentation of the stable storage.
// WAL is either in read mode or append mode but not both.
// A newly created WAL is in append mode, and ready for appending records.
// A just opened WAL is in read mode, and ready for reading records.
// The WAL will be ready for appending after reading out all the previous records.
type WAL struct {
	dir      string           // the living directory of the underlay files
	metadata []byte           // metadata recorded at the head of each WAL
	state    raftpb.HardState // hardstate recorded at the head of WAL

	start   walpb.Snapshot // snapshot to start reading
	decoder *decoder       // decoder to decode records

	f       *os.File // underlay file opened for appending, sync
	seq     uint64   // sequence of the wal file currently used for writes
	enti    uint64   // index of the last entry saved to the wal
	encoder *encoder // encoder to encode records

	locks []fileutil.Lock // the file locks the WAL is holding (the name is increasing)
}

// Create creates a WAL ready for appending records. The given metadata is
// recorded at the head of each WAL file, and can be retrieved with ReadAll.
func Create(dirpath string, metadata []byte) (*WAL, error) {
	if Exist(dirpath) {
		return nil, os.ErrExist
	}

	if err := os.MkdirAll(dirpath, privateDirMode); err != nil {
		return nil, err
	}

	p := path.Join(dirpath, walName(0, 0))
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0600)
	if err != nil {
		return nil, err
	}
	l, err := fileutil.NewLock(f.Name())
	if err != nil {
		return nil, err
	}
	err = l.Lock()
	if err != nil {
		return nil, err
	}

	w := &WAL{
		dir:      dirpath,
		metadata: metadata,
		seq:      0,
		f:        f,
		encoder:  newEncoder(f, 0),
	}
	w.locks = append(w.locks, l)
	if err := w.saveCrc(0); err != nil {
		return nil, err
	}
	if err := w.encoder.encode(&walpb.Record{Type: metadataType, Data: metadata}); err != nil {
		return nil, err
	}
	if err = w.SaveSnapshot(walpb.Snapshot{}); err != nil {
		return nil, err
	}
	return w, nil
}

// Open opens the WAL at the given snap.
// The snap SHOULD have been previously saved to the WAL, or the following
// ReadAll will fail.
// The returned WAL is ready to read and the first record will be the one after
// the given snap. The WAL cannot be appended to before reading out all of its
// previous records.
func Open(dirpath string, snap walpb.Snapshot) (*WAL, error) {
	return openAtIndex(dirpath, snap, true)
}

// OpenNotInUse only opens the wal files that are not in use.
// Other than that, it is similar to Open.
func OpenNotInUse(dirpath string, snap walpb.Snapshot) (*WAL, error) {
	return openAtIndex(dirpath, snap, false)
}

func openAtIndex(dirpath string, snap walpb.Snapshot, all bool) (*WAL, error) {
	names, err := fileutil.ReadDir(dirpath)
	if err != nil {
		return nil, err
	}
	names = checkWalNames(names)
	if len(names) == 0 {
		return nil, ErrFileNotFound
	}

	nameIndex, ok := searchIndex(names, snap.Index)
	if !ok || !isValidSeq(names[nameIndex:]) {
		return nil, ErrFileNotFound
	}

	// open the wal files for reading
	rcs := make([]io.ReadCloser, 0)
	ls := make([]fileutil.Lock, 0)
	for _, name := range names[nameIndex:] {
		f, err := os.Open(path.Join(dirpath, name))
		if err != nil {
			return nil, err
		}
		l, err := fileutil.NewLock(f.Name())
		if err != nil {
			return nil, err
		}
		err = l.TryLock()
		if err != nil {
			if all {
				return nil, err
			} else {
				log.Printf("wal: opened all the files until %s, since it is still in use by an etcd server", name)
				break
			}
		}
		rcs = append(rcs, f)
		ls = append(ls, l)
	}
	rc := MultiReadCloser(rcs...)

	// open the lastest wal file for appending
	seq, _, err := parseWalName(names[len(names)-1])
	if err != nil {
		rc.Close()
		return nil, err
	}
	last := path.Join(dirpath, names[len(names)-1])
	f, err := os.OpenFile(last, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		rc.Close()
		return nil, err
	}

	// create a WAL ready for reading
	w := &WAL{
		dir:     dirpath,
		start:   snap,
		decoder: newDecoder(rc),

		f:     f,
		seq:   seq,
		locks: ls,
	}
	return w, nil
}

// ReadAll reads out all records of the current WAL.
// If it cannot read out the expected snap, it will return ErrSnapshotNotFound.
// If loaded snap doesn't match with the expected one, it will return
// all the records and error ErrSnapshotMismatch.
// TODO: detect not-last-snap error.
// TODO: maybe loose the checking of match.
// After ReadAll, the WAL will be ready for appending new records.
func (w *WAL) ReadAll() (metadata []byte, state raftpb.HardState, ents []raftpb.Entry, err error) {
	rec := &walpb.Record{}
	decoder := w.decoder

	var match bool
	for err = decoder.decode(rec); err == nil; err = decoder.decode(rec) {
		switch rec.Type {
		case entryType:
			e := mustUnmarshalEntry(rec.Data)
			if e.Index > w.start.Index {
				ents = append(ents[:e.Index-w.start.Index-1], e)
			}
			w.enti = e.Index
		case stateType:
			state = mustUnmarshalState(rec.Data)
		case metadataType:
			if metadata != nil && !reflect.DeepEqual(metadata, rec.Data) {
				state.Reset()
				return nil, state, nil, ErrMetadataConflict
			}
			metadata = rec.Data
		case crcType:
			crc := decoder.crc.Sum32()
			// current crc of decoder must match the crc of the record.
			// do no need to match 0 crc, since the decoder is a new one at this case.
			if crc != 0 && rec.Validate(crc) != nil {
				state.Reset()
				return nil, state, nil, ErrCRCMismatch
			}
			decoder.updateCRC(rec.Crc)
		case snapshotType:
			var snap walpb.Snapshot
			pbutil.MustUnmarshal(&snap, rec.Data)
			if snap.Index == w.start.Index {
				if snap.Term != w.start.Term {
					state.Reset()
					return nil, state, nil, ErrSnapshotMismatch
				}
				match = true
			}
		default:
			state.Reset()
			return nil, state, nil, fmt.Errorf("unexpected block type %d", rec.Type)
		}
	}
	if err != io.EOF {
		state.Reset()
		return nil, state, nil, err
	}
	err = nil
	if !match {
		err = ErrSnapshotNotFound
	}

	// close decoder, disable reading
	w.decoder.close()
	w.start = walpb.Snapshot{}

	w.metadata = metadata
	// create encoder (chain crc with the decoder), enable appending
	w.encoder = newEncoder(w.f, w.decoder.lastCRC())
	w.decoder = nil
	return metadata, state, ents, err
}

// Cut closes current file written and creates a new one ready to append.
func (w *WAL) Cut() error {
	// create a new wal file with name sequence + 1
	fpath := path.Join(w.dir, walName(w.seq+1, w.enti+1))
	f, err := os.OpenFile(fpath, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0600)
	if err != nil {
		return err
	}
	l, err := fileutil.NewLock(f.Name())
	if err != nil {
		return err
	}
	err = l.Lock()
	if err != nil {
		return err
	}
	w.locks = append(w.locks, l)
	if err = w.sync(); err != nil {
		return err
	}
	w.f.Close()

	// update writer and save the previous crc
	w.f = f
	w.seq++
	prevCrc := w.encoder.crc.Sum32()
	w.encoder = newEncoder(w.f, prevCrc)
	if err := w.saveCrc(prevCrc); err != nil {
		return err
	}
	if err := w.encoder.encode(&walpb.Record{Type: metadataType, Data: w.metadata}); err != nil {
		return err
	}
	if err := w.saveState(&w.state); err != nil {
		return err
	}
	return w.sync()
}

func (w *WAL) sync() error {
	if w.encoder != nil {
		if err := w.encoder.flush(); err != nil {
			return err
		}
	}
	return w.f.Sync()
}

// ReleaseLockTo releases the locks w is holding, which
// have index smaller or equal to the given index.
func (w *WAL) ReleaseLockTo(index uint64) error {
	for _, l := range w.locks {
		_, i, err := parseWalName(path.Base(l.Name()))
		if err != nil {
			return err
		}
		if i > index {
			return nil
		}
		err = l.Unlock()
		if err != nil {
			return err
		}
		err = l.Destroy()
		if err != nil {
			return err
		}
		w.locks = w.locks[1:]
	}
	return nil
}

func (w *WAL) Close() error {
	if w.f != nil {
		if err := w.sync(); err != nil {
			return err
		}
		if err := w.f.Close(); err != nil {
			return err
		}
	}
	for _, l := range w.locks {
		// TODO: log the error
		l.Unlock()
		l.Destroy()
	}
	return nil
}

func (w *WAL) saveEntry(e *raftpb.Entry) error {
	b := pbutil.MustMarshal(e)
	rec := &walpb.Record{Type: entryType, Data: b}
	if err := w.encoder.encode(rec); err != nil {
		return err
	}
	w.enti = e.Index
	return nil
}

func (w *WAL) saveState(s *raftpb.HardState) error {
	if raft.IsEmptyHardState(*s) {
		return nil
	}
	w.state = *s
	b := pbutil.MustMarshal(s)
	rec := &walpb.Record{Type: stateType, Data: b}
	return w.encoder.encode(rec)
}

func (w *WAL) Save(st raftpb.HardState, ents []raftpb.Entry) error {
	// TODO(xiangli): no more reference operator
	if err := w.saveState(&st); err != nil {
		return err
	}
	for i := range ents {
		if err := w.saveEntry(&ents[i]); err != nil {
			return err
		}
	}
	return w.sync()
}

func (w *WAL) SaveSnapshot(e walpb.Snapshot) error {
	b := pbutil.MustMarshal(&e)
	rec := &walpb.Record{Type: snapshotType, Data: b}
	if err := w.encoder.encode(rec); err != nil {
		return err
	}
	// update enti only when snapshot is ahead of last index
	if w.enti < e.Index {
		w.enti = e.Index
	}
	return w.sync()
}

func (w *WAL) saveCrc(prevCrc uint32) error {
	return w.encoder.encode(&walpb.Record{Type: crcType, Crc: prevCrc})
}
