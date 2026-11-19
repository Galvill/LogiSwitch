# LogiSwitch Implementation Plan

**Spec:** `docs/superpowers/specs/2026-11-18-logiswitch-design.md`, sections Constraints, Behaviour, Command-line interface, Configuration, Logging, Testing, Risks.
**Goal:** a `logiswitch.exe` Windows service that diverts the MX Keys Easy-Switch keys and moves mouse then keyboard to the pressed slot, with a pending mouse switch for an asleep mouse.
**Constraints:** the spec's `## Constraints` section applies. Additionally:
- Module path `logiswitch`, Go 1.25, `go.mod` requires only `golang.org/x/sys`.
- Every package with `//go:build windows` files has a `!windows` counterpart so `go test ./...` and `go vet ./...` pass on Linux.
- Task 5 is a gate: Tasks 6+ start only after the user reports the spike result.
**Verification:** the repo documents none; run all of these, and each task's own target:
- `go test ./...` and `go vet ./...` (Linux)
- `GOOS=windows GOARCH=amd64 go vet ./...`
- `GOOS=windows GOARCH=amd64 go build -o dist/logiswitch.exe ./cmd/logiswitch`

## File structure

| File | Responsibility |
|---|---|
| `go.mod`, `.gitignore` | module; ignore `dist/` |
| `internal/hid/hid.go` | `Info`, `Device`, `Opener` types, errors (all platforms) |
| `internal/hid/hid_windows.go` | enumeration, open, overlapped read/write via `hid.dll`, cfgmgr32 |
| `internal/hid/hid_other.go` | `Enumerate`/`Open` return `ErrUnsupported` |
| `internal/hidpp/message.go` | HID++ long report encode/decode, error decode |
| `internal/hidpp/conn.go` | reader goroutine, request/response matching, notifications |
| `internal/hidpp/features.go` | root, CHANGE_HOST, REPROG_CONTROLS_V4 calls |
| `internal/hidpp/hidpptest/fake.go` | simulated HID++ BLE device implementing `hid.Device` |
| `cmd/spike/main_windows.go`, `main_other.go` | throwaway service for the session-0 gate |
| `internal/config/config.go` | load `config.json`, defaults, validation |
| `internal/logging/rotate.go` | size-capped log file writer |
| `internal/switcher/switcher.go` | device registry, discovery, divert upkeep, `Tick`, `Run`, `Release` |
| `internal/switcher/switch.go` | press handling, mouse/keyboard sends, pending mouse switch |
| `internal/switcher/status.go` | `Status`, `SetAll` for `list` / `set N` |
| `cmd/logiswitch/main_windows.go` | CLI dispatch, `run`, `list`, `set` |
| `cmd/logiswitch/service_windows.go` | `svc.Handler`, event log |
| `cmd/logiswitch/install_windows.go` | `install`, `uninstall`, `start`, `stop` via `svc/mgr` |
| `cmd/logiswitch/main_other.go` | prints "Windows only", exit 1 |
| `README.md` | install, usage, pairing, crash recovery, Options+ note |

### Task 1: Module, `hid` types, HID++ framing and connection

**Files:**
- Create: `go.mod`, `.gitignore`, `internal/hid/hid.go`, `internal/hidpp/message.go`, `internal/hidpp/conn.go`
- Test: `internal/hidpp/message_test.go`, `internal/hidpp/conn_test.go`

**Interfaces:**
- Produces (`hidpp`): `NewConn(d hid.Device, dev byte) *Conn`; `(*Conn).Request(feat, fn byte, params []byte, timeout time.Duration) (Message, error)`; `(*Conn).Send(feat, fn byte, params []byte, timeout time.Duration) error`; `(*Conn).Notifications() <-chan Message` (buffered 16); `(*Conn).Close() error`; `ErrTimeout`; `DeviceError{Code byte}`.

```go
// package hid
type Info struct{ Path, Product string; VendorID, ProductID, UsagePage, Usage uint16; InputLen, OutputLen int }
type Device interface {
    Read(buf []byte, timeout time.Duration) (int, error) // (0, nil) on timeout
    Write(report []byte, timeout time.Duration) error
    Close() error
}
type Opener interface{ Enumerate(vid uint16) ([]Info, error); Open(path string) (Device, error) }
var ErrUnsupported, ErrClosed error
// package hidpp
type Message struct{ Dev, Feature, Function, SWID byte; Params [16]byte }
func Encode(m Message) []byte         // 20 bytes, [0]=0x11
func Decode(b []byte) (Message, bool) // false unless len>=20 && b[0]==0x11
```

