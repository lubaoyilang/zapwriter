// Copyright (c) 2016 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package zapwriter

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/uber-go/zap"
)

const (
	// For JSON-escaping; see mixedEncoder.safeAddString below.
	_hex = "0123456789abcdef"
	// Initial buffer size for encoders.
	_initialBufSize = 1024
)

var (
	// errNilSink signals that Encoder.WriteEntry was called with a nil WriteSyncer.
	errNilSink = errors.New("can't write encoded message a nil WriteSyncer")

	// Default formatters for JSON encoders.
	mixedPool = sync.Pool{New: func() interface{} {
		return &mixedEncoder{
			// Pre-allocate a reasonably-sized buffer for each encoder.
			bytes:     make([]byte, 0, _initialBufSize),
			formatter: defaultMixedFormatter,
		}
	}}
)

type MixedFormatter func(time.Time, zap.Level, string) string

var defaultMixedFormatterLevel = map[zap.Level]string{
	zap.DebugLevel: "D",
	zap.InfoLevel:  "I",
	zap.WarnLevel:  "W",
	zap.ErrorLevel: "E",
	zap.PanicLevel: "P",
	zap.FatalLevel: "F",
}

func defaultMixedFormatter(t time.Time, lvl zap.Level, msg string) string {
	return "[" + t.Local().Format(time.RFC3339) + "] " + defaultMixedFormatterLevel[lvl] + " " + msg + " "
}

// mixedEncoder is an Encoder implementation that writes JSON.
type mixedEncoder struct {
	bytes     []byte
	formatter MixedFormatter
}

// NewMixedEncoder creates a fast, low-allocation JSON encoder. By default, JSON
// encoders put the log message under the "msg" key, the timestamp (as
// floating-point seconds since epoch) under the "ts" key, and the log level
// under the "level" key. The encoder appropriately escapes all field keys and
// values.
//
// Note that the encoder doesn't deduplicate keys, so it's possible to produce a
// message like
//   {"foo":"bar","foo":"baz"}
// This is permitted by the JSON specification, but not encouraged. Many
// libraries will ignore duplicate key-value pairs (typically keeping the last
// pair) when unmarshaling, but users should attempt to avoid adding duplicate
// keys.
func NewMixedEncoder(formatter ...MixedFormatter) zap.Encoder {
	enc := mixedPool.Get().(*mixedEncoder)
	enc.truncate()

	if len(formatter) > 0 && formatter[0] != nil {
		enc.formatter = formatter[0]
	} else {
		enc.formatter = defaultMixedFormatter
	}

	return enc
}

func (enc *mixedEncoder) Free() {
	mixedPool.Put(enc)
}

// AddString adds a string key and value to the encoder's fields. Both key and
// value are JSON-escaped.
func (enc *mixedEncoder) AddString(key, val string) {
	enc.addKey(key)
	enc.bytes = append(enc.bytes, '"')
	enc.safeAddString(val)
	enc.bytes = append(enc.bytes, '"')
}

// AddBool adds a string key and a boolean value to the encoder's fields. The
// key is JSON-escaped.
func (enc *mixedEncoder) AddBool(key string, val bool) {
	enc.addKey(key)
	enc.bytes = strconv.AppendBool(enc.bytes, val)
}

// AddInt adds a string key and integer value to the encoder's fields. The key
// is JSON-escaped.
func (enc *mixedEncoder) AddInt(key string, val int) {
	enc.AddInt64(key, int64(val))
}

// AddInt64 adds a string key and int64 value to the encoder's fields. The key
// is JSON-escaped.
func (enc *mixedEncoder) AddInt64(key string, val int64) {
	enc.addKey(key)
	enc.bytes = strconv.AppendInt(enc.bytes, val, 10)
}

// AddUint adds a string key and integer value to the encoder's fields. The key
// is JSON-escaped.
func (enc *mixedEncoder) AddUint(key string, val uint) {
	enc.AddUint64(key, uint64(val))
}

// AddUint64 adds a string key and integer value to the encoder's fields. The key
// is JSON-escaped.
func (enc *mixedEncoder) AddUint64(key string, val uint64) {
	enc.addKey(key)
	enc.bytes = strconv.AppendUint(enc.bytes, val, 10)
}

func (enc *mixedEncoder) AddUintptr(key string, val uintptr) {
	enc.AddUint64(key, uint64(val))
}

