# LogiSwitch — design

Date: 2026-11-18

## Intent

The user has an original Logitech MX Keys keyboard and an MX Master 3S mouse.
Logitech's "smart switch" (mouse follows the keyboard's Easy-Switch channel) is
not supported for this pair. The user wants a small Go utility, running in the
background as a Windows service, so that pressing Easy-Switch N on the keyboard
moves **both** keyboard and mouse to channel N.

## Environment (facts, verified on the user's hardware)

- 3 computers, all Windows 11. One computer per Easy-Switch slot (1, 2, 3).
- Both devices connect directly over **Bluetooth LE** (no Bolt/Unifying receiver).
- Each device exposes a HID++ 2.0 vendor collection: usage page `0xFF43`,
  usage `0x0202`. Device index `0xFF`. Long reports, report ID `0x11`, 20 bytes.
- MX Keys (`product_string` "MX_Keys"):
  - CHANGE_HOST `0x1814` at feature index 9.
  - REPROG_CONTROLS_V4 `0x1B04` at feature index 8, version 4.
  - Easy-Switch keys are controls with CIDs `0x00D1`, `0x00D2`, `0x00D3`,
    flags `hotkey, TEMP_DIVERTABLE, PERSIST_DIVERTABLE`.
- MX Master 3S ("MX_Master_3S"): CHANGE_HOST `0x1814` at feature index 10.
  Its own Easy-Switch button is **not** a divertable control.
- Feature indexes must be discovered at runtime (root feature `0x0000`,
  getFeature), not hard-coded — they differ per device/firmware.
- Proven with `probe_easyswitch.py` (in repo root): temporarily diverting
  CIDs D1–D3 via `setCidReporting` (fn 3, flags `0x03`) stops the keyboard from
  switching and instead produces `divertedButtonsEvent` notifications
  (fn 0, swid 0) listing pressed CIDs, then an all-zero "released" event.
  Undivert = flags `0x02`.
- Switching is proven with the user's `logi_switch.py`: `setHost` = CHANGE_HOST
  fn 1, param slot (0-based), no reply expected.

## Constraints

- Pure Go, no cgo; one static `logiswitch.exe` built from Linux/WSL with `GOOS=windows GOARCH=amd64`. Only dependency beyond stdlib: `golang.org/x/sys`.
- HID++ feature indexes are discovered at runtime via root getFeature, never hard-coded.
- Devices are identified by capability (features/CIDs), never by product name.
- Keyboard `setHost` is never delayed by mouse I/O (mouse writes ≤ ~200 ms timeout, concurrent).
- Service stop, shutdown, uninstall and `run` Ctrl+C undivert D1–D3 on every open keyboard.
- Slots are 1-based in config, CLI and logs; 0-based on the wire.
- Protocol and switching logic build and test on Linux; only `internal/hid` and service wiring are Windows-specific.

## Goals

1. Pressing Easy-Switch N on the keyboard, on whichever computer currently owns
   it, sends the mouse to slot N and the keyboard to slot N.
2. Runs as a Windows service: starts at boot, works before login and on the
   lock screen.
3. Self-healing: diversion is lost whenever the keyboard reconnects
   (switching back, sleep/wake, power cycle, PC resume). The service must
   re-apply it within ~2 s of the keyboard becoming reachable.
4. Single static `.exe`, pure Go (no cgo), cross-compiled from WSL with
   `GOOS=windows GOARCH=amd64`.
5. If the mouse is not on this computer, the keyboard still switches.

## Non-goals

