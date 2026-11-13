package main

import (
	"bytes"
	"strings"
	"sync"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin/plugintest"
	"github.com/stretchr/testify/mock"
)

const adoptStripLockKeyPrefix = "mutex_adopt_strip_"

// fakeAdoptStripKV backs the adopt-strip markers and the strip cluster mutex
// with an in-memory map so adoption tests run the real state machine.
type fakeAdoptStripKV struct {
	mu   sync.Mutex
	data map[string][]byte
}

func (f *fakeAdoptStripKV) marker(channelID string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return string(f.data[adoptedStripKey(channelID)])
}

func (f *fakeAdoptStripKV) setMarker(channelID, value string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.data[adoptedStripKey(channelID)] = []byte(value)
}

func (f *fakeAdoptStripKV) lockHeld(channelID string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.data[adoptStripLockKeyPrefix+channelID]
	return ok
}

// expectSeededAdd stubs the plugin adding userID to channelID as a new member
// and recording it in the seed ledger.
func expectSeededAdd(api *plugintest.API, channelID, userID string) {
	key := taVisibilitySeededKey(channelID)
	api.On("GetChannelMember", channelID, userID).Return(nil, notFoundErr())
	api.On("AddChannelMember", channelID, userID).Return(&model.ChannelMember{}, nil)
	api.On("KVGet", key).Return(nil, nil).Maybe()
	api.On("KVCompareAndSet", key, mock.Anything, mock.Anything).Return(true, nil).Maybe()
}

// stubAdoptStripKV registers optional KVGet / KVSet / KVSetWithOptions stubs
// for adopt-strip keys. Register before broader KV stubs: testify uses the
// first matching expectation.
func stubAdoptStripKV(api *plugintest.API) *fakeAdoptStripKV {
	f := &fakeAdoptStripKV{data: map[string][]byte{}}
	isMarker := func(k string) bool { return strings.HasPrefix(k, kvAdoptedStripPrefix) }
	isLock := func(k string) bool { return strings.HasPrefix(k, adoptStripLockKeyPrefix) }

	api.On("KVGet", mock.MatchedBy(isMarker)).Return(func(k string) ([]byte, *model.AppError) {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.data[k], nil
	}).Maybe()
	api.On("KVSet", mock.MatchedBy(isMarker), mock.Anything).Return(func(k string, v []byte) *model.AppError {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.data[k] = v
		return nil
	}).Maybe()
	api.On("KVSetWithOptions", mock.MatchedBy(isLock), mock.Anything, mock.Anything).Return(
		func(k string, v []byte, o model.PluginKVSetOptions) (bool, *model.AppError) {
			f.mu.Lock()
			defer f.mu.Unlock()
			cur, exists := f.data[k]
			if o.Atomic {
				if o.OldValue == nil && exists {
					return false, nil
				}
				if o.OldValue != nil && (!exists || !bytes.Equal(cur, o.OldValue)) {
					return false, nil
				}
			}
			if v == nil {
				delete(f.data, k)
			} else {
				f.data[k] = v
			}
			return true, nil
		}).Maybe()
	return f
}
