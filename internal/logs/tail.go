// Copyright 2026 The Loch.sh Authors
// SPDX-License-Identifier: Apache-2.0

package logs

// tail keeps the last size bytes written to it.
type tail struct {
	size    int
	buf     []byte
	midLine bool
}

func newTail(size int) *tail { return &tail{size: size, buf: make([]byte, 0, 2*size)} }

func (t *tail) write(p []byte) {
	if len(p) == 0 {
		return
	}
	if len(p) >= t.size {
		t.buf = append(t.buf[:0], p[len(p)-t.size:]...)
	} else {
		if len(t.buf)+len(p) > cap(t.buf) {
			keep := t.buf[len(t.buf)-(t.size-len(p)):]
			t.buf = append(t.buf[:0], keep...)
		}
		t.buf = append(t.buf, p...)
	}
	t.midLine = p[len(p)-1] != '\n'
}

func (t *tail) writeLine(p []byte) {
	if t.midLine {
		t.write([]byte{'\n'})
	}
	t.write(p)
}

// bytes returns a copy of the last size bytes.
func (t *tail) bytes() []byte {
	b := t.buf
	if len(b) > t.size {
		b = b[len(b)-t.size:]
	}
	return append([]byte(nil), b...)
}