- Mouse → keyboard direction (mouse's switch button can't be intercepted).
- macOS / Linux hosts.
- Bolt / Unifying receivers (keep the transport layer open to it, don't build it).
- GUI or tray icon.
- Any networking between computers.
- Coexistence guarantees with Logi Options+ (see Risks).

## Anti-goals (failures even if it "works")

- Leaving the Easy-Switch keys dead after the service stops cleanly. Stop,
  uninstall and OS shutdown must undivert.
- Needing Logitech software, Python, or extra DLLs on the computers.
- Noticeable delay: switch must be sent immediately on key press.

## Approaches considered

Detection of "the keyboard switched":

1. **Divert the Easy-Switch keys via HID++ 0x1B04 (chosen).** The computer that
   owns the keyboard learns the exact target slot before anything moves. Local
   only, no network. Proven to work on this MX Keys.
2. Network coordination: the computer the keyboard arrives at broadcasts, the
   mouse owner forwards the mouse. Works with any keyboard but needs every
   computer reachable over LAN and a protocol. Unnecessary given (1).
3. On keyboard disconnect, send mouse to "the other" computer. Only works with
   two computers and misfires on keyboard sleep. Ruled out (3 computers).

How it runs (user chose **Windows service**, over a per-user logon app which
would have been simpler but not work at the login/lock screen).

HID access (user chose **pure Go** via `golang.org/x/sys/windows` + direct
`hid.dll` / `setupapi.dll` calls, over cgo `hidapi` bindings which would
complicate cross-compiling from WSL).

## Behaviour

### Device discovery

- Enumerate HID interfaces (SetupDi with `HidD_GetHidGuid`), open each with
  `CreateFile` (shared read/write, overlapped), filter by
  `HidD_GetAttributes` VID `0x046D`, and by `HidP_GetCaps` usage page/usage
  `0xFF43/0x0202` (also accept `0xFF00/0x0002` for wired long-report
  collections; harmless).
- For each candidate, query root `getFeature` for `0x1814` and `0x1B04`.
- **Keyboard** = has 0x1814 and 0x1B04 with CIDs D1–D3 temp-divertable.
  **Mouse** = has 0x1814 and is not a keyboard. Identify by capability, not by
  product name. If multiple of either exist, act on all (simple loop).

### Keeping the keys diverted

- Poll loop every 2 s (builder may tune): for each keyboard, ensure it is open
  and diverted. Use `getCidReporting` (fn 2) on D1 to check; if not diverted,
  `setCidReporting` divert on D1–D3 and log "keys taken over".
- If I/O on a handle fails, close it and rediscover on the next tick. Mouse
  handles are rediscovered the same way.
- My call: polling rather than `RegisterDeviceNotification`, because BLE HID
  collections on Windows can stay enumerated while the device is disconnected,
  so device-arrival events alone don't reliably signal "reconnected". Polling
  with an actual HID++ query is the ground truth. Builder may add device
  notifications on top to react faster.

### Switching

- One reader goroutine per open handle. Responses are matched by
  (feature index, function|swid); notifications (swid 0) are dispatched.
- On a `divertedButtonsEvent` containing D1/D2/D3 (press, not release):
  1. keyboard slot `k = cid - 0xD1` (0-based).
  2. If `k` equals the keyboard's current host, do nothing (user pressed this
     computer's own key). Current host comes from `getHostInfo` (CHANGE_HOST
     fn 0), read when the keyboard is (re)diverted and cached — not queried
     on the key-press path.
  3. Mouse slot `m = mapping[k]` (default identity). Record a **pending mouse
     switch** `{slot: m, expires: now + 2 min}` (replacing any earlier one) and
     attempt `setHost(m)` on each reachable mouse.
  4. Send keyboard `setHost(k)` without waiting for the mouse writes: mouse
     sends run concurrently / with a short write timeout (≤ ~200 ms) so an
     unreachable mouse never delays the keyboard.
  5. Log the switch.

### Pending mouse switch (asleep mouse)

The MX Master 3S drops its BLE link when idle, so at key-press time the mouse
may be unreachable even though it is still paired to this computer.

- A pending mouse switch survives the keyboard leaving. While it is pending,
  the poll loop keeps trying the mouse; as soon as a mouse answers on this
  computer, send `setHost(m)` and clear the pending switch.
- It is cleared when: sent successfully, it expires (2 min), or a new
  Easy-Switch press on this computer replaces it.
- When a mouse write succeeds at key-press time, clear it immediately. Since
  `setHost` has no reply, "succeeds" means the write completed without error;
  if the mouse is still enumerated and answers a ping on the next tick, treat
  it as not switched and resend (still within expiry).
- User decision (2026-11-18): chosen over "log and let the user press the
  mouse button".

### Stop / shutdown