// AddFloat64 adds a string key and float64 value to the encoder's fields. The
// key is JSON-escaped, and the floating-point value is encoded using
// strconv.FormatFloat's 'f' option (always use grade-school notation, even for
// large exponents).
func (enc *mixedEncoder) AddFloat64(key string, val float64) {
	enc.addKey(key)
	switch {
	case math.IsNaN(val):
		enc.bytes = append(enc.bytes, `"NaN"`...)
	case math.IsInf(val, 1):
		enc.bytes = append(enc.bytes, `"+Inf"`...)
	case math.IsInf(val, -1):
		enc.bytes = append(enc.bytes, `"-Inf"`...)
	default:
		enc.bytes = strconv.AppendFloat(enc.bytes, val, 'f', -1, 64)
	}
}

// AddMarshaler adds a LogMarshaler to the encoder's fields.
func (enc *mixedEncoder) AddMarshaler(key string, obj zap.LogMarshaler) error {
	enc.addKey(key)
	enc.bytes = append(enc.bytes, '{')
	err := obj.MarshalLog(enc)
	enc.bytes = append(enc.bytes, '}')
	return err
}

// AddObject uses reflection to add an arbitrary object to the logging context.
func (enc *mixedEncoder) AddObject(key string, obj interface{}) error {
	marshaled, err := json.Marshal(obj)
	if err != nil {
		return err
	}
	enc.addKey(key)
	enc.bytes = append(enc.bytes, marshaled...)
	return nil
}

// Clone copies the current encoder, including any data already encoded.
func (enc *mixedEncoder) Clone() zap.Encoder {
	clone := mixedPool.Get().(*mixedEncoder)
	clone.truncate()
	clone.bytes = append(clone.bytes, enc.bytes...)
	return clone
}

// WriteEntry writes a complete log message to the supplied writer, including
// the encoder's accumulated fields. It doesn't modify or lock the encoder's
// underlying byte slice. It's safe to call from multiple goroutines, but it's
// not safe to call WriteEntry while adding fields.
func (enc *mixedEncoder) WriteEntry(sink io.Writer, msg string, lvl zap.Level, t time.Time) error {
	if sink == nil {
		return errNilSink
	}

	final := mixedPool.Get().(*mixedEncoder)
	final.truncate()
	final.bytes = append(final.bytes, []byte(enc.formatter(t, lvl, msg))...)
	final.bytes = append(final.bytes, '{')

	if len(enc.bytes) > 0 {
		final.bytes = append(final.bytes, enc.bytes...)
	}
	final.bytes = append(final.bytes, '}', '\n')

	expectedBytes := len(final.bytes)
	n, err := sink.Write(final.bytes)
	final.Free()
	if err != nil {
		return err
	}
	if n != expectedBytes {
		return fmt.Errorf("incomplete write: only wrote %v of %v bytes", n, expectedBytes)
	}
	return nil
}

func (enc *mixedEncoder) truncate() {
	enc.bytes = enc.bytes[:0]
}

func (enc *mixedEncoder) addKey(key string) {
	last := len(enc.bytes) - 1
	// At some point, we'll also want to support arrays.
	if last >= 0 && enc.bytes[last] != '{' {
		enc.bytes = append(enc.bytes, ',')
	}
	enc.bytes = append(enc.bytes, '"')
	enc.safeAddString(key)
	enc.bytes = append(enc.bytes, '"', ':')
}

// safeAddString JSON-escapes a string and appends it to the internal buffer.
// Unlike the standard library's escaping function, it doesn't attempt to
// protect the user from browser vulnerabilities or JSONP-related problems.
func (enc *mixedEncoder) safeAddString(s string) {
	for i := 0; i < len(s); {
		if b := s[i]; b < utf8.RuneSelf {
			i++
			if 0x20 <= b && b != '\\' && b != '"' {
				enc.bytes = append(enc.bytes, b)
				continue
			}
			switch b {
			case '\\', '"':
				enc.bytes = append(enc.bytes, '\\', b)
			case '\n':
				enc.bytes = append(enc.bytes, '\\', 'n')
			case '\r':
				enc.bytes = append(enc.bytes, '\\', 'r')
			case '\t':
				enc.bytes = append(enc.bytes, '\\', 't')
			default:
				// Encode bytes < 0x20, except for the escape sequences above.
				enc.bytes = append(enc.bytes, `\u00`...)
				enc.bytes = append(enc.bytes, _hex[b>>4], _hex[b&0xF])
			}
			continue
		}
		c, size := utf8.DecodeRuneInString(s[i:])
		if c == utf8.RuneError && size == 1 {
			enc.bytes = append(enc.bytes, `\ufffd`...)
			i++
			continue
		}
		enc.bytes = append(enc.bytes, s[i:i+size]...)
		i += size
	}
}
