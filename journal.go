// +build linux

package log

import (
	"encoding/binary"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"os"
	"strings"
	"sync"
	"syscall"
)

// JournalWriter is an Writer that writes logs to journald.
type JournalWriter struct {
	// JournalSocket specifies socket name, using `/run/systemd/journal/socket` if empty.
	JournalSocket string

	once sync.Once
	addr *net.UnixAddr
	conn *net.UnixConn
}

// Close implements io.Closer.
func (w *JournalWriter) Close() (err error) {
	if w.conn != nil {
		err = w.conn.Close()
	}
	return
}

// WriteEntry implements Writer.
func (w *JournalWriter) WriteEntry(e *Entry) (n int, err error) {
	w.once.Do(func() {
		// unix addr
		w.addr = &net.UnixAddr{
			Net:  "unixgram",
			Name: w.JournalSocket,
		}
		if w.addr.Name == "" {
			w.addr.Name = "/run/systemd/journal/socket"
		}
		// unix conn
		var autobind *net.UnixAddr
		autobind, err = net.ResolveUnixAddr("unixgram", "")
		if err != nil {
			return
		}
		w.conn, err = net.ListenUnixgram("unixgram", autobind)
	})

	if err != nil {
		return
	}

	var t dot
	err = jsonToDot(e.buf, &t)
	if err != nil {
		return
	}

	// buffer
	b := bbpool.Get().(*bb)
	b.B = b.B[:0]
	defer bbpool.Put(b)

	print := func(w io.Writer, name, value string) {
		if strings.ContainsRune(value, '\n') {
			fmt.Fprintln(w, name)
			binary.Write(w, binary.LittleEndian, uint64(len(value)))
			fmt.Fprintln(w, value)
		} else {
			fmt.Fprintf(w, "%s=%s\n", name, value)
		}
	}

	// level
	var priority string
	switch t.Level {
	case TraceLevel:
		priority = "7" // Debug
	case DebugLevel:
		priority = "7" // Debug
	case InfoLevel:
		priority = "6" // Informational
	case WarnLevel:
		priority = "4" // Warning
	case ErrorLevel:
		priority = "3" // Error
	case FatalLevel:
		priority = "2" // Critical
	case PanicLevel:
		priority = "0" // Emergency
	default:
		priority = "5" // Notice
	}
	print(b, "PRIORITY", priority)

	// message
	print(b, "MESSAGE", t.Message)

	// fields
	for _, kv := range t.KeyValue {
		print(b, strings.ToUpper(kv.Key), kv.Value)
	}

	print(b, "JSON", b2s(e.buf))

	// write
	n, _, err = w.conn.WriteMsgUnix(b.B, nil, w.addr)
	if err == nil {
		return
	}

	opErr, ok := err.(*net.OpError)
	if !ok || opErr == nil {
		return
	}

	sysErr, ok := opErr.Err.(*os.SyscallError)
	if !ok || sysErr == nil {
		return
	}
	if sysErr.Err != syscall.EMSGSIZE && sysErr.Err != syscall.ENOBUFS {
		return
	}

	// Large log entry, send it via tempfile and ancillary-fd.
	var file *os.File
	file, err = ioutil.TempFile("/dev/shm/", "journal.XXXXX")
	if err != nil {
		return
	}
	err = syscall.Unlink(file.Name())
	if err != nil {
		return
	}
	defer file.Close()
	n, err = file.Write(b.B)
	if err != nil {
		return
	}
	rights := syscall.UnixRights(int(file.Fd()))
	_, _, err = w.conn.WriteMsgUnix([]byte{}, rights, w.addr)
	if err == nil {
		n = len(e.buf)
	}

	return
}

var _ Writer = (*JournalWriter)(nil)
