# LibreScoot Bluetooth Service

[![CC BY-NC-SA 4.0][cc-by-nc-sa-shield]][cc-by-nc-sa]

The LibreScoot Bluetooth Service acts as a communication bridge between an nRF52 microcontroller and a Redis-based backend system. It facilitates control and monitoring of vehicle components through BLE connectivity, including lock/unlock commands, battery status, and mileage synchronization.

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

## Fault Tracking

The service reports faults via the FaultSet API to the `ble:fault` Redis key:

| Code | Description |
|------|-------------|
| 1    | Serial port communication error |
| 2    | nRF52 initialization error |

## License

This work is licensed under a
[Creative Commons Attribution-NonCommercial-ShareAlike 4.0 International License][cc-by-nc-sa].

[![CC BY-NC-SA 4.0][cc-by-nc-sa-image]][cc-by-nc-sa]

[cc-by-nc-sa]: http://creativecommons.org/licenses/by-nc-sa/4.0/
[cc-by-nc-sa-image]: https://licensebuttons.net/l/by-nc-sa/4.0/88x31.png
[cc-by-nc-sa-shield]: https://img.shields.io/badge/License-CC%20BY--NC--SA%204.0-lightgrey.svg

## Contributing

Contributions are welcome! Please feel free to submit a Pull Request.

---

Made with ❤️ by the LibreScoot community