**Behavior:**
- Requests use SWID `0x07`; reply matches on `Dev`, `Feature`, `Function`, `SWID`.
- Reply `Feature==0xFF`, `Params[0..1]` = request feature, fn|swid → `DeviceError{Params[2]}`.
- `SWID==0` → `Notifications()`; when full, drop the oldest.
- No match in time → `ErrTimeout`. Read error → returned by pending and later calls; channel closed.
- One request in flight per `Conn`.

**Tests:**
- `TestEncodeDecodeRoundTrip`, `TestDecodeRejectsShortReport`.
- `TestRequestSkipsInterleavedNotification`: a notification before the reply is delivered to the channel and the reply is returned.
- `TestRequestDeviceError`: error reply → `DeviceError{Code: 0x05}`.
- `TestRequestTimeout`, `TestReadErrorFailsRequests`.

**Gotchas:**
- Use a tiny scripted `hid.Device` stub inside `conn_test.go`; the full fake comes in Task 2.

### Task 2: Fake HID++ device

**Files:**
- Create: `internal/hidpp/hidpptest/fake.go`
- Test: `internal/hidpp/hidpptest/fake_test.go`

**Interfaces:**
- Consumes: `hid.Device`, `hidpp.Encode/Decode` from Task 1.
- Produces:

```go
type Fake struct{ /* mutex-guarded */ }
func NewKeyboard(currentHost int) *Fake   // 0x1814 @9, 0x1B04 @8, CIDs D1–D3 flags 0x64
func NewMouse(currentHost int) *Fake      // 0x1814 @10, 0x1B04 @9, no host CIDs
func (f *Fake) Disconnect(); func (f *Fake) Reconnect() // reconnect clears diversion
func (f *Fake) Press(cid uint16)          // emits press + release notifications if diverted
func (f *Fake) SetWriteDelay(d time.Duration)
func (f *Fake) Diverted(cid uint16) bool; func (f *Fake) Host() int; func (f *Fake) SetHostCalls() []int
```

**Behavior:**
- Answers root fn0 getFeature (unknown → index 0), CHANGE_HOST fn0 `[n=3, current]`, fn1 setHost (no reply; records slot, then behaves disconnected), REPROG fn0/fn1/fn2/fn3 per probe layout.
- While disconnected: `Write` returns an error after the write delay; `Read` times out.

**Tests:**
- `TestFakeDivertAndPress`: after fn3 divert D2, `Press(0xD2)` yields a notification listing `0x00D2`, then an all-zero one.
- `TestFakeReconnectClearsDivert`.

**Gotchas:**
- getCidReporting reply: `Params[0..1]`=CID, `Params[2]` bit0 = diverted. Match `probe_easyswitch.py` byte offsets.

### Task 3: HID++ feature calls

**Files:**
- Create: `internal/hidpp/features.go`
- Test: `internal/hidpp/features_test.go`

**Interfaces:**
- Consumes: `Conn` (Task 1), `hidpptest` (Task 2).
- Produces:

```go
const FeatChangeHost, FeatReprogV4 = 0x1814, 0x1B04
var HostSwitchCIDs = [3]uint16{0xD1, 0xD2, 0xD3}
var ErrNoFeature error
func (c *Conn) FeatureIndex(id uint16) (byte, error)
func (c *Conn) HostInfo(fi byte) (n, current int, err error)   // 0-based current
func (c *Conn) SetHost(fi byte, slot int, timeout time.Duration) error
func (c *Conn) Controls(fi byte) ([]Control, error)            // Control{CID uint16; Flags byte}
func (c *Conn) Diverted(fi byte, cid uint16) (bool, error)
func (c *Conn) SetDiverted(fi byte, cid uint16, on bool) error
func DivertedCIDs(m Message) []uint16                          // non-zero CIDs of fn0 event
```

**Behavior:**
- `FeatureIndex` reply index 0 → `ErrNoFeature`.
- `SetDiverted(on)` sends flags `0x03`, off `0x02`.
- `SetHost` uses `Send` (no reply awaited).

**Tests:**
- `TestFeatureIndexMissing`, `TestHostInfo`, `TestControlsListsHostCIDs` (flags `0x64`), `TestDivertRoundTrip`, `TestDivertedCIDsParsesRelease` (all-zero → empty).

### Task 4: Windows HID transport

**Files:**
- Create: `internal/hid/hid_windows.go`, `internal/hid/hid_other.go`

**Interfaces:**
- Produces: `hid.System` (value implementing `Opener`) on both platforms.

