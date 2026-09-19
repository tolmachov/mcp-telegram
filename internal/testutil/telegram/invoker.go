// Package telegram provides one concurrency-safe typed fake for Telegram RPC
// tests across tools, messages, resources, and client code.
package telegram

import (
	"context"
	"fmt"
	"reflect"
	"sync"

	"github.com/gotd/td/bin"
)

type InvokeFunc func(context.Context, bin.Encoder, bin.Decoder) error

// Typed adapts a type-safe request/response handler to InvokeFunc. A test that
// wires the wrong Telegram RPC type fails with an explicit type error.
func Typed[I bin.Encoder, O bin.Decoder](handler func(context.Context, I, O) error) InvokeFunc {
	return func(ctx context.Context, input bin.Encoder, output bin.Decoder) error {
		typedInput, ok := input.(I)
		if !ok {
			return fmt.Errorf("telegram fake: input is %T, want %s", input, reflect.TypeFor[I]())
		}
		typedOutput, ok := output.(O)
		if !ok {
			return fmt.Errorf("telegram fake: output is %T, want %s", output, reflect.TypeFor[O]())
		}
		return handler(ctx, typedInput, typedOutput)
	}
}

// Invoker executes a FIFO script and records the concrete request types. It is
// safe for concurrent calls, which lets cache/singleflight tests use the same
// fake as ordinary handler tests.
type Invoker struct {
	mu      sync.Mutex
	script  []InvokeFunc
	request []reflect.Type
}

func New(script ...InvokeFunc) *Invoker {
	return &Invoker{script: append([]InvokeFunc(nil), script...)}
}

func (f *Invoker) Invoke(ctx context.Context, input bin.Encoder, output bin.Decoder) error {
	f.mu.Lock()
	f.request = append(f.request, reflect.TypeOf(input))
	if len(f.script) == 0 {
		f.mu.Unlock()
		return fmt.Errorf("telegram fake: unexpected request %T", input)
	}
	next := f.script[0]
	f.script = f.script[1:]
	f.mu.Unlock()
	return next(ctx, input, output)
}

func (f *Invoker) RequestTypes() []reflect.Type {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]reflect.Type(nil), f.request...)
}

func (f *Invoker) Remaining() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.script)
}