- On service Stop, Shutdown, and on `run` mode Ctrl+C: undivert D1–D3 on every
  open keyboard, close handles, exit.
- If the process crashes, the keys stay diverted until the keyboard reconnects
  or is power-cycled (turn it off and on). Document this.

### Pairing a new computer

Long-pressing Easy-Switch to enter pairing mode is also intercepted while
diverted. My call: document "run `logiswitch stop` (or turn the keyboard off
and on and long-press before the service re-takes the keys) to pair". Not
worth more machinery for a rare action.

## Command-line interface

`logiswitch.exe <command>`:

| Command | Effect |
|---|---|
| `install` | Register service "LogiSwitch" (LocalSystem, automatic start) pointing at this exe's absolute path; create event log source. Requires admin. |
| `uninstall` | Stop if running, remove service and event source. |
| `start` / `stop` | Start / stop the service. |
| `run` | Run the same loop in the console with logs to stdout. For testing. |
| `list` | One-shot: print detected devices, feature indexes, current host, divert state. |
| `set N` | One-shot: send mouse(s) and keyboard to slot N (1–3). |

When launched by the Service Control Manager, it runs as a service
(`svc.IsWindowsService()`).

## Configuration

Optional `%ProgramData%\LogiSwitch\config.json`; absent file = defaults.

```json
{
  "mouseSlotForKeyboardSlot": { "1": 1, "2": 2, "3": 3 },
  "pollIntervalMs": 2000,
  "logLevel": "info"
}
```

Read at service start (restart to apply). Slots are 1-based in config and CLI.

## Logging

- `%ProgramData%\LogiSwitch\logiswitch.log`, size-capped (e.g. 1 MB, keep one
  old file).
- Windows Event Log for start, stop, and errors only.
- `run` mode logs to stdout.

## Project layout (suggested; builder may adjust)

```
cmd/logiswitch/      main, CLI dispatch, service wiring (x/sys/windows/svc, svc/mgr, svc/eventlog)
internal/hid/        Windows HID enumeration + overlapped read/write (windows build tag)
internal/hidpp/      HID++ 2.0 message encode/decode, request/response matching, features
internal/switcher/   discovery, divert upkeep, event handling, switching logic
internal/config/     config load + defaults
```

`internal/hidpp` and `internal/switcher` talk to a small `Transport`
interface so they can be unit-tested with a fake device on Linux. Only
`internal/hid` and service wiring are Windows-only.

## Testing

- Unit tests (run in WSL): HID++ framing, request/response matching with
  interleaved notifications, divert upkeep state machine, switch sequence
  ordering, "pressed own slot" no-op, mapping, missing-mouse case, slow mouse
  write not delaying the keyboard, pending mouse switch (delivered on
  reconnect, expired, superseded) — against a fake transport and fake clock.
- Manual hardware tests (user, on Windows): `list`; `set N`; `run` and press
  each key; reconnect after switching back; sleep/wake; service install,
  reboot, switch at login screen; stop restores normal keys.

## Risks

- **Logi Options+** may also divert keys or hold the HID++ channel. If
  installed, it may conflict; recommend uninstalling it on these computers.
  `list` should report the divert state so conflicts are visible.
- **Session 0 / service HID access**: expected to work (LocalSystem can open
  vendor HID collections) but unverified. **First build step is a throwaway
  spike**: a minimal service that, on start, opens the keyboard and mouse,
  diverts D1–D3, and on a press sends both to that slot, logging to a file.
  Go/no-go: the user installs it, reboots, and a switch works (a) while logged
  in and (b) from the lock screen / login screen.
  **Fallback** (user decision, 2026-11-18): if the service cannot do HID++ I/O,
  ship the same binary as a per-user program started at logon (e.g. a
  scheduled task "at log on", `install` registering that instead). Everything
  else unchanged; it won't work at the login screen. If only (b) fails, keep
  the service anyway.
- **Output report length**: use `HidP_GetCaps` `OutputReportByteLength`
  (expected 20) rather than assuming.

## Left to the builder

Package and type names, exact poll interval, log format, retry/backoff on
repeated open failures, whether to add device-notification wakeups.
