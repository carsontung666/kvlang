//go:build linux && cgo

package kvshm

/*
#cgo CFLAGS: -I${SRCDIR}/native/include
#cgo CXXFLAGS: -std=c++17 -O2 -fno-lto -fno-strict-aliasing -I${SRCDIR}/native/include -I${SRCDIR}/native/src
#cgo LDFLAGS: -lstdc++ -pthread -lrt
#include <stdlib.h>
#include "kvspace/kshm.h"
*/
import "C"

import (
	"bytes"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/array2d/kvspace-go"
)

type nativeClient struct {
	mu      sync.Mutex
	cond    *sync.Cond
	handle  *C.kshm_t
	active  int
	closing bool
	closed  bool
}

var _ kvspace.KVSpace = (*nativeClient)(nil)

func openNative(addr string, selected engine) kvspace.KVSpace {
	if addr == "" {
		addr = "kvlang"
	}
	if err := rejectNUL(addr, "shared-memory name"); err != nil {
		panic(err)
	}
	name := C.CString(addr)
	defer C.free(unsafe.Pointer(name))

	runtime.LockOSThread()
	var options C.kshm_options_t
	C.kshm_options_init(&options)
	options.engine_id = C.uint32_t(selected)
	handle := C.kshm_open_with_options(name, &options)
	var openErr error
	if handle == nil {
		openErr = fmt.Errorf("kvlang: cannot open shm %q: %s", addr, C.GoString(C.kshm_last_error()))
	}
	runtime.UnlockOSThread()
	if openErr != nil {
		panic(openErr)
	}

	client := &nativeClient{handle: handle}
	client.cond = sync.NewCond(&client.mu)
	return client
}

func rejectNUL(value, field string) error {
	if strings.IndexByte(value, 0) >= 0 {
		return fmt.Errorf("%w: embedded NUL in %s", kvspace.ErrInvalidPath, field)
	}
	return nil
}

func requireDirectory(value string) error {
	if !strings.HasSuffix(value, "/") {
		return fmt.Errorf("%w: %s", kvspace.ErrDirMustEndWithSlash, value)
	}
	return nil
}

func statusError(status C.int, message string) error {
	if status == C.KSHM_OK {
		return nil
	}
	var base error
	switch status {
	case C.KSHM_ERR_INVALID_PATH:
		base = kvspace.ErrInvalidPath
	case C.KSHM_ERR_INVALID_VALUE:
		base = kvspace.ErrInvalidValue
	case C.KSHM_ERR_INVALID_DIRECTORY_VALUE:
		base = kvspace.ErrInvalidDirValue
	case C.KSHM_ERR_DISCONNECTED:
		base = kvspace.ErrDisconnected
	case C.KSHM_ERR_RESOLVE:
		base = kvspace.ErrResolve
	case C.KSHM_ERR_EXT_WRITE:
		base = kvspace.ErrExtWrite
	case C.KSHM_ERR_EXT_DELETE:
		base = kvspace.ErrExtDel
	case C.KSHM_ERR_EXT_CASCADE:
		base = kvspace.ErrExtCascade
	case C.KSHM_ERR_EXT_TARGET:
		base = kvspace.ErrExtTarget
	case C.KSHM_ERR_EXT_COLLISION:
		base = kvspace.ErrExtCollision
	case C.KSHM_ERR_LINK_TYPE:
		base = kvspace.ErrLinkTypeMismatch
	case C.KSHM_ERR_LINK_EXISTS:
		base = kvspace.ErrLinkPathExists
	case C.KSHM_ERR_NOT_DIRECTORY:
		base = kvspace.ErrNotDir
	}
	if message == "" {
		message = fmt.Sprintf("native shm status %d", int(status))
	}
	if base != nil {
		return fmt.Errorf("%w: %s", base, message)
	}
	return fmt.Errorf("kvlang: native shm error (%d): %s", int(status), message)
}

func invokeStatus(call func() C.int) error {
	runtime.LockOSThread()
	status := call()
	var message string
	if status != C.KSHM_OK {
		message = C.GoString(C.kshm_last_error())
	}
	runtime.UnlockOSThread()
	return statusError(status, message)
}

