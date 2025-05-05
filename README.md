# MDB Bluetooth Service

[![CC BY-NC-SA 4.0][cc-by-nc-sa-shield]][cc-by-nc-sa]

The MDB Bluetooth Service acts as a communication bridge, interfacing an nRF52 with a Redis-based backend system. It facilitates the control and monitoring of vehicle components through this interface.

## Features

- Serial communication using a custom USOCK protocol
- Redis-based state management and command handling
- Interface for vehicle systems (battery, locks, mileage, firmware info)
- Handles messages between the serial device and Redis
- Monitors Redis for commands to send to the serial device
- Initialization sequence for the connected device (e.g., nRF52)
- Graceful shutdown on signal interrupts

## System Architecture

The service operates around a central `service` package that manages:
- Connection to the serial device via the `usock` package
- Connection to the Redis server via the `redis` package
- Handling incoming messages from the serial device
- Watching for outgoing commands from Redis
- Translating and forwarding messages/commands between the serial interface and Redis
- Managing vehicle state updates (battery status, locks, mileage, etc.)

### Key Components

- **Main Application (`cmd/mdb-bluetooth`)**: Initializes connections, sets up the service, and handles startup/shutdown.
- **Service (`pkg/service`)**: Core logic for message handling, Redis interaction, and state management.
- **USOCK (`pkg/usock`)**: Handles the custom serial communication protocol with the microcontroller.
- **Redis (`pkg/redis`)**: Manages the connection and interaction with the Redis instance.

## Building and Running

The project uses a Makefile for common tasks.

To build the service (for ARMv6 Linux):

```bash
make build
```

Other build targets exist (e.g., `make build-amd64`). The output binary will be placed in the `bin/` directory (e.g., `bin/mdb-bluetooth`).

To run the service (example):

```bash
./bin/mdb-bluetooth --serial /dev/ttymxc1 --redis-addr localhost:6379
```

Refer to the command-line flags for configuration options.

## Configuration

The service can be configured via command-line flags:

- `--serial`: Path to the serial device (default: `/dev/ttymxc1`)
- `--baud`: Baud rate for serial communication (default: `115200`)
- `--redis-addr`: Address of the Redis server (default: `localhost:6379`)
- `--redis-pass`: Password for the Redis server (default: `""`)
- `--redis-db`: Redis database number (default: `0`)

Redis keys used for state and commands are defined as constants within the `main` package.

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