**Behavior:**
- `Enumerate(0x046D)` lists present HID interfaces (GUID `{4D1E55B2-F16F-11CF-88CB-001111000030}`) via `windows.CM_Get_Device_Interface_List` with `CM_GET_DEVICE_INTERFACE_LIST_PRESENT`; fills `Info` from `HidD_GetAttributes`, `HidD_GetPreparsedData`+`HidP_GetCaps`, `HidD_GetProductString`. Interfaces that fail to open with zero access are skipped.
- `Open` uses `CreateFile(GENERIC_READ|GENERIC_WRITE, FILE_SHARE_READ|FILE_SHARE_WRITE, OPEN_EXISTING, FILE_FLAG_OVERLAPPED)`.
- `Read`/`Write`: overlapped I/O, `WaitForSingleObject(timeout)`, on timeout `CancelIoEx` and wait for completion before reusing the buffer; read timeout → `(0, nil)`, write timeout → error.
- `Write` pads/truncates to `OutputLen`.
- Non-Windows: `Enumerate`/`Open` → `ErrUnsupported`.

**Tests:**
- None automated (hardware). Verified by `GOOS=windows go vet` and Task 5.

**Gotchas:**
- Enumerate with access 0 (attributes work without RW); keyboard/mouse system collections deny RW, only the vendor collection opens RW.
- `hid.dll` via `windows.NewLazySystemDLL`; `HidP_GetCaps` returns `HIDP_STATUS_SUCCESS = 0x00110000`.
- Product string is UTF-16, ≤ 126 bytes.

### Task 5: Session-0 spike (gate)

**Files:**
- Create: `cmd/spike/main_windows.go`, `cmd/spike/main_other.go`

**Interfaces:**
- Consumes: `hid.System`, `hidpp.Conn` features (Tasks 1–4).

**Behavior:**
- `spike.exe install|uninstall` registers/removes auto-start LocalSystem service `LogiSwitchSpike`; `spike.exe run` runs in console.
- Loop: every 2 s find keyboard and mouse (usage `0xFF43/0x0202`), divert D1–D3 if not diverted; on press N send mouse `SetHost(N)` then keyboard `SetHost(N)`. Log every step to `C:\ProgramData\LogiSwitch\spike.log`. Undivert on stop.

**Tests:**
- Manual by user: install, reboot, switch while logged in, switch from lock screen and login screen; send back `spike.log`.

**Gotchas:**
- Throwaway: no tests, no config. Stop and ask the user for the result. If HID++ I/O fails in the service, revise Task 10 to install a logon scheduled task instead (spec Risks, Fallback) before continuing.

### Task 6: Config

**Files:**
- Create: `internal/config/config.go`
- Test: `internal/config/config_test.go`

**Interfaces:**
- Produces:

```go
type Config struct {
    MouseSlotFor [3]int        // 0-based, index = keyboard slot (0-based)
    PollInterval time.Duration
    LogLevel     slog.Level
}
func Default() Config            // identity, 2s, Info
func Load(path string) (Config, error)
func DefaultPath() string        // %ProgramData%\LogiSwitch\config.json
```

**Behavior:**
- Missing file → `Default()`, nil. Partial file → missing keys default.
- Slot keys/values outside 1–3 → error naming the key. `pollIntervalMs < 200` → error. `logLevel` not debug|info|warn|error → error. Unknown JSON fields → error.

**Tests:**
- `TestLoadMissingFile`, `TestLoadPartial`, `TestLoadMappingConvertsToZeroBased` (`"2":3` → `MouseSlotFor[1]==2`), `TestLoadRejectsBadSlot`, `TestLoadRejectsShortPoll`, `TestLoadRejectsUnknownField`.

### Task 7: Rotating log file

**Files:**
- Create: `internal/logging/rotate.go`
- Test: `internal/logging/rotate_test.go`

**Interfaces:**
- Produces: `func OpenRotating(path string, maxBytes int64) (io.WriteCloser, error)`, `func DefaultPath() string` (`%ProgramData%\LogiSwitch\logiswitch.log`).

**Behavior:**
- Creates parent dir. Appends. A write that would exceed `maxBytes` renames the file to `path+".1"` (replacing it) and starts a new one. Safe for concurrent writes.

**Tests:**
- `TestRotateKeepsOneOld`: 3 writes over the cap leave `path` and `path.1` only, `path` holding the last write.
- `TestAppendsAcrossOpen`.

### Task 8: Switcher — discovery and divert upkeep

**Files:**
- Create: `internal/switcher/switcher.go`
- Test: `internal/switcher/switcher_test.go`

**Interfaces:**
- Consumes: `hid.Opener`, `hidpp` features, `config.Config`.
- Produces:

```go
type Clock interface{ Now() time.Time }
func New(o hid.Opener, cfg config.Config, clk Clock, log *slog.Logger) *Switcher
func (s *Switcher) Tick()                       // one upkeep pass
func (s *Switcher) Run(ctx context.Context)     // Tick every PollInterval + press events; Release on ctx done
func (s *Switcher) Release()                    // undivert D1–D3 on open keyboards, close all
```

