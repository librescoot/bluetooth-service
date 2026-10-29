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

BLE extended-command requests are topic-prefixed strings. The implementation supports navigation, keycard, USB mode, time, settings, status, alarm, LTC, BLE bond removal, power-management, DBC power, and capability queries. Generic settings reads and writes use the `settings:schema` value published by the settings service; writes reject unknown or read-only keys and validate the schema types that the service understands. Clients should use the `cap` query rather than hard-code the available extended-command set. The full `cap:ext:...` registry is also published to the local Redis `system` hash as `capabilities` at startup and refreshed on `cap:ext` queries. `nav=2` advertises multi-hop navigation; a missing registry or a different nav version must not enable route controls.

`cap:ext` reports `keycard=2` for physical-card commands, phone management,
master-card listing and credential names. Bluetooth-service forwards these
`keycard:*` commands to the bundled keycard-service.
`cap:keycard` likewise reports only the commands currently available, while
legacy `cap:list` retains the unversioned `keycard` category. Clients must
keep an alias set command within the 100-byte extended-command limit: names
are capped at 32 UTF-8 bytes and base64url-encoded without padding.

`cap:ext` reports `sms=1` when the modem-service inbox projection is available.
`sms:start` freezes an inbox snapshot and replies `s:s:<token>:<count>:<unread>`.
For each zero-based index, send `sms:get:<token>:<index>:<offset>:<budget>`;
offset is a base-36 byte offset and budget is the notification payload limit,
from 20 to 244 bytes and no greater than ATT MTU minus three. A response
`s:p:<token>:<offset>:<base64url>` carries unpadded encoded bytes; append them
until `s:e:<token>:<offset>`, then decode the complete UTF-8 JSON message.
An invalid or expired request returns `s:x:<reason>`. Sessions expire after
five minutes of inactivity and are cleared on Bluetooth disconnect.
`sms:read:<id>` and `sms:delete:<id>` queue the corresponding inbox action and
reply `s:queued`; clients should refresh the inbox to observe the result.
All bonded phones with extended-command access can use this interface.

### Read-only hash queries

`cap:ext` reports `hash=1` for generic, allowlisted multi-field hash reads.
`cap:hash` lists the request forms. All bonded phones with extended-command
access can use this interface. The exact allowlist is `battery:0` and `battery:1`,
with fields `present` and `serial-number` on each. Every other hash or field is
denied. There are no writes, hash-wide reads, key discovery, or client-supplied
Redis commands. Values and request payloads are not logged. Transport debug logs
retain frame metadata but omit payload and full-frame dumps for the shared
extended-command frame ID.

Requests are at most 100 UTF-8 bytes, including `hash:`:

```text
hash:read:<id>:<budget>:<hash>/<field>[,<field>]
hash:get:<id>:<offset>:<budget>
```

- `id` is exactly eight lowercase hexadecimal digits, excluding `00000000`.
  Allocate a fresh ID for every logical read; never reuse an ID within a
  connection, including after errors, completion, or expiry. On reconnect,
  discard pending requests and queued notifications before starting new reads.
- `budget` is a canonical decimal notification payload limit, 20..244 bytes,
  no greater than negotiated ATT MTU minus three. The host has no MTU signal,
  so the client supplies this limit on every request. All hash responses fit
  it, including at ATT MTU 23. Malformed requests receive errors of at most
  20 bytes, even when their budget is invalid.
- Request one or two distinct fields, in the desired result order. Hashes and
  fields are exact, case-sensitive names, without escaping or normalization.
- `offset` and response lengths are canonical lowercase base-36 byte counts
  (no sign or leading zeros except `0`). Offsets are limited to 4096.

`read` freezes the selected fields atomically in one bounded, read-only Redis
script: length checks followed by `HMGET`. It returns `r:s:<id>:<length>`.
The frozen payload is a UTF-8 JSON array in requested-field order. Each element
is the unchanged Redis string, or `null` for a missing field. An absent hash
returns nulls; an empty string remains `""`. Values are limited to 256 UTF-8 bytes
per field and encoded JSON to 4096 bytes. Invalid UTF-8 and oversized values
fail the whole read. Presence, serial normalization, and handling cached values
are client concerns; the service does not interpret them.

`get` pulls bytes from that snapshot, using the same pull/base64url convention
as SMS:

```text
r:p:<id>:<offset>:<base64url-without-padding>
r:e:<id>:<offset>
r:x:<id>:<code>
```

