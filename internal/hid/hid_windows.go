//go:build windows

package hid

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// SystemOpener is the Windows HID backend (hid.dll + CfgMgr32).
type SystemOpener struct{}

// System is the platform HID backend.
var System Opener = SystemOpener{}

// guidHID is GUID_DEVINTERFACE_HID {4D1E55B2-F16F-11CF-88CB-001111000030}.
var guidHID = windows.GUID{
	Data1: 0x4D1E55B2,
	Data2: 0xF16F,
	Data3: 0x11CF,
	Data4: [8]byte{0x88, 0xCB, 0x00, 0x11, 0x11, 0x00, 0x00, 0x30},
}

const hidpStatusSuccess = 0x00110000 // HIDP_STATUS_SUCCESS

var (
	hidDLL                    = windows.NewLazySystemDLL("hid.dll")
	procHidDGetAttributes     = hidDLL.NewProc("HidD_GetAttributes")
	procHidDGetPreparsedData  = hidDLL.NewProc("HidD_GetPreparsedData")
	procHidDFreePreparsedData = hidDLL.NewProc("HidD_FreePreparsedData")
	procHidPGetCaps           = hidDLL.NewProc("HidP_GetCaps")
	procHidDGetProductString  = hidDLL.NewProc("HidD_GetProductString")

	loadOnce sync.Once
	loadErr  error
)

// loadHID resolves all hid.dll procedures up front so LazyProc.Call never panics.
func loadHID() error {
	loadOnce.Do(func() {
		for _, p := range []*windows.LazyProc{
			procHidDGetAttributes, procHidDGetPreparsedData, procHidDFreePreparsedData,
			procHidPGetCaps, procHidDGetProductString,
		} {
			if err := p.Find(); err != nil {
				loadErr = fmt.Errorf("hid: load hid.dll: %w", err)
				return
			}
		}
	})
	return loadErr
}

// hiddAttributes is HIDD_ATTRIBUTES (sizeof 12 on Windows; Go pads identically).
type hiddAttributes struct {
	Size          uint32
	VendorID      uint16
	ProductID     uint16
	VersionNumber uint16
}

// hidpCaps is HIDP_CAPS. Note Usage precedes UsagePage.
type hidpCaps struct {
	Usage                     uint16
	UsagePage                 uint16
	InputReportByteLength     uint16
	OutputReportByteLength    uint16
	FeatureReportByteLength   uint16
	Reserved                  [17]uint16
	NumberLinkCollectionNodes uint16
	NumberInputButtonCaps     uint16
	NumberInputValueCaps      uint16
	NumberInputDataIndices    uint16
	NumberOutputButtonCaps    uint16
	NumberOutputValueCaps     uint16
	NumberOutputDataIndices   uint16
	NumberFeatureButtonCaps   uint16
	NumberFeatureValueCaps    uint16
	NumberFeatureDataIndices  uint16
}

// HidD_* return BOOLEAN (1 byte): only the low byte of the return register is defined.
func boolOK(r1 uintptr) bool { return byte(r1) != 0 }

func getAttributes(h windows.Handle) (hiddAttributes, error) {
	a := hiddAttributes{Size: uint32(unsafe.Sizeof(hiddAttributes{}))}
	r1, _, e := procHidDGetAttributes.Call(uintptr(h), uintptr(unsafe.Pointer(&a)))
	if !boolOK(r1) {
		return a, fmt.Errorf("hid: HidD_GetAttributes: %w", e)
	}
	return a, nil
}

func getCaps(h windows.Handle) (hidpCaps, error) {
	var caps hidpCaps
	var pp uintptr // PHIDP_PREPARSED_DATA
	r1, _, e := procHidDGetPreparsedData.Call(uintptr(h), uintptr(unsafe.Pointer(&pp)))
	if !boolOK(r1) {
		return caps, fmt.Errorf("hid: HidD_GetPreparsedData: %w", e)
	}
	defer procHidDFreePreparsedData.Call(pp)
	st, _, _ := procHidPGetCaps.Call(pp, uintptr(unsafe.Pointer(&caps)))
	if uint32(st) != hidpStatusSuccess {
		return caps, fmt.Errorf("hid: HidP_GetCaps: status 0x%08x", uint32(st))
	}
	return caps, nil
}

func getProduct(h windows.Handle) string {
	var buf [128]uint16 // 256 bytes; USB string descriptors are at most 126 bytes
	r1, _, _ := procHidDGetProductString.Call(uintptr(h),
		uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)*2))
	if !boolOK(r1) {
		return ""
	}
	buf[len(buf)-1] = 0
	return windows.UTF16ToString(buf[:])
}