func (c *nativeClient) begin() (*C.kshm_t, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closing || c.closed || c.handle == nil {
		return nil, kvspace.ErrDisconnected
	}
	c.active++
	return c.handle, nil
}

func (c *nativeClient) end() {
	c.mu.Lock()
	c.active--
	if c.active == 0 {
		c.cond.Broadcast()
	}
	c.mu.Unlock()
}

func (c *nativeClient) call(call func(*C.kshm_t) C.int) error {
	handle, err := c.begin()
	if err != nil {
		return err
	}
	defer c.end()
	return invokeStatus(func() C.int { return call(handle) })
}

func cString(value string) (*C.char, func()) {
	result := C.CString(value)
	return result, func() { C.free(unsafe.Pointer(result)) }
}

func cStringArray(values []string) (**C.char, func()) {
	if len(values) == 0 {
		return nil, func() {}
	}
	memory := C.calloc(C.size_t(len(values)), C.size_t(unsafe.Sizeof(uintptr(0))))
	if memory == nil {
		panic("kvlang: cannot allocate native string array")
	}
	items := unsafe.Slice((**C.char)(memory), len(values))
	for index, value := range values {
		items[index] = C.CString(value)
	}
	return (**C.char)(memory), func() {
		for _, item := range items {
			C.free(unsafe.Pointer(item))
		}
		C.free(memory)
	}
}

func sizeToInt(size C.size_t) int {
	value := uint64(size)
	maximum := uint64(^uint(0) >> 1)
	if value > maximum {
		panic("kvlang: native result is too large")
	}
	return int(value)
}

func copyNativeBytes(data *C.uint8_t, size C.size_t) []byte {
	length := sizeToInt(size)
	if length == 0 {
		return nil
	}
	if data == nil {
		panic("kvlang: native result has a null data pointer")
	}
	return append([]byte(nil), unsafe.Slice((*byte)(unsafe.Pointer(data)), length)...)
}

func decodeNativeValue(data *C.uint8_t, size C.size_t) kvspace.XValue {
	encoded := copyNativeBytes(data, size)
	if len(encoded) == 0 {
		return kvspace.None{}
	}
	head := kvspace.DecodeXValueHead(encoded)
	value := head.Decode()
	if head.Kind == "" || kvspace.IsNone(value) || !bytes.Equal(value.Encode(), encoded) {
		panic(fmt.Errorf("%w: malformed TLV returned by native shm", kvspace.ErrInvalidValue))
	}
	return value
}

func (c *nativeClient) Get(prefix string, keys []string) []kvspace.XValue {
	if err := requireDirectory(prefix); err != nil {
		panic(err)
	}
	if err := rejectNUL(prefix, "prefix"); err != nil {
		panic(err)
	}
	for _, key := range keys {
		if err := rejectNUL(key, "key"); err != nil {
			panic(err)
		}
	}
	prefixValue, freePrefix := cString(prefix)
	defer freePrefix()
	keyValues, freeKeys := cStringArray(keys)
	defer freeKeys()

	var output C.kshm_value_list_t
	err := c.call(func(handle *C.kshm_t) C.int {
		return C.kshm_get(handle, prefixValue, keyValues, C.size_t(len(keys)), &output)
	})
	if err != nil {
		panic(err)
	}
	defer C.kshm_value_list_free(&output)
	items := unsafe.Slice(output.items, sizeToInt(output.len))
	result := make([]kvspace.XValue, len(items))
	for index, item := range items {
		result[index] = decodeNativeValue(item.data, item.len)
	}
	return result
}

