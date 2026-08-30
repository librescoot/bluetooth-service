# Librescoot Bluetooth Service

Part of the [Librescoot](https://librescoot.org/) open-source platform.

The Bluetooth Service connects the vehicle's nRF52 controller to the Librescoot Redis/Valkey IPC bus. It exchanges controller state and commands over the nRF52 serial USOCK link, exposing that functionality to BLE clients through the controller firmware.
## Capabilities

- Synchronizes vehicle, battery, mileage, firmware, navigation, USB, and power-management state between Redis/Valkey and the nRF52.
- Handles controller-originated BLE actions, including pairing, lock and vehicle state, keycard, navigation, alarm, time, USB, settings, power-management, and LTC charger requests.
- Consumes commands from the `scooter:ble` queue for advertising, bonding, LTC control, data-stream synchronization, and firmware-update requests.
- Performs nRF52 firmware discovery and optional startup/forced updates from its firmware directory.
- Receives BLE OTA bundles, stages them, and hands MDB or DBC artifacts to the update path.
- Reports serial and nRF initialization faults through Redis/Valkey.

## Operation and interfaces

The service starts a serial USOCK connection and Redis/Valkey hash watchers. It subscribes to `vehicle`, `battery:0`, `battery:1`, `power-manager`, `engine-ecu`, `system`, `ble`, `navigation`, `usb`, and `keycard`; initial state is synchronized when the watchers start.

It processes these Redis/Valkey list commands on `scooter:ble`:

- `advertising-start-with-whitelisting`, `advertising-restart-no-whitelisting`, and `advertising-stop`
- `delete-bond`, `delete-all-bonds`, and `remove`
- `ltc-enable`, `ltc-disable`, `ltc-force-enable`, `ltc-force-disable`, and `ltc-status`
- `data-stream-sync` and `firmware-update`

BLE extended-command requests are topic-prefixed strings. The implementation supports navigation, keycard, USB mode, time, settings, status, alarm, LTC, BLE bond removal, power-management, and capability queries. Generic settings reads and writes use the `settings:schema` value published by the settings service; writes reject unknown or read-only keys and validate the schema types that the service understands. Clients should use the `cap` query rather than hard-code the available extended-command set.

## Configuration

Configuration is supplied as command-line flags:

| Flag | Default | Purpose |
| --- | --- | --- |
| `--serial` | `/dev/ttymxc1` | nRF52 serial device |
| `--baud` | `115200` | Serial baud rate |
| `--redis-addr` | `localhost:6379` | Redis/Valkey address |
| `--log-level` | `3` | Log level: `0` none through `4` debug |
| `--firmware-dir` | service default | Directory containing nRF firmware files |
| `--auto-update` | `true` | Update nRF firmware at startup when a newer version is available |
| `--ota-staging-dir` | service default | Directory for incoming BLE OTA bundles |
| `--version` | — | Print the build version and exit |

The production recipe installs the firmware updater and bundled firmware under `/usr/share/nrf-fw/`; its systemd unit starts `/usr/bin/bluetooth-service`.

## Build and test

A Go toolchain is required. The Makefile builds static Linux ARMv7 binaries by default.

```sh
make build       # ARMv7 binary: bin/bluetooth-service
make build-host  # host binary: bin/bluetooth-service-host
make test
```

Additional maintenance targets are `make lint`, `make fmt`, `make deps`, and `make clean`.

## Deployment and runtime dependencies

The image recipe installs the executable at `/usr/bin/bluetooth-service` and the unit as `librescoot-bluetooth.service`. The unit runs as `root`, requires `valkey.service`, and restarts the process unconditionally.

Runtime operation requires:

- a reachable Redis or Valkey instance;
- the configured serial device and an nRF52 running the matching USOCK/BLE firmware;
- write access to the firmware and OTA staging locations when updates are enabled; and
- `timedatectl` when BLE time-setting requests are used.

For a packaged target, use the installed unit rather than a hand-written unit:

```sh
systemctl status librescoot-bluetooth.service
journalctl -u librescoot-bluetooth.service
```

## Operational and security notes

- The service is a privileged bridge between wireless commands and vehicle services. Restrict serial-device access and protect the Redis/Valkey IPC endpoint from untrusted clients.
- A BLE time request invokes `timedatectl set-time`; only authorize BLE peers that may change the system clock.
- Firmware update and OTA staging paths contain executable update inputs. Keep them service-owned and monitor update failures in the journal and Redis/Valkey fault state.
- Send `SIGTERM` or `SIGINT` for a coordinated shutdown; the service stops the controller link before exiting.

## License

This project is licensed under the [Creative Commons Attribution-NonCommercial-ShareAlike 4.0 International License](LICENSE).

Made with ❤️ by the Librescoot community