A data packet contains as many bytes as fit the budget. Base64 represents bytes,
so UTF-8 characters may span packets. Match the active connection, ID, and
requested offset before accepting a packet. Append decoded bytes, enforce the
advertised length, and decode UTF-8/JSON only after the complete payload.
Allow only one outstanding operation per ID. A repeated `get` before completion
returns the same bytes for the same budget. To complete, request the exact
advertised length; the service releases the snapshot when producing `r:e`.
Further requests for a completed or expired ID return `session`, including an
EOF retry after a lost notification. Start a fresh read with a new ID to recover.

At most four snapshots, including pending reads, exist globally. A fifth read
returns `busy`; active snapshots are never evicted. Each expires five minutes
after creation, with no refresh on reads. Disconnect clears all snapshots and
invalidates pending Redis reads and queued responses. An active duplicate ID
returns `id` without changing the snapshot. Clients must respect ID uniqueness;
the service retains no unbounded history of completed IDs.

At most four hash handlers may execute concurrently, including Redis reads and
waiting for the shared paced notification sender. Excess requests are silently
dropped, rather than queued for a `busy` response. Clients need a response timeout
and a manual retry path. Retry an entire read with a fresh ID; a timed-out `get`
may be retried at the same offset while its snapshot remains active. Disconnect
does not reset handler admission while earlier handlers are still running.

At MTU 23, two maximum-length control-character values encode to 3079 JSON bytes
and require 1324 data packets. Including start and EOF, the 100 ms notification
spacing alone takes 132.5 seconds. The five-minute snapshot lifetime accommodates
this; a client with a shorter overall deadline may time out on such values.

Error codes: `command` (malformed request), `denied` (hash or field not allowed),
`redis` (backend unavailable or wrong hash type), `large` (size overflow),
`utf8` (invalid value encoding), `busy` (snapshot limit), `session` (missing,
pending, expired, or completed snapshot), `offset` (invalid/out-of-range byte
offset), `budget` (invalid notification budget), and `id` (active duplicate).
Errors echo a valid request ID; invalid/missing IDs use reserved `00000000`,
which clients must never associate with an active request.

Example with `battery:0[present] = "false"` and a missing `serial-number`, using
MTU 247 (budget 244):

```text
hash:read:1234abcd:244:battery:0/present,serial-number
r:s:1234abcd:e
hash:get:1234abcd:0:244
r:p:1234abcd:0:WyJmYWxzZSIsbnVsbF0
hash:get:1234abcd:e:244
r:e:1234abcd:e
```

The decoded payload is `["false",null]`. MTU-23, Unicode, escaped-value, and
missing/empty golden exchanges are in
`pkg/service/testdata/hash_contract_vectors.json`.

### Alarm and power commands

`cap:ext` reports `alarm=2` for the bundled alarm-service semantics: `alarm:stop`
ends the episode with a five-minute cooldown; `alarm:disarm` holds disarmed until
the next park cycle, hibernation starts, or eight hours pass. Neither changes
`alarm.enabled`; `alarm:disable` switches it off persistently. Legacy `cap:list`
retains the unversioned `alarm` category.

Explicit BLE hibernation forwards `hibernate-manual` to PM regardless of vehicle state; extended `pm:hibernate-for <duration>` forwards `hibernate-for:<seconds>`. PM owns parked-state admission and graceful vehicle preparation. Controller-originated automatic hibernation forwards `hibernate-auto` and does not initiate vehicle locking.

Wake-timer acknowledgements publish the nRF's duration echo as `power-manager[wake-timer-ack-seconds]`, alongside `wake-timer-armed` for telemetry. PM requires the echoed duration to match its request; deploy the corresponding PM and vehicle-service preparation handlers together.

DBC power commands do not unlock the vehicle or change its state:

- `dbc:status` replies with `dbc:status:power:<on|off|unknown>:ready:<true|false|unknown>`.
- `dbc:on` and `dbc:off` queue the power change and acknowledge it immediately.
- `dbc:on-wait` replies once dashboard power is on and the DBC has published `ready=true`.
- `dbc:off-wait` replies once vehicle-service reports dashboard power off.

Wait commands time out after 60 seconds. DBC power-off is rejected while a DBC update is active; no force-off command is exposed over BLE.

## File transfer

