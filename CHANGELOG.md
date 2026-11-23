# Changelog

## 1.0.0 (2026-11-23)


### Features

* add fake HID++ device for tests ([d233a09](https://github.com/Galvill/LogiSwitch/commit/d233a096f43ed302a6913ef682ed0f070c403b31))
* add hid types and HID++ framing/connection ([a17e9b8](https://github.com/Galvill/LogiSwitch/commit/a17e9b8806b19510aa2a169273457cbb155312df))
* **cmd:** logiswitch CLI, Windows service and installer ([fe3be12](https://github.com/Galvill/LogiSwitch/commit/fe3be1208ba1611bb8406c3cfccf0ee1f822a76c))
* **cmd:** version command; log version at service start ([2d8a8e4](https://github.com/Galvill/LogiSwitch/commit/2d8a8e4a73c25edfbe207a40c408954703261f84))
* **config:** optional config.json loader with validation ([0fde54a](https://github.com/Galvill/LogiSwitch/commit/0fde54a60f0a524984f0dfad67e422a8ebe8a27a))
* **hid:** Windows HID transport (CfgMgr32 + hid.dll, overlapped I/O) ([eb4a4f0](https://github.com/Galvill/LogiSwitch/commit/eb4a4f01f8f64fe9212ea506939932c74aa320a1))
* **logging:** harden data dir and log files; truncate log by handle on failed rotation ([5a2c63d](https://github.com/Galvill/LogiSwitch/commit/5a2c63d5c09c0ca6d51a1e5a67b2d34a3dc68a42))
* **logging:** size-capped rotating log file ([c37dff0](https://github.com/Galvill/LogiSwitch/commit/c37dff071a1ee51f7642e3e8ebe3fdfc14764ad6))
* **switcher,service:** release keys on suspend; list shows pre-existing diversion ([4ad2a76](https://github.com/Galvill/LogiSwitch/commit/4ad2a76e1a113822e2708dc42cfa950c72ed40ff))
* **switcher:** device discovery, classification cache and divert upkeep ([a3fb198](https://github.com/Galvill/LogiSwitch/commit/a3fb198a814977f3e39c9d4bddf75c0c90aa50a3))
* **switcher:** press handling, pending mouse switch, off-owner device opens ([9d18486](https://github.com/Galvill/LogiSwitch/commit/9d184869eb3fa501a48c7bc94e9223375c1fde09))
* **switcher:** Status and SetAll; cancel pending on keyboard return; skip divert check on leaving keyboard ([1b26b97](https://github.com/Galvill/LogiSwitch/commit/1b26b97ccf2c304e8c5c5e61e7a591c85f9d5421))


### Bug Fixes

* **deps:** bump golang.org/x/sys from 0.47.0 to 0.50.0 ([a3cadb3](https://github.com/Galvill/LogiSwitch/commit/a3cadb3f490e1a82d1e4815387c26f2e94cd6eb8))
* **deps:** bump golang.org/x/sys from 0.47.0 to 0.50.0 ([053ae86](https://github.com/Galvill/LogiSwitch/commit/053ae86fbdcfa8da51732700c81d62c6dfed6774))
* **install,logging:** clear error when the installed exe is in use; plain hyphen in config warnings ([be8dc20](https://github.com/Galvill/LogiSwitch/commit/be8dc208e1fec40553ed8f6266f645a98c101618))
* **install,logging:** install exe under Program Files; prune and vet data dir; one path source ([e21a037](https://github.com/Galvill/LogiSwitch/commit/e21a037257679a2d151ee0b3f351f18eb129cbb8))
* **logging:** bound growth on rotation failure; replace non-regular log path; tighten perms ([013a777](https://github.com/Galvill/LogiSwitch/commit/013a777392920f296f5ce235e76520471fde3d0b))
* **logging:** recover from failed close, ErrClosed after Close, stronger tests ([16f3eb4](https://github.com/Galvill/LogiSwitch/commit/16f3eb4a0ce73a101141c911431a889d29a387e3))
* Send does not wait for a reply; mask Function in Encode ([9af3931](https://github.com/Galvill/LogiSwitch/commit/9af393109711185cc58bf5123f3e201539fde8cc))
* **switcher:** leaving grace, per-keyboard pending cancel, SetAll write error ([61a778b](https://github.com/Galvill/LogiSwitch/commit/61a778b0083652e6ea361886f9c26ac1aa63aace))
* **switcher:** re-press during leaving grace, undivert stops at first error, 5 s divert re-check, timed-out mouse write counts as sent, presence logs ([2924ae2](https://github.com/Galvill/LogiSwitch/commit/2924ae2f6ec0edbf817f63f31a320f5bbb9e1538))