func (c *nativeClient) setPrefix(pairs []kvspace.KVPair) error {
	if len(pairs) == 0 {
		return c.call(func(handle *C.kshm_t) C.int {
			return C.kshm_set(handle, nil, 0)
		})
	}
	memory := C.calloc(C.size_t(len(pairs)), C.size_t(unsafe.Sizeof(C.kshm_kv_t{})))
	if memory == nil {
		return fmt.Errorf("kvlang: cannot allocate native pair array")
	}
	defer C.free(memory)
	items := unsafe.Slice((*C.kshm_kv_t)(memory), len(pairs))
	for index, pair := range pairs {
		items[index].key = C.CString(pair.Key)
		var encoded []byte
		if !kvspace.IsNone(pair.Val) {
			encoded = pair.Val.Encode()
		}
		if len(encoded) != 0 {
			items[index].value = (*C.uint8_t)(C.CBytes(encoded))
			items[index].value_len = C.size_t(len(encoded))
		}
	}
	defer func() {
		for index := range items {
			C.free(unsafe.Pointer(items[index].key))
			C.free(unsafe.Pointer(items[index].value))
		}
	}()
	return c.call(func(handle *C.kshm_t) C.int {
		return C.kshm_set(handle, (*C.kshm_kv_t)(memory), C.size_t(len(pairs)))
	})
}

func (c *nativeClient) Set(pairs []kvspace.KVPair) error {
	valid := len(pairs)
	var validationErr error
	for index, pair := range pairs {
		if err := rejectNUL(pair.Key, "key"); err != nil {
			valid = index
			validationErr = err
			break
		}
	}
	if err := c.setPrefix(pairs[:valid]); err != nil {
		return err
	}
	return validationErr
}

func (c *nativeClient) List(prefix string, expandExt bool) []string {
	if err := requireDirectory(prefix); err != nil {
		panic(err)
	}
	if err := rejectNUL(prefix, "prefix"); err != nil {
		panic(err)
	}
	prefixValue, freePrefix := cString(prefix)
	defer freePrefix()
	var output C.kshm_string_list_t
	expand := C.int(0)
	if expandExt {
		expand = 1
	}
	err := c.call(func(handle *C.kshm_t) C.int {
		return C.kshm_list(handle, prefixValue, expand, &output)
	})
	if err != nil {
		panic(err)
	}
	defer C.kshm_string_list_free(&output)
	items := unsafe.Slice(output.items, sizeToInt(output.len))
	result := make([]string, len(items))
	for index, item := range items {
		result[index] = string(copyNativeBytes((*C.uint8_t)(unsafe.Pointer(item.data)), item.len))
	}
	return result
}

func (c *nativeClient) delPrefix(keys []string) error {
	values, freeValues := cStringArray(keys)
	defer freeValues()
	return c.call(func(handle *C.kshm_t) C.int {
		return C.kshm_del(handle, values, C.size_t(len(keys)))
	})
}

func (c *nativeClient) Del(keys ...string) error {
	valid := len(keys)
	var validationErr error
	for index, key := range keys {
		if err := rejectNUL(key, "key"); err != nil {
			valid = index
			validationErr = err
			break
		}
	}
	if err := c.delPrefix(keys[:valid]); err != nil {
		return err
	}
	return validationErr
}

func (c *nativeClient) DelTree(prefix string) error {
	if err := rejectNUL(prefix, "prefix"); err != nil {
		return err
	}
	value, freeValue := cString(prefix)
	defer freeValue()
	return c.call(func(handle *C.kshm_t) C.int {
		return C.kshm_del_tree(handle, value)
	})
}

func (c *nativeClient) Notify(key string, value kvspace.XValue) error {
	if err := rejectNUL(key, "key"); err != nil {
		return err
	}
	keyValue, freeKey := cString(key)
	defer freeKey()
	var encoded []byte
	if !kvspace.IsNone(value) {
		encoded = value.Encode()
	}
	var data unsafe.Pointer
	if len(encoded) != 0 {
		data = C.CBytes(encoded)
		defer C.free(data)
	}
	return c.call(func(handle *C.kshm_t) C.int {
		return C.kshm_notify(
			handle,
			keyValue,
			(*C.uint8_t)(data),
			C.size_t(len(encoded)),
		)
	})
}

