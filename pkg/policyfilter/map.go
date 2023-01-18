// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Tetragon

package policyfilter

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"unsafe"

	"github.com/cilium/ebpf"
	"github.com/cilium/tetragon/pkg/bpf"
	"github.com/cilium/tetragon/pkg/option"
	"github.com/cilium/tetragon/pkg/sensors/tracing"
)

// map operations used by policyfilter.

// pfMap is a simple wrapper for ebpf.Map so that we can write methods for it
type pfMap struct {
	*ebpf.Map
}

// newMap returns a new policy filter map.
func newPfMap() (pfMap, error) {
	// use the generic kprobe program, to find the policy filter map spec
	mapName := "policy_filter_maps"
	objName, _ := tracing.GenericKprobeObjs()
	objPath := path.Join(option.Config.HubbleLib, objName)
	spec, err := ebpf.LoadCollectionSpec(objPath)
	if err != nil {
		return pfMap{}, fmt.Errorf("loading spec for %s failed: %w", objPath, err)
	}
	policyMapSpec, ok := spec.Maps[mapName]
	if !ok {
		return pfMap{}, fmt.Errorf("%s not found in %s", mapName, objPath)
	}

	ret, err := ebpf.NewMap(policyMapSpec)
	if err != nil {
		return pfMap{}, err
	}

	pinPath := filepath.Join(bpf.MapPrefixPath(), mapName)
	os.Remove(pinPath)
	err = ret.Pin(pinPath)
	if err != nil {
		ret.Close()
		return pfMap{}, fmt.Errorf("failed to pin map: %w", err)
	}

	return pfMap{ret}, err
}

// release closes the policy filter bpf map and remove (unpin) the bpffs file
func (m pfMap) release() error {
	if err := m.Close(); err != nil {
		return err
	}

	if err := m.Unpin(); err != nil {
		return err
	}

	return nil
}

// addPolicyMap adds and initalizes a new policy map
func (m pfMap) newPolicyMap(polID PolicyID, cgIDs []CgroupID) (polMap, error) {
	name := fmt.Sprintf("policy_%d_map", polID)
	innerSpec := &ebpf.MapSpec{
		Name:       name,
		Type:       ebpf.Hash,
		KeySize:    uint32(unsafe.Sizeof(CgroupID(0))),
		ValueSize:  uint32(1),
		MaxEntries: uint32(polMapSize),
	}

	inner, err := ebpf.NewMap(innerSpec)
	if err != nil {
		return polMap{}, fmt.Errorf("failed to create policy (id=%d) map: %w", polID, err)
	}

	// update inner map with ids
	ret := polMap{inner}
	if err := ret.addCgroupIDs(cgIDs); err != nil {
		ret.Close()
		return polMap{}, fmt.Errorf("failed to update policy (id=%d): %w", polID, err)
	}

	// update outer map
	// NB(kkourt): use UpdateNoExist because we expect only a single policy with a given id
	if err := m.Update(polID, uint32(ret.FD()), ebpf.UpdateNoExist); err != nil {
		ret.Close()
		return polMap{}, fmt.Errorf("failed to insert innser policy (id=%d) map: %w", polID, err)
	}

	return ret, nil
}

func (m pfMap) readAll() (map[PolicyID]map[CgroupID]struct{}, error) {

	readInner := func(id uint32) (map[CgroupID]struct{}, error) {
		inMap, err := ebpf.NewMapFromID(ebpf.MapID(id))
		defer inMap.Close()
		if err != nil {
			return nil, fmt.Errorf("error opening inner map: %w", err)
		}

		inIter := inMap.Iterate()
		var key CgroupID
		var val uint8

		ret := map[CgroupID]struct{}{}
		for inIter.Next(&key, &val) {
			ret[key] = struct{}{}
		}

		if err := inIter.Err(); err != nil {
			return nil, fmt.Errorf("error iterating inner map: %w", err)
		}

		return ret, nil

	}

	ret := make(map[PolicyID]map[CgroupID]struct{})
	var key PolicyID
	var id uint32

	iter := m.Iterate()
	for iter.Next(&key, &id) {
		cgids, err := readInner(id)
		if err != nil {
			return nil, err
		}
		ret[key] = cgids
	}

	if err := iter.Err(); err != nil {
		return nil, fmt.Errorf("error iterating outer map: %w", err)
	}

	return ret, nil
}

// polMap is a simple wrapper for ebpf.Map so that we can write methods for it
type polMap struct {
	*ebpf.Map
}

type batchError struct {
	// SuccCount is the number of succesful operations
	SuccCount int
	err       error
}

func (e *batchError) Error() string {
	return e.err.Error()
}

func (e *batchError) Unwrap() error {
	return e.err
}

// addCgroupIDs add cgroups ids to the policy map
// todo: use batch operations when supported
func (m polMap) addCgroupIDs(cgIDs []CgroupID) error {
	var zero uint8
	for i, cgID := range cgIDs {
		if err := m.Update(&cgID, zero, ebpf.UpdateAny); err != nil {
			return &batchError{
				SuccCount: i,
				err:       fmt.Errorf("failed to update policy map (cgroup id: %d): %w", cgID, err),
			}
		}
	}

	return nil
}

// addCgroupIDs delete cgroups ids from the policy map
// todo: use batch operations when supported
func (m polMap) delCgroupIDs(cgIDs []CgroupID) error {
	for i, cgID := range cgIDs {
		if err := m.Delete(&cgID); err != nil {
			return &batchError{
				SuccCount: i,
				err:       fmt.Errorf("failed to delete items from policy map (cgroup id: %d): %w", cgID, err),
			}
		}
	}

	return nil
}
