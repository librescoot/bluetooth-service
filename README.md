# Librescoot Bluetooth Service

The Librescoot Bluetooth Service acts as a communication bridge between an nRF52 microcontroller and a Redis-based backend system. It facilitates control and monitoring of vehicle components through BLE connectivity, including lock/unlock commands, battery status, and mileage synchronization.

Part of the [Librescoot](https://librescoot.org/) open-source platform.

## Features

- Serial communication with nRF52 using USOCK protocol over CBOR
- Redis-based state management via redis-ipc library
- BLE pairing management and device connectivity
- Bidirectional message forwarding between nRF52 and Redis
- Vehicle state synchronization (locks, battery, mileage)
- Fault tracking and reporting via FaultSet API
- Configurable logging levels
- Build-time version information embedded in binary
- Graceful shutdown on signal interrupts

## Dependencies

- `github.com/librescoot/redis-ipc` - Redis-based IPC library for service communication
- `github.com/tarm/serial` - Serial port communication
- `github.com/fxamacker/cbor/v2` - CBOR encoding/decoding for USOCK protocol

## System Architecture

The service operates around a central `service` package that manages:
- Connection to the nRF52 via the `usock` package
- Connection to the Redis server via the `redis-ipc` library
- Handling incoming messages from the serial device
- Watching for outgoing commands from Redis
- Translating and forwarding messages/commands between the serial interface and Redis
- Managing vehicle state updates (battery status, locks, mileage, etc.)

### Key Components

- **Main Application (`cmd/bluetooth-service`)**: Initializes connections, sets up the service, and handles startup/shutdown.
- **Service (`pkg/service`)**: Core logic for message handling, Redis interaction, and state management.
- **USOCK (`pkg/usock`)**: Handles the custom serial communication protocol with the nRF52 microcontroller.
- **BLE (`pkg/ble`)**: BLE-specific data structures and constants.
- **Logger (`pkg/logger`)**: Leveled logging with systemd/journald integration.

## Building and Running

To build the service:

```bash
make build
```

To build for the current host platform (development):

```bash
make build-host
```

The compiled binary will be available in the `bin` directory.

### Development

- `make build`: Build for ARM (default target, production)
- `make build-arm`: Alias for `build`
- `make build-amd64`: Build for AMD64
- `make build-host`: Build for current host platform
- `make dist`: Stripped ARM binary for distribution
- `make test`: Run tests
- `make lint`: Run golangci-lint
- `make fmt`: Format code
- `make deps`: Download and tidy dependencies
- `make clean`: Remove build artifacts

## Usage

Run the service with default settings:

```bash
./bin/bluetooth-service
```

### Command Line Options

- `--version`: Print version information and exit
- `--serial`: Serial device path (default: `/dev/ttymxc1`)
- `--baud`: Serial baud rate (default: `115200`)
- `--redis-addr`: Redis server address (default: `localhost:6379`)
- `--redis-pass`: Redis password (default: empty)
- `--redis-db`: Redis database number (default: `0`)
- `--log-level`: Log level (0=NONE, 1=ERROR, 2=WARN, 3=INFO, 4=DEBUG, default: 3)

## Logging

The service utilizes a leveled logging system with systemd/journald integration. When running under systemd, timestamps are omitted to avoid duplication with journald's timestamps.

Log levels:
- **0=NONE**: No logs
- **1=ERROR**: Only error messages
- **2=WARN**: Warning messages and errors
- **3=INFO**: Informational messages, warnings, and errors (default)
- **4=DEBUG**: Detailed debug messages and all above

## BLE Interface

### Power Requests

The nRF52's `REQUESTS_POWER` characteristic (write) accepts UTF-8 string commands:

| Command | Effect |
|---------|--------|
| `wakeup` | Exit hibernation mode |
| `hibernate` | Request hibernation via iMX6 pm-service |
| `reboot` | Soft reboot — forwarded to iMX6 pm-service |
| `hard-reboot` | Power-cycle all rails via nRF52 hard reboot FSM (cuts power, waits, restores) |

`hard-reboot` is only accepted during normal operation (stand-by, suspend, parked, ready-to-drive). It's rejected during active hibernation entry or if a hard reboot is already in progress.

### Extended Commands

The extended command characteristic (write, service 0x0400) accepts topic-prefixed string commands. Responses come back on the response characteristic (notify, same service).

Available command topics:

| Topic | Commands | Redis target |
|-------|----------|-------------|
| `nav` | `dest <lat>,<lon>[,<name>]`, `clear`, `fav:add`, `fav:delete`, `fav:navigate`, `fav:list` | `navigation` hash |
| `keycard` | `list`, `count`, `add`, `remove` | `scooter:keycard` queue |
| `usb` | `ums`, `normal` | `usb` hash |
| `time` | `set <unix_timestamp>` | timedatectl |
| `config` | `apn`, `hibernate-timer`, `update-channel`, `auto-standby-seconds` | `settings` hash |
| `status` | `maps-available`, `navigation-available` | various hashes |
| `alarm` | `enable`, `disable`, `arm`, `disarm`, `start`, `stop` | `scooter:alarm` queue |
| `cap` | `list`, `<topic>` | (local) |

Responses follow the format `<topic>:ok`, `<topic>:error:<reason>`, or `<topic>:<data>`.

## Fault Tracking

The service reports faults via the FaultSet API to the `ble:fault` Redis key:

| Code | Description |
|------|-------------|
| 1    | Serial port communication error |
| 2    | nRF52 initialization error |

## License

This project is dual-licensed. The source code is available under the
[Creative Commons Attribution-NonCommercial-ShareAlike 4.0 International License][cc-by-nc-sa].
The maintainers reserve the right to grant separate licenses for commercial distribution; please contact the maintainers to discuss commercial licensing.

[![CC BY-NC-SA 4.0][cc-by-nc-sa-image]][cc-by-nc-sa]

[cc-by-nc-sa]: http://creativecommons.org/licenses/by-nc-sa/4.0/
[cc-by-nc-sa-image]: https://licensebuttons.net/l/by-nc-sa/4.0/88x31.png

## Contributing

Contributions are welcome! Please feel free to submit a Pull Request.

---

Made with ❤️ by the Librescoot community
