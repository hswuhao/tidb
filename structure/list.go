// Copyright 2015 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package structure

import (
	"encoding/binary"

	"github.com/juju/errors"
	"github.com/ngaut/log"
	"github.com/pingcap/tidb/kv"
	"github.com/pingcap/tidb/terror"
)

type listMeta struct {
	LIndex int64
	RIndex int64
}

func (meta listMeta) Value() []byte {
	buf := make([]byte, 16)
	binary.BigEndian.PutUint64(buf[0:8], uint64(meta.LIndex))
	binary.BigEndian.PutUint64(buf[8:16], uint64(meta.RIndex))
	return buf
}

func (meta listMeta) IsEmpty() bool {
	return meta.LIndex >= meta.RIndex
}

// LPush prepends one or multiple values to a list.
func (t *TxStructure) LPush(key []byte, values ...[]byte) error {
	return t.listPush(key, true, values...)
}

// RPush appends one or multiple values to a list.
func (t *TxStructure) RPush(key []byte, values ...[]byte) error {
	return t.listPush(key, false, values...)
}

func (t *TxStructure) listPush(key []byte, left bool, values ...[]byte) error {
	if t.readWriter == nil {
		return errWriteOnSnapshot
	}
	if len(values) == 0 {
		return nil
	}
	log.Infof("list push key %v, val %v", key, values)

	metaKey := t.encodeListMetaKey(key)
	meta, err := t.loadListMeta(metaKey)
	if err != nil {
		return errors.Trace(err)
	}

	index := int64(0)
	for _, v := range values {
		if left {
			meta.LIndex--
			index = meta.LIndex
		} else {
			index = meta.RIndex
			meta.RIndex++
		}

		dataKey := t.encodeListDataKey(key, index)
		log.Infof("mid...list push key %v, val %v, meta key %v, data key %v, index %v",
			key, values, metaKey, dataKey, index)
		if err = t.readWriter.Set(dataKey, v); err != nil {
			return errors.Trace(err)
		}
	}

	log.Infof("end...list push key %v, val %v, meta key %v, meta %v",
		key, values, metaKey, meta)
	return t.readWriter.Set(metaKey, meta.Value())
}

// LPop removes and gets the first element in a list.
func (t *TxStructure) LPop(key []byte) ([]byte, error) {
	return t.listPop(key, true)
}

// RPop removes and gets the last element in a list.
func (t *TxStructure) RPop(key []byte) ([]byte, error) {
	return t.listPop(key, false)
}

func (t *TxStructure) listPop(key []byte, left bool) ([]byte, error) {
	if t.readWriter == nil {
		return nil, errWriteOnSnapshot
	}
	metaKey := t.encodeListMetaKey(key)
	meta, err := t.loadListMeta(metaKey)
	log.Infof("list pop meta key %v, meta %v, err %v", metaKey, meta, err)
	if err != nil || meta.IsEmpty() {
		return nil, errors.Trace(err)
	}

	index := int64(0)
	if left {
		index = meta.LIndex
		meta.LIndex++
	} else {
		meta.RIndex--
		index = meta.RIndex
	}

	dataKey := t.encodeListDataKey(key, index)

	log.Infof("mid...list pop meta key %v, meta %v, data key %v, index %v",
		metaKey, meta, dataKey, index)
	var data []byte
	data, err = t.reader.Get(dataKey)
	if err != nil {
		return nil, errors.Trace(err)
	}

	if err = t.readWriter.Delete(dataKey); err != nil {
		return nil, errors.Trace(err)
	}

	if !meta.IsEmpty() {
		err = t.readWriter.Set(metaKey, meta.Value())
	} else {
		err = t.readWriter.Delete(metaKey)
	}

	log.Infof("end...list pop meta key %v, meta %v, data key %v", metaKey, meta, dataKey)
	return data, errors.Trace(err)
}

// LLen gets the length of a list.
func (t *TxStructure) LLen(key []byte) (int64, error) {
	metaKey := t.encodeListMetaKey(key)
	meta, err := t.loadListMeta(metaKey)
	return meta.RIndex - meta.LIndex, errors.Trace(err)
}

// LIndex gets an element from a list by its index.
func (t *TxStructure) LIndex(key []byte, index int64) ([]byte, error) {
	metaKey := t.encodeListMetaKey(key)
	meta, err := t.loadListMeta(metaKey)
	if err != nil || meta.IsEmpty() {
		return nil, errors.Trace(err)
	}

	index = adjustIndex(index, meta.LIndex, meta.RIndex)

	if index >= meta.LIndex && index < meta.RIndex {
		dataKey := t.encodeListDataKey(key, index)
		ret, err := t.reader.Get(dataKey)
		log.Warnf("index meta key %v, data key %v, ret %v, err %v", metaKey, dataKey, ret, err)
		return ret, errors.Trace(err)
	}
	return nil, nil
}

// LSet updates an element in the list by its index.
func (t *TxStructure) LSet(key []byte, index int64, value []byte) error {
	if t.readWriter == nil {
		return errWriteOnSnapshot
	}
	metaKey := t.encodeListMetaKey(key)
	meta, err := t.loadListMeta(metaKey)
	if err != nil || meta.IsEmpty() {
		return errors.Trace(err)
	}

	index = adjustIndex(index, meta.LIndex, meta.RIndex)

	if index >= meta.LIndex && index < meta.RIndex {
		return t.readWriter.Set(t.encodeListDataKey(key, index), value)
	}
	return errInvalidListIndex.Gen("invalid list index %d", index)
}

// LClear removes the list of the key.
func (t *TxStructure) LClear(key []byte) error {
	if t.readWriter == nil {
		return errWriteOnSnapshot
	}
	metaKey := t.encodeListMetaKey(key)
	meta, err := t.loadListMeta(metaKey)
	if err != nil || meta.IsEmpty() {
		return errors.Trace(err)
	}

	for index := meta.LIndex; index < meta.RIndex; index++ {
		dataKey := t.encodeListDataKey(key, index)
		if err = t.readWriter.Delete(dataKey); err != nil {
			return errors.Trace(err)
		}
	}

	return t.readWriter.Delete(metaKey)
}

func (t *TxStructure) loadListMeta(metaKey []byte) (listMeta, error) {
	v, err := t.reader.Get(metaKey)
	log.Infof("load list meta key %v, v %v, err %v", metaKey, v, err)
	if terror.ErrorEqual(err, kv.ErrNotExist) {
		err = nil
	} else if err != nil {
		return listMeta{}, errors.Trace(err)
	}

	meta := listMeta{0, 0}
	log.Infof("load list meta 000 %v, v %v, err %v", meta, v, err)
	if v == nil {
		return meta, nil
	}

	if len(v) != 16 {
		return meta, errInvalidListMetaData
	}

	meta.LIndex = int64(binary.BigEndian.Uint64(v[0:8]))
	meta.RIndex = int64(binary.BigEndian.Uint64(v[8:16]))
	log.Infof("load list meta === %v, v %v, err %v", meta, v, err)
	return meta, nil
}

func adjustIndex(index int64, min, max int64) int64 {
	if index >= 0 {
		return index + min
	}

	return index + max
}
