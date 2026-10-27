package embedserver

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"unsafe"

	"github.com/ebitengine/purego"
)

// The indices below are the append-only OrtApi table in the v1.23.2 C header.
// Native handles and buffers never escape this adapter. Run owns its cancellation
// callback and waits for it before releasing the run options.
type ort struct {
	library, env, session, allocator uintptr
	errorMessage                     func(uintptr) string
	createEnv                        func(int32, string, *uintptr) uintptr
	createSession                    func(uintptr, unsafe.Pointer, uintptr, *uintptr) uintptr
	run                              func(uintptr, uintptr, []*byte, []uintptr, uintptr, []*byte, uintptr, *uintptr) uintptr
	createOptions                    func(*uintptr) uintptr
	setOptimization                  func(uintptr, int32) uintptr
	createRun                        func(*uintptr) uintptr
	terminate                        func(uintptr) uintptr
	createTensor                     func(uintptr, []int64, uintptr, int32, *uintptr) uintptr
	tensorData                       func(uintptr, *unsafe.Pointer) uintptr
	tensorShape                      func(uintptr, *uintptr) uintptr
	elementType                      func(uintptr, *int32) uintptr
	elementCount                     func(uintptr, *uintptr) uintptr
	getAllocator                     func(*uintptr) uintptr
	releaseEnv                       func(uintptr)
	releaseStatus                    func(uintptr)
	releaseSession                   func(uintptr)
	releaseValue                     func(uintptr)
	releaseRun                       func(uintptr)
	releaseShape                     func(uintptr)
	releaseOptions                   func(uintptr)
}

func openORT(library, model string) (_ *ort, err error) {
	o := new(ort)
	o.library, err = openLibrary(library)
	if err != nil {
		return nil, fmt.Errorf("load ONNX Runtime: %w", err)
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, o.Close())
		}
	}()
	symbol, err := librarySymbol(o.library, "OrtGetApiBase")
	if err != nil {
		return nil, err
	}
	var base func() unsafe.Pointer
	purego.RegisterFunc(&base, symbol)
	b := unsafe.Slice((*uintptr)(base()), 2)
	var getAPI func(uint32) unsafe.Pointer
	var version func() string
	purego.RegisterFunc(&getAPI, b[0])
	purego.RegisterFunc(&version, b[1])
	if v := version(); v != "1.23.2" {
		return nil, fmt.Errorf("ONNX Runtime %s: require 1.23.2", v)
	}
	table := getAPI(23)
	if table == nil {
		return nil, errors.New("ONNX Runtime API 23 unavailable")
	}
	api := unsafe.Slice((*uintptr)(table), 101)
	for _, binding := range []struct {
		fn    any
		index int
	}{
		{&o.errorMessage, 2},
		{&o.createEnv, 3},
		{&o.createSession, 7},
		{&o.run, 9},
		{&o.createOptions, 10},
		{&o.setOptimization, 23},
		{&o.createRun, 39},
		{&o.terminate, 46},
		{&o.createTensor, 48},
		{&o.tensorData, 51},
		{&o.elementType, 60},
		{&o.elementCount, 64},
		{&o.tensorShape, 65},
		{&o.getAllocator, 78},
		{&o.releaseEnv, 92},
		{&o.releaseStatus, 93},
		{&o.releaseSession, 95},
		{&o.releaseValue, 96},
		{&o.releaseRun, 97},
		{&o.releaseShape, 99},
		{&o.releaseOptions, 100},
	} {
		purego.RegisterFunc(binding.fn, api[binding.index])
	}
	if err := o.check(o.createEnv(3, "kit-embed", &o.env)); err != nil {
		return nil, err
	}
	var options uintptr
	if err := o.check(o.createOptions(&options)); err != nil {
		return nil, err
	}
	defer o.releaseOptions(options)
	// Extended layer-normalization fusion in 1.23.2 cannot load this FP16
	// graph. Basic optimizations preserve its cast nodes and numeric behavior.
	if err := o.check(o.setOptimization(options, 1)); err != nil {
		return nil, err
	}
	path, err := modelPath(model)
	if err != nil {
		return nil, err
	}
	var pin runtime.Pinner
	pin.Pin(path)
	defer pin.Unpin()
	status := o.createSession(o.env, path, options, &o.session)
	if err := o.check(status); err != nil {
		return nil, err
	}
	if err := o.check(o.getAllocator(&o.allocator)); err != nil {
		return nil, err
	}
	return o, nil
}

func (o *ort) check(status uintptr) error {
	if status == 0 {
		return nil
	}
	message := o.errorMessage(status)
	o.releaseStatus(status)
	return fmt.Errorf("ONNX Runtime: %s", message)
}

func (o *ort) Close() error {
	if o.session != 0 {
		o.releaseSession(o.session)
	}
	if o.env != 0 {
		o.releaseEnv(o.env)
	}
	return closeLibrary(o.library)
}

func (o *ort) tensor(values []int) (uintptr, error) {
	var value uintptr
	shape := []int64{1, int64(len(values))}
	if err := o.check(o.createTensor(o.allocator, shape, 2, 7, &value)); err != nil {
		return 0, err
	}
	var data unsafe.Pointer
	if err := o.check(o.tensorData(value, &data)); err != nil {
		o.releaseValue(value)
		return 0, err
	}
	for i := range values {
		unsafe.Slice((*int64)(data), len(values))[i] = int64(values[i])
	}
	return value, nil
}

func (o *ort) infer(ctx context.Context, ids, mask, types []int) ([]float32, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	values := make([]uintptr, 0, 3)
	defer func() {
		for _, v := range values {
			o.releaseValue(v)
		}
	}()
	for _, input := range [][]int{ids, mask, types} {
		value, err := o.tensor(input)
		if err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	var options uintptr
	if err := o.check(o.createRun(&options)); err != nil {
		return nil, err
	}
	defer o.releaseRun(options)
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { _ = o.check(o.terminate(options)); close(done) })
	defer func() {
		if !stop() {
			<-done
		}
	}()
	names := [][]byte{[]byte("input_ids\x00"), []byte("attention_mask\x00"), []byte("token_type_ids\x00"), []byte("last_hidden_state\x00")}
	var pin runtime.Pinner
	defer pin.Unpin()
	pointers := make([]*byte, len(names))
	for i := range names {
		pin.Pin(&names[i][0])
		pointers[i] = &names[i][0]
	}
	pin.Pin(&pointers[0])
	var output uintptr
	status := o.run(o.session, options, pointers[:3], values, 3, pointers[3:], 1, &output)
	if output != 0 {
		defer o.releaseValue(output)
	}
	if err := o.check(status); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var shape, count uintptr
	var dtype int32
	if err := o.check(o.tensorShape(output, &shape)); err != nil {
		return nil, err
	}
	defer o.releaseShape(shape)
	if err := o.check(o.elementType(shape, &dtype)); err != nil {
		return nil, err
	}
	if err := o.check(o.elementCount(shape, &count)); err != nil {
		return nil, err
	}
	if dtype != 1 || count != uintptr(len(ids)*768) {
		return nil, errors.New("unexpected ONNX output shape or type")
	}
	var data unsafe.Pointer
	if err := o.check(o.tensorData(output, &data)); err != nil {
		return nil, err
	}
	hidden := unsafe.Slice((*float32)(data), int(count))
	pooled := make([]float32, 768)
	for i, attended := range mask {
		if attended != 0 {
			for j := range pooled {
				pooled[j] += hidden[i*768+j]
			}
		}
	}
	normalize(pooled)
	return pooled, nil
}