func openPath(path string, access, flags uint32) (windows.Handle, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return windows.InvalidHandle, err
	}
	return windows.CreateFile(p, access,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil,
		windows.OPEN_EXISTING, flags, 0)
}

// Enumerate lists present HID interfaces with the given vendor ID (0 = all).
func (SystemOpener) Enumerate(vid uint16) ([]Info, error) {
	if err := loadHID(); err != nil {
		return nil, err
	}
	paths, err := windows.CM_Get_Device_Interface_List("", &guidHID, windows.CM_GET_DEVICE_INTERFACE_LIST_PRESENT)
	if err != nil {
		if errors.Is(err, windows.ERROR_NO_SUCH_DEVICE_INTERFACE) {
			return nil, nil
		}
		return nil, fmt.Errorf("hid: list interfaces: %w", err)
	}
	var out []Info
	for _, path := range paths {
		info, ok := probe(path, vid)
		if ok {
			out = append(out, info)
		}
	}
	return out, nil
}

// probe opens path with zero access (works even for system keyboard/mouse
// collections) and reads attributes, caps and product string.
func probe(path string, vid uint16) (Info, bool) {
	h, err := openPath(path, 0, 0)
	if err != nil {
		return Info{}, false
	}
	defer windows.CloseHandle(h)
	attr, err := getAttributes(h)
	if err != nil || (vid != 0 && attr.VendorID != vid) {
		return Info{}, false
	}
	caps, err := getCaps(h)
	if err != nil {
		return Info{}, false
	}
	return Info{
		Path:      path,
		Product:   getProduct(h),
		VendorID:  attr.VendorID,
		ProductID: attr.ProductID,
		UsagePage: caps.UsagePage,
		Usage:     caps.Usage,
		InputLen:  int(caps.InputReportByteLength),
		OutputLen: int(caps.OutputReportByteLength),
	}, true
}

// Open opens a HID interface for overlapped read/write.
func (SystemOpener) Open(path string) (Device, error) {
	if err := loadHID(); err != nil {
		return nil, err
	}
	h, err := openPath(path, windows.GENERIC_READ|windows.GENERIC_WRITE, windows.FILE_FLAG_OVERLAPPED)
	if err != nil {
		return nil, fmt.Errorf("hid: open %s: %w", path, err)
	}
	caps, err := getCaps(h)
	if err == nil && (caps.InputReportByteLength == 0 || caps.OutputReportByteLength == 0) {
		err = fmt.Errorf("hid: %s has no input or output reports", path)
	}
	if err != nil {
		windows.CloseHandle(h)
		return nil, err
	}
	d := &device{
		h:    h,
		rbuf: make([]byte, caps.InputReportByteLength),
		wbuf: make([]byte, caps.OutputReportByteLength),
	}
	var evs [3]windows.Handle
	for i := range evs {
		// Manual-reset, initially non-signalled.
		if evs[i], err = windows.CreateEvent(nil, 1, 0, nil); err != nil {
			for _, e := range evs[:i] {
				windows.CloseHandle(e)
			}
			windows.CloseHandle(h)
			return nil, fmt.Errorf("hid: CreateEvent: %w", err)
		}
	}
	d.rov.HEvent, d.wov.HEvent, d.closeEv = evs[0], evs[1], evs[2]
	d.rwait = [2]windows.Handle{d.rov.HEvent, d.closeEv}
	d.wwait = [2]windows.Handle{d.wov.HEvent, d.closeEv}
	return d, nil
}

// device is an open HID handle. It is always heap-allocated and never copied,
// so the OVERLAPPED structs, buffers and wait arrays handed to the kernel stay
// at fixed addresses (Go's heap does not move) and are kept alive by d for as
// long as any I/O may reference them. No method returns while its I/O is
// still pending.
type device struct {
	h       windows.Handle
	closeEv windows.Handle // manual-reset; signalled by Close to abort waits
	closed  atomic.Bool

	rmu   sync.Mutex // serialises Read; guards rov, rbuf, rwait
	rov   windows.Overlapped
	rbuf  []byte
	rwait [2]windows.Handle

	wmu   sync.Mutex // serialises Write; guards wov, wbuf, wwait
	wov   windows.Overlapped
	wbuf  []byte
	wwait [2]windows.Handle
}

type ioStatus int

const (
	ioDone ioStatus = iota
	ioTimeout
	ioClosed
)

// millis converts a timeout to a Win32 wait value: negative waits forever,
// positive values round up to whole milliseconds.
func millis(d time.Duration) uint32 {
	if d < 0 {
		return windows.INFINITE
	}
	ms := (d + time.Millisecond - 1) / time.Millisecond
	if ms >= windows.INFINITE {
		return windows.INFINITE - 1
	}
	return uint32(ms)
}

