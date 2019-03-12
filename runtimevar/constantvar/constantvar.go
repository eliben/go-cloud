// Copyright 2018 The Go Cloud Development Kit Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package constantvar provides a runtimevar implementation with Variables
// that never change. Use New, NewBytes, or NewError to construct a
// *runtimevar.Variable.
//
// URLs
//
// For runtimevar.OpenVariable URLs, constantvar registers for the scheme
// "constant". The host and path are ignored. It supports the following URL
// parameters:
//   - val: The value to use for the constant Variable. The bytes from val
//       are passed to NewBytes.
//   - decoder: The decoder to use. Defaults to runtimevar.BytesDecoder.
//       See runtimevar.DecoderByName for supported values.
//   - err: The error to use for the constant Variable. A new error is created
//       using errors.New and passed to NewError.
// If both "err" and "val" are provided, "val" is ignored.
// Example URL: "constant://?val=foo&decoder=string".
//
// As
//
// constantvar does not support any types for As.
package constantvar // import "gocloud.dev/runtimevar/constantvar"

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"time"

	"gocloud.dev/gcerrors"
	"gocloud.dev/runtimevar"
	"gocloud.dev/runtimevar/driver"
)

func init() {
	runtimevar.DefaultURLMux().RegisterVariable(Scheme, &URLOpener{})
}

// Scheme is the URL scheme constantvar registers its URLOpener under on blob.DefaultMux.
const Scheme = "constant"

// URLOpener opens Variable URLs like "constant://?val=foo&decoder=string".
type URLOpener struct{}

// OpenVariableURL opens the variable at the URL's path. See the package doc
// for more details.
func (*URLOpener) OpenVariableURL(ctx context.Context, u *url.URL) (*runtimevar.Variable, error) {
	q := u.Query()
	decoder, err := runtimevar.DecoderByName(q.Get("decoder"))
	if err != nil {
		return nil, fmt.Errorf("open variable %q: invalid \"decoder\": %v", u, err)
	}
	var value string
	var errVal error
	for param, values := range q {
		val := values[0]
		switch param {
		case "decoder":
			// processed elsewhere
		case "val":
			value = val
		case "err":
			errVal = errors.New(val)
		default:
			return nil, fmt.Errorf("open variable %q: invalid query parameter %q", u, param)
		}
	}
	if errVal != nil {
		return NewError(errVal), nil
	}
	return NewBytes([]byte(value), decoder), nil
}

var errNotExist = errors.New("variable does not exist")

// New constructs a *runtimevar.Variable holding value.
func New(value interface{}) *runtimevar.Variable {
	return runtimevar.New(&watcher{value: value, t: time.Now()})
}

// NewBytes uses decoder to decode b. If the decode succeeds, it constructs
// a *runtimevar.Variable holding the decoded value. If the decode fails, it
// constructs a runtimevar.Variable that always fails with the error.
func NewBytes(b []byte, decoder *runtimevar.Decoder) *runtimevar.Variable {
	value, err := decoder.Decode(b)
	if err != nil {
		return NewError(err)
	}
	return New(value)
}

// NewError constructs a *runtimevar.Variable that always fails. Runtimevar
// wraps errors returned by provider implementations, so the error returned
// by runtimevar will not equal err.
func NewError(err error) *runtimevar.Variable {
	return runtimevar.New(&watcher{err: err})
}

// watcher implements driver.Watcher and driver.State.
type watcher struct {
	value interface{}
	err   error
	t     time.Time
}

// Value implements driver.State.Value.
func (w *watcher) Value() (interface{}, error) {
	return w.value, w.err
}

// UpdateTime implements driver.State.UpdateTime.
func (w *watcher) UpdateTime() time.Time {
	return w.t
}

// As implements driver.State.As.
func (w *watcher) As(i interface{}) bool {
	return false
}

// WatchVariable implements driver.WatchVariable.
func (w *watcher) WatchVariable(ctx context.Context, prev driver.State) (driver.State, time.Duration) {
	// The first time this is called, return the constant value.
	if prev == nil {
		return w, 0
	}
	// On subsequent calls, block forever as the value will never change.
	<-ctx.Done()
	w.err = ctx.Err()
	return w, 0
}

// Close implements driver.Close.
func (*watcher) Close() error { return nil }

// ErrorAs implements driver.ErrorAs.
func (*watcher) ErrorAs(err error, i interface{}) bool { return false }

// ErrorCode implements driver.ErrorCode
func (*watcher) ErrorCode(err error) gcerrors.ErrorCode {
	if err == errNotExist {
		return gcerrors.NotFound
	}
	return gcerrors.Unknown
}
