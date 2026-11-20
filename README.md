# LogiSwitch

A pure-Go Windows service (runs as LocalSystem) that makes a Logitech MX Master 3S mouse follow an MX Keys keyboard's Easy-Switch channel over Bluetooth LE. Press Easy-Switch N on the keyboard and the mouse moves to host N as well.

## Requirements

- Windows 10/11 on every PC, with the keyboard and mouse paired over Bluetooth LE to each.
- Logi Options+ uninstalled (it may fight over the Easy-Switch keys).
- Go 1.25 to build (x/sys v0.47.0).

## Build

```
GOOS=windows GOARCH=amd64 go build -o dist/logiswitch.exe ./cmd/logiswitch
```

## Install

Do this on all 3 PCs. From an elevated prompt, wherever the exe is:

```
logiswitch.exe install
```

The exe copies itself to `C:\Program Files\LogiSwitch\logiswitch.exe` (a locked-down folder) and registers that copy as an auto-start service. Reinstalling over a stopped service upgrades it; stop a running service first (`logiswitch stop`). `uninstall` removes the service but leaves the copy and prints its path.

First-time setup if you used the spike: delete `C:\ProgramData\LogiSwitch` and, if still installed, run `sc delete LogiSwitchSpike` from an elevated prompt.

## Usage

| Command | Effect |
|---|---|
| `install` / `uninstall` | Register / remove the service (elevated) |
| `start` / `stop` | Start / stop the service |
| `run` | Run in the foreground (Ctrl+C to stop) |
| `list` | Show the keyboard and mouse found, with host slot |
| `set N` | Move both devices to slot N (1-3) |

`run`, `list` and `set` refuse while the service is running; stop it first.

## Configuration

Optional `C:\ProgramData\LogiSwitch\config.json`:

```json
{"mouseSlotForKeyboardSlot": {"1": 1, "2": 2, "3": 3}, "pollIntervalMs": 500, "logLevel": "info"}
```

The default poll interval is 500 ms. The file is honoured only if owned by SYSTEM or Administrators, so create it from an elevated prompt; otherwise it is removed with a warning.

The data dir is locked to SYSTEM/Administrators (Users read). Unexpected files in it are deleted at service start.

Log: `C:\ProgramData\LogiSwitch\logiswitch.log` (1 MiB, one `.1` kept). Errors, start and stop also go to the Windows Event Log (source LogiSwitch).

## Behaviour and limits

- Pressing Easy-Switch N moves the mouse, then the keyboard, to N. Pressing the slot you are already on is ignored.
- If the mouse is asleep, it gets the switch when it wakes on the old PC (up to 2 minutes; cancelled if you return to that PC first).
- Mouse to keyboard is unsupported: the mouse's Easy-Switch button cannot be intercepted.
- Works at the lock and login screens.

## Troubleshooting

- Pairing a new host: `logiswitch stop`, long-press Easy-Switch (works natively), then `logiswitch start`.
- Crash recovery (keys stuck non-native): turn the keyboard off and on.
- Keys misbehave: make sure Logi Options+ is uninstalled.
- Check `logiswitch.log` and the Event Log.

## Diagnostics

`probe_easyswitch.py` reports whether a keyboard's Easy-Switch keys are divertable.

## Hardware test checklist

1. Clean-up from the spike: elevated `sc delete LogiSwitchSpike` if it exists, then delete `C:\ProgramData\LogiSwitch`.
2. Leave `logiswitch.exe` in Downloads. In a non-elevated terminal, `logiswitch install` prints "error: access denied: run as administrator", exit 1.
3. As a standard user, recreate the dir and plant a file: `mkdir C:\ProgramData\LogiSwitch` then `echo x > C:\ProgramData\LogiSwitch\planted.txt`.
4. Elevated, from Downloads, `logiswitch install` prints "installed service LogiSwitch ... exe: C:\Program Files\LogiSwitch\logiswitch.exe"; `planted.txt` is gone.
5. `sc qc LogiSwitch`: BINARY_PATH_NAME is the quoted Program Files copy, START_TYPE AUTO_START, SERVICE_START_NAME LocalSystem.
6. `icacls C:\ProgramData\LogiSwitch` and `icacls "C:\Program Files\LogiSwitch"` each show only SYSTEM (OI)(CI)(F), Administrators (OI)(CI)(F) and Users (OI)(CI)(RX), no inherited entries. The exe shows only inherited entries of those three and its owner (`dir /q`) is BUILTIN\Administrators.
7. Before starting the service, from an elevated terminal, `logiswitch list` shows a keyboard row (HOST = slot, DIVERTED true while list runs) and a mouse row. A sleeping mouse may be missing.
8. `logiswitch set 2` moves both to slot 2; come back with Easy-Switch 1 or `set 1` from the other PC. With the mouse asleep, `set` prints "(no mouse found)". `logiswitch set 4` prints usage, exit 2.
9. `logiswitch run`: press Easy-Switch 2 and the log shows `mouse switched ... how=...`, then the keyboard moves. Come back and press the own slot: nothing switches (debug log only: "own slot pressed; ignoring"). Ctrl+C prints "keys released" and the keys are native again.
10. `logiswitch start` prints "started". Event Viewer, Application, source LogiSwitch: "LogiSwitch started". The log shows "service started" and `keys taken over`.
11. `logiswitch list` while the service runs prints "error: the LogiSwitch service is running ...", exit 1. `logiswitch install` now says "stop the service first".
12. Config trust: from an elevated Notepad save `C:\ProgramData\LogiSwitch\config.json` as `{"logLevel":"debug"}`, then `logiswitch stop` and `start`: debug lines appear. To test removal: `icacls C:\ProgramData\LogiSwitch\config.json /setowner %USERNAME%` as a non-Administrators owner, restart the service: the log shows `config.json not owned by SYSTEM/Administrators - removed, using defaults` and the file is gone.
13. Logged in: press each Easy-Switch key; the mouse follows each time.
14. Lock screen (Win+L): switch away and back; the mouse follows.
15. Reboot. At the login screen, before logging in, switch; the mouse follows.
16. Mouse asleep: let it sleep, switch with the keyboard, wake the mouse; it goes to the new slot (`mouse switched ... how=...`, delivered on reconnect).
17. Repeat steps 1-6, 10 and 13-16 on the other PCs.
18. `logiswitch stop` prints "stopped" within 10 s; event "LogiSwitch stopped"; the keys are native.
19. (Optional) End logiswitch.exe in Task Manager; the service restarts after about 5 s.
20. `logiswitch uninstall` prints "uninstalled service LogiSwitch" plus the paths of the copy and data it leaves. `sc query LogiSwitch` reports it does not exist. Delete `C:\Program Files\LogiSwitch` manually if wanted.