`cap:ext` reports `files=1` when the connected controller supports the binary
file tunnel. The separate file service supports listing, SHA-256 metadata,
uploads and downloads with bounded windows, cancellation and chunk-aligned
resume. Its USOCK frames are `0xB3` (data), `0xB4` (control) and `0xB5` (status).

Named stores confine access: `logs` exposes completed `.tar.gz` archives in
`/data/log-bundles` read-only; `inbox` stages uploads in `/data/ble-files/inbox`.
Uploads are verified before non-overwriting publication. Transfers never install
firmware or activate maps. Log archives contain sensitive diagnostic data.

Legacy read-only Logs listing and downloads remain available whenever the BLE
file tunnel is supported; they do not depend on the `/data` opt-in. The
administrative `/data` browser uses protocol v2 and is disabled by default.
Enable it only for a root-administered development session by adding a local
systemd override:

```sh
sudo systemctl edit librescoot-bluetooth.service
```

Set the override to:

```ini
[Service]
ExecStart=
ExecStart=/usr/bin/bluetooth-service --serial=/dev/ttymxc1 --baud=115200 --redis-addr=localhost:6379 --log-level=3 --firmware-dir=/usr/share/nrf-fw --auto-update=true --ota-staging-dir=/data/ota --enable-data-browser
```

Then restart `librescoot-bluetooth.service`; remove the override to disable the
browser. The service advertises `data=1` only while this option is enabled and
the nRF file tunnel is available. This grants paired BLE clients administrative
read/write access across `/data`; uploaded files may affect other services that
consume them, but are never automatically installed or executed. File replacement
requires explicit client intent and a match against the target's expected size
and SHA-256. Publication is atomic after complete-file verification. A separate,
uncooperative process can still race the final target check and atomic rename; the
service does not claim a portable filesystem compare-and-swap guarantee. Non-UTF-8
directory names and special files are not transferable; unsupported entries may be
omitted or marked unsupported. The `.ble-transfer` name prefix is reserved for
private transfer staging and is not addressable through the browser; legacy
`.partial` staging names are reserved as well. The kernel's aggregate path-length
limit still applies; an overlong path returns the protocol's invalid-request error
without falling back to a host path.

Active transfers hold the `ble-files` block power inhibitor. Cancellation,
completion and BLE disconnect release it. File transfers and firmware OTA
transfers are mutually exclusive; the OTA wire protocol remains separate.

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
| `--ota-staging-dir` | `/data/ota` | Directory for incoming BLE OTA bundles |
| `--enable-data-browser` | `false` | Enable administrative BLE browser rooted at `/data` |
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
- `timedatectl` when BLE time-setting requests are used; and
- `chronyc` for NTP trust detection used by nRF UTC holdover seeding.

For a packaged target, use the installed unit rather than a hand-written unit:

```sh
systemctl status librescoot-bluetooth.service
journalctl -u librescoot-bluetooth.service
```

## Operational and security notes

- The service is a privileged bridge between wireless commands and vehicle services. Restrict serial-device access and protect the Redis/Valkey IPC endpoint from untrusted clients.
- A BLE time request invokes `timedatectl set-time`; only authorize BLE peers that may change the system clock. The nRF UTC holdover is seeded from accepted Bluetooth time, a GPS-confirmed clock update, or an external NTP reference; it never makes Linux time move backward.
- Firmware update and OTA staging paths contain executable update inputs. Keep them service-owned and monitor update failures in the journal and Redis/Valkey fault state.
- Send `SIGTERM` or `SIGINT` for a coordinated shutdown; the service stops the controller link before exiting.

### Navigation plan ownership

BLE `nav:dest` and saved-location navigate replace the plan with one stop;
`nav:route:add` appends atomically, `remove` sends the revision from `plan.get`,
`skip` advances the current stop with its plan and stop IDs, leaving it unreached,
and `nav:clear`/`nav:route:clear` clear the plan. Legacy `navi:start` also
replaces the plan. These operations call settings-service via redis-ipc on
`settings:route-plan` with a five-second timeout. The service does not write
`navigation` fields: settings-service persists the plan and publishes its
projection. A missing owner or rejected mutation is an error, never a direct
Redis fallback. BLE response strings and zero-based list / one-based remove
indices remain unchanged. The complete RPC wire contract is documented in
settings-service's README.


## License

This project is licensed under the [Creative Commons Attribution-NonCommercial-ShareAlike 4.0 International License](LICENSE).

Made with ❤️ by the Librescoot community