// complete finishes an overlapped operation on ov whose ReadFile/WriteFile
// call returned startErr. On timeout or Close it cancels the operation with
// CancelIoEx and then blocks in GetOverlappedResult until the kernel has
// finished with ov and the buffer. If the operation completed despite the
// cancellation, its result is returned as ioDone.
func (d *device) complete(ov *windows.Overlapped, wait *[2]windows.Handle, startErr error, timeout time.Duration) (uint32, ioStatus, error) {
	if startErr != nil && startErr != windows.ERROR_IO_PENDING {
		return 0, ioDone, startErr // never started; nothing in flight
	}
	status := ioDone
	var waitErr error
	if startErr == windows.ERROR_IO_PENDING {
		ev, err := windows.WaitForMultipleObjects(wait[:], false, millis(timeout))
		switch {
		case err != nil:
			waitErr = err
		case ev == windows.WAIT_OBJECT_0:
			// I/O completed.
		case ev == windows.WAIT_OBJECT_0+1:
			status = ioClosed
		case ev == uint32(windows.WAIT_TIMEOUT):
			status = ioTimeout
		default:
			waitErr = fmt.Errorf("hid: unexpected wait result 0x%x", ev)
		}
		if status != ioDone || waitErr != nil {
			// ERROR_NOT_FOUND here just means it already completed.
			_ = windows.CancelIoEx(d.h, ov)
		}
	}
	var n uint32
	err := windows.GetOverlappedResult(d.h, ov, &n, true)
	switch {
	case waitErr != nil:
		return 0, ioDone, waitErr
	case err == nil:
		return n, ioDone, nil
	case err == windows.ERROR_OPERATION_ABORTED && status != ioDone:
		return 0, status, nil
	default:
		return 0, ioDone, err
	}
}

// Read reads one input report (including the report ID byte) into buf,
// truncating if buf is short. It returns (0, nil) on timeout.
func (d *device) Read(buf []byte, timeout time.Duration) (int, error) {
	d.rmu.Lock()
	defer d.rmu.Unlock()
	if d.closed.Load() {
		return 0, ErrClosed
	}
	_ = windows.ResetEvent(d.rov.HEvent)
	var done uint32
	startErr := windows.ReadFile(d.h, d.rbuf, &done, &d.rov)
	n, status, err := d.complete(&d.rov, &d.rwait, startErr, timeout)
	switch {
	case status == ioClosed:
		return 0, ErrClosed
	case err != nil:
		if d.closed.Load() {
			return 0, ErrClosed
		}
		return 0, fmt.Errorf("hid: read: %w", err)
	case status == ioTimeout:
		return 0, nil
	}
	return copy(buf, d.rbuf[:n]), nil
}

// Write sends one output report, zero-padded or truncated to the device's
// output report length. A timeout is an error.
func (d *device) Write(report []byte, timeout time.Duration) error {
	d.wmu.Lock()
	defer d.wmu.Unlock()
	if d.closed.Load() {
		return ErrClosed
	}
	clear(d.wbuf)
	copy(d.wbuf, report)
	_ = windows.ResetEvent(d.wov.HEvent)
	var done uint32
	startErr := windows.WriteFile(d.h, d.wbuf, &done, &d.wov)
	n, status, err := d.complete(&d.wov, &d.wwait, startErr, timeout)
	switch {
	case status == ioClosed:
		return ErrClosed
	case err != nil:
		if d.closed.Load() {
			return ErrClosed
		}
		return fmt.Errorf("hid: write: %w", err)
	case status == ioTimeout:
		return ErrWriteTimeout
	case int(n) != len(d.wbuf):
		return fmt.Errorf("hid: short write: %d of %d bytes", n, len(d.wbuf))
	}
	return nil
}

// Close aborts any in-flight Read/Write, waits for them to return, then
// releases the handle and events. It is safe to call concurrently with Read
// and Write, and more than once.
func (d *device) Close() error {
	if !d.closed.CompareAndSwap(false, true) {
		return nil
	}
	_ = windows.SetEvent(d.closeEv) // wakes any pending wait; it then cancels and drains its own I/O
	d.rmu.Lock()
	d.wmu.Lock()
	defer d.wmu.Unlock()
	defer d.rmu.Unlock()
	err := windows.CloseHandle(d.h)
	windows.CloseHandle(d.rov.HEvent)
	windows.CloseHandle(d.wov.HEvent)
	windows.CloseHandle(d.closeEv)
	d.h = windows.InvalidHandle
	return err
}