**Behavior:**
- Tick: enumerate VID `0x046D`, usage `0xFF43/0x0202` or `0xFF00/0x0002`; open unseen paths, classify per spec Device discovery.
- A device that answers but lacks features → ignored path for the process lifetime; a timeout → retried next Tick.
- Keyboard not diverted (`Diverted(D1)` false) → divert D1–D3, cache `HostInfo` current, log "keys taken over".
- Any I/O error → close, forget, rediscover next Tick.

**Tests:**
- `TestTickDivertsKeyboard`, `TestTickRedivertsAfterReconnect`, `TestTickIgnoresNonTarget`, `TestTickRetriesTimedOutDevice`, `TestReleaseUndiverts`.

**Gotchas:**
- Tests need a fake `hid.Opener` over `hidpptest.Fake` devices; put it in `switcher_test.go`.
- `Run` owns all state on one goroutine; `Conn` notification readers forward into one channel.

### Task 9: Switcher — press handling and pending mouse switch

**Files:**
- Create: `internal/switcher/switch.go`
- Test: `internal/switcher/switch_test.go`

**Interfaces:**
- Consumes: Task 8 registry, `Conn.SetHost`, `DivertedCIDs`.

**Behavior:**
- Press CID `0xD1+k` with `k ==` cached keyboard host → no sends, log at debug.
- Otherwise: set pending `{slot: MouseSlotFor[k], expires: now+2m}`, start mouse `SetHost` sends in goroutines (timeout 200 ms), send keyboard `SetHost(k)` immediately, log `switch to slot k+1`.
- Mouse send OK → pending marked sent at `t`. Next Tick ≥ 1 s later: mouse answers `HostInfo` → resend; doesn't → clear.
- Mouse send failed / no mouse → each Tick tries again; expiry or new press replaces/clears it. Pending survives the keyboard leaving.
- Release events (empty CID list) ignored.

**Tests:**
- `TestPressSwitchesMouseThenKeyboard`, `TestPressOwnSlotNoop`, `TestPressUsesMapping`, `TestSlowMouseDoesNotDelayKeyboard` (mouse write delay 1 s, keyboard `SetHostCalls` within 50 ms), `TestPendingDeliveredOnMouseReconnect`, `TestPendingExpires`, `TestPendingSupersededByNewPress`, `TestNoMouseKeyboardStillSwitches`.

**Gotchas:**
- Drive time with a fake `Clock`; call `Tick` directly rather than `Run`.

### Task 10: CLI, service, install

**Files:**
- Create: `internal/switcher/status.go`, `cmd/logiswitch/main_windows.go`, `service_windows.go`, `install_windows.go`, `main_other.go`
- Test: `internal/switcher/status_test.go`

**Interfaces:**
- Produces: `func (s *Switcher) Status() []DeviceStatus` (`DeviceStatus{Product, Path, Kind string; ChangeHostIdx, ReprogIdx byte; CurrentHost int; Diverted bool}`), `func (s *Switcher) SetAll(slot int) error`.

**Behavior:**
- Commands per spec Command-line interface; unknown command or `set` outside 1–3 → usage, exit 2.
- `svc.IsWindowsService()` true → run service: log to rotating file + event log source `LogiSwitch` (start, stop, errors); Stop/Shutdown cancel ctx → `Release()`.
- `run`: slog to stdout, Ctrl+C → `Release()`.
- `install`: service `LogiSwitch`, `StartAutomatic`, LocalSystem, exe absolute path, recovery restart after 5 s; `eventlog.InstallAsEventCreate`. Not admin → message "run as administrator", exit 1.
- `uninstall`: stop (wait ≤ 10 s), delete service and event source.

**Tests:**
- `TestStatusReportsDiverted`, `TestSetAllSendsMouseThenKeyboard`.

### Task 11: README and spike removal

**Files:**
- Create: `README.md`
- Delete: `cmd/spike/`

**Behavior:**
- README covers build command, copying to `C:\Program Files\LogiSwitch\`, `install` on all 3 PCs, `list`/`run` for testing, `config.json` example, pairing (`logiswitch stop`), crash recovery (keyboard off/on), uninstalling Logi Options+, mouse→keyboard direction unsupported.

**Tests:**
- Manual hardware checklist from spec Testing, run by user.

## Coverage

Spec sections: Constraints, Device discovery, Keeping the keys diverted, Switching, Pending mouse switch, Stop / shutdown, Pairing a new computer, Command-line interface, Configuration, Logging, Testing, Risks (spike and fallback).