func (c *nativeClient) Watch(key string, timeout time.Duration) kvspace.XValue {
	if err := rejectNUL(key, "key"); err != nil {
		panic(err)
	}
	keyValue, freeKey := cString(key)
	defer freeKey()
	var milliseconds int64
	if timeout > 0 {
		milliseconds = int64(timeout / time.Millisecond)
		if timeout%time.Millisecond != 0 {
			milliseconds++
		}
	}
	var output C.kshm_value_t
	err := c.call(func(handle *C.kshm_t) C.int {
		return C.kshm_watch(handle, keyValue, C.int64_t(milliseconds), &output)
	})
	if err != nil {
		panic(err)
	}
	defer C.kshm_value_free(&output)
	return decodeNativeValue(output.data, output.len)
}

func (c *nativeClient) Mkindex(path string) error {
	if err := requireDirectory(path); err != nil {
		return err
	}
	if err := rejectNUL(path, "path"); err != nil {
		return err
	}
	value, freeValue := cString(path)
	defer freeValue()
	return c.call(func(handle *C.kshm_t) C.int {
		return C.kshm_mkindex(handle, value)
	})
}

func (c *nativeClient) Link(target, linkPath string) error {
	if err := rejectNUL(target, "target"); err != nil {
		return err
	}
	if err := rejectNUL(linkPath, "link path"); err != nil {
		return err
	}
	targetValue, freeTarget := cString(target)
	defer freeTarget()
	linkValue, freeLink := cString(linkPath)
	defer freeLink()
	return c.call(func(handle *C.kshm_t) C.int {
		return C.kshm_link(handle, targetValue, linkValue)
	})
}

func (c *nativeClient) ExtIndex(path, extPath string) error {
	if err := requireDirectory(path); err != nil {
		return err
	}
	if err := requireDirectory(extPath); err != nil {
		return err
	}
	if err := rejectNUL(path, "path"); err != nil {
		return err
	}
	if err := rejectNUL(extPath, "extension path"); err != nil {
		return err
	}
	pathValue, freePath := cString(path)
	defer freePath()
	extValue, freeExt := cString(extPath)
	defer freeExt()
	return c.call(func(handle *C.kshm_t) C.int {
		return C.kshm_ext_index(handle, pathValue, extValue)
	})
}

func (c *nativeClient) UnLink(path string) error {
	if err := rejectNUL(path, "path"); err != nil {
		return err
	}
	value, freeValue := cString(path)
	defer freeValue()
	return c.call(func(handle *C.kshm_t) C.int {
		return C.kshm_unlink(handle, value)
	})
}

func (c *nativeClient) Clear() error {
	return c.call(func(handle *C.kshm_t) C.int { return C.kshm_clear(handle) })
}

func (c *nativeClient) Compact() error {
	return c.call(func(handle *C.kshm_t) C.int { return C.kshm_compact(handle) })
}

func (c *nativeClient) engineID() (engine, error) {
	var stats C.kshm_stats_t
	err := c.call(func(handle *C.kshm_t) C.int {
		return C.kshm_stats(handle, &stats)
	})
	return engine(stats.engine_id), err
}

func (c *nativeClient) DisConn() error {
	c.mu.Lock()
	for c.closing && !c.closed {
		c.cond.Wait()
	}
	if c.closed || c.handle == nil {
		c.mu.Unlock()
		return nil
	}
	c.closing = true
	handle := c.handle
	c.mu.Unlock()

	disconnectErr := invokeStatus(func() C.int { return C.kshm_disconnect(handle) })

	c.mu.Lock()
	for c.active != 0 {
		c.cond.Wait()
	}
	c.handle = nil
	c.closed = true
	c.cond.Broadcast()
	c.mu.Unlock()

	C.kshm_close(handle)
	return disconnectErr
}

func destroyNative(name string) error {
	if err := rejectNUL(name, "shared-memory name"); err != nil {
		return err
	}
	value, freeValue := cString(name)
	defer freeValue()
	return invokeStatus(func() C.int { return C.kshm_destroy(value) })
}

// Destroy removes a named native shared-memory region after all clients close.
func Destroy(name string) error { return destroyNative(name) }
