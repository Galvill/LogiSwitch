"""
probe_easyswitch.py - check whether a Logitech device lets software take over
its Easy-Switch keys (HID++ 2.0 feature 0x1B04, REPROG_CONTROLS_V4).

Install:  pip install hidapi
Usage:    python probe_easyswitch.py          # read-only: list controls and their divert flags
          python probe_easyswitch.py --live   # also divert Easy-Switch keys temporarily and
                                              # print presses (Ctrl+C to stop and restore)

While --live runs, pressing an Easy-Switch key will NOT switch the keyboard;
it is reported here instead. On exit the keys are restored. A temporary divert
also clears on its own if the keyboard reconnects or power-cycles.
"""
import sys
import time
import hid

VID = 0x046D
RECEIVER_PIDS = {0xC548, 0xC52B}
FEATURE_REPROG_V4 = 0x1B04
FEATURE_CHANGE_HOST = 0x1814
SWID = 0x07
HOST_SWITCH_CIDS = {0x00D1: "Easy-Switch 1", 0x00D2: "Easy-Switch 2", 0x00D3: "Easy-Switch 3"}

FLAG_NAMES = [(0x01, "mouse"), (0x02, "fkey"), (0x04, "hotkey"), (0x08, "fnToggle"),
              (0x10, "reprogHint"), (0x20, "TEMP_DIVERTABLE"), (0x40, "PERSIST_DIVERTABLE"),
              (0x80, "virtual")]


def find_channels():
    seen = set()
    for d in hid.enumerate(VID, 0):
        up, u, path = d["usage_page"], d["usage"], d["path"]
        if path in seen:
            continue
        if up == 0xFF00 and u == 0x0002:
            seen.add(path)
            idxs = range(1, 7) if d["product_id"] in RECEIVER_PIDS else [0xFF]
            yield path, d, list(idxs)
        elif up == 0xFF43 and u == 0x0202:      # Bluetooth LE direct
            seen.add(path)
            yield path, d, [0xFF]


def send(h, idx, feat, func, params=b""):
    msg = bytearray(20)
    msg[0], msg[1], msg[2], msg[3] = 0x11, idx, feat, (func << 4) | SWID
    msg[4:4 + len(params)] = params
    h.write(bytes(msg))
    return msg[3]


def request(h, idx, feat, func, params=b"", timeout=0.8):
    sw = send(h, idx, feat, func, params)
    end = time.time() + timeout
    while time.time() < end:
        r = h.read(20, 100)
        if not r or r[1] != idx:
            continue
        if r[2] == feat and r[3] == sw:
            return r
        if r[2] in (0xFF, 0x8F):
            return None
    return None


def feature(h, idx, feature_id):
    """Return (index, version) or None."""
    r = request(h, idx, 0x00, 0, feature_id.to_bytes(2, "big"))
    if r and r[4]:
        return r[4], r[6]
    return None


def flags_str(f):
    return ",".join(n for bit, n in FLAG_NAMES if f & bit) or "-"


def probe_device(h, idx, name):
    """Print the device's controls. Return (feature index, [divertable Easy-Switch CIDs])."""
    print(f"\n=== {name} (idx={idx:#x}) ===")
    ch = feature(h, idx, FEATURE_CHANGE_HOST)
    print(f"CHANGE_HOST 0x1814: {'index %d' % ch[0] if ch else 'not supported'}")
    rp = feature(h, idx, FEATURE_REPROG_V4)
    if not rp:
        print("REPROG_CONTROLS_V4 0x1B04: not supported -> software cannot take over its keys")
        return None, []
    fi, ver = rp
    print(f"REPROG_CONTROLS_V4 0x1B04: index {fi}, version {ver}")
    r = request(h, idx, fi, 0)                      # getCount
    if not r:
        print("  getCount: no reply")
        return fi, []
    count = r[4]
    print(f"  {count} controls:")
    found, divertable = [], []
    for i in range(count):
        r = request(h, idx, fi, 1, bytes([i]))      # getCidInfo(index)
        if not r:
            print(f"  [{i}] no reply")
            continue
        cid = (r[4] << 8) | r[5]
        tid = (r[6] << 8) | r[7]
        flags = r[8]
        label = HOST_SWITCH_CIDS.get(cid, "")
        mark = "  <==" if label else ""
        print(f"  [{i:2}] cid={cid:#06x} tid={tid:#06x} flags={flags_str(flags)} {label}{mark}")
        if label:
            found.append(cid)
            if flags & 0x20:
                divertable.append(cid)
    print()
    if not found:
        print("  RESULT: Easy-Switch keys are not listed as controls.")
    elif divertable:
        print(f"  RESULT: Easy-Switch keys CAN be diverted: {[HOST_SWITCH_CIDS[c] for c in divertable]}")
    else:
        print("  RESULT: Easy-Switch keys are listed but NOT divertable.")
    return fi, divertable


def set_divert(h, idx, fi, cid, on):
    # setCidReporting: cid(2), flags: bit0 divert, bit1 divert-valid
    flags = 0x03 if on else 0x02
    return request(h, idx, fi, 3, cid.to_bytes(2, "big") + bytes([flags]))


def live_test(h, idx, fi, cids):
    print("\n--- LIVE TEST: press Easy-Switch keys on the keyboard (Ctrl+C to stop) ---")
    for c in cids:
        ok = set_divert(h, idx, fi, c, True)
        print(f"divert {HOST_SWITCH_CIDS[c]}: {'ok' if ok else 'no reply'}")
    try:
        while True:
            r = h.read(20, 200)
            if not r or r[1] != idx or r[2] != fi:
                continue
            if (r[3] >> 4) == 0:                    # divertedButtonsEvent
                pressed = [(r[4 + 2 * k] << 8) | r[5 + 2 * k] for k in range(4)]
                pressed = [p for p in pressed if p]
                names = [HOST_SWITCH_CIDS.get(p, hex(p)) for p in pressed]
                print(f"{time.strftime('%H:%M:%S')} pressed: {names or '(released)'}")
    except KeyboardInterrupt:
        pass
    finally:
        for c in cids:
            set_divert(h, idx, fi, c, False)
        print("restored Easy-Switch keys")


def main():
    live = "--live" in sys.argv[1:]
    any_dev = False
    for path, info, idxs in find_channels():
        try:
            h = hid.device()
            h.open_path(path)
        except OSError as e:
            print(f"cannot open {info['product_string']}: {e}")
            continue
        for idx in idxs:
            if not feature(h, idx, FEATURE_CHANGE_HOST) and not feature(h, idx, FEATURE_REPROG_V4):
                continue
            any_dev = True
            name = info["product_string"] or hex(info["product_id"])
            fi, divertable = probe_device(h, idx, name)
            if live and divertable:
                live_test(h, idx, fi, divertable)
        h.close()
    if not any_dev:
        print("no HID++ devices found (are they connected to this computer?)")


if __name__ == "__main__":
    main()
