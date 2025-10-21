# Scooter File Transfer Client

A Python3 client for transferring files to the electric scooter via Bluetooth Low Energy (BLE) on macOS.

## Installation

1. Install Python 3.8 or later (if not already installed):
```bash
brew install python3
```

2. Install required packages:
```bash
pip3 install -r requirements.txt
```

## Usage

### Basic Usage

Send a file to the scooter:
```bash
python3 scooter_file_transfer.py path/to/file.bin
```

### Advanced Options

```bash
# Specify BLE address directly (skip scanning)
python3 scooter_file_transfer.py file.bin -a "AA:BB:CC:DD:EE:FF"

# Enable verbose output for debugging
python3 scooter_file_transfer.py file.bin -v

# Combine options
python3 scooter_file_transfer.py file.bin -a "AA:BB:CC:DD:EE:FF" -v
```

## Features

- **Auto-discovery**: Automatically scans for the scooter
- **Manual selection**: Choose from available BLE devices if scooter not found
- **Progress bar**: Shows transfer progress in real-time
- **Error handling**: Automatic cleanup on errors or interruption
- **CRC verification**: Ensures data integrity
- **Chunked transfer**: Splits large files into 1024-byte chunks

## File Limitations

- Maximum file size: 10 MB
- Supported file types: Any binary file
- Transfer speed: ~12-20 KB/s (limited by BLE)

## Example Session

```bash
$ python3 scooter_file_transfer.py firmware_update.bin

Scanning for scooter...
Found scooter: unu Scooter at AA:BB:CC:DD:EE:FF
Connecting to AA:BB:CC:DD:EE:FF...
Connected!
Sending file: firmware_update.bin (51200 bytes)
Initializing transfer...
Transfer initialized: firmware_update.bin_1234567890
Sending 50 chunks...
[████████████████████████████████████████] 100% (50/50)
Completing transfer...
Transfer completed successfully!
Disconnected
```

## Troubleshooting

### "No module named 'bleak'"
Install the requirements:
```bash
pip3 install -r requirements.txt
```

### "Scooter not found"
1. Ensure the scooter is powered on
2. Ensure Bluetooth is enabled on your Mac
3. Try moving closer to the scooter
4. Use manual device selection when prompted

### "Permission denied" on macOS
Grant Terminal/Python Bluetooth permissions:
1. System Preferences → Security & Privacy → Privacy
2. Select Bluetooth
3. Add Terminal or Python to allowed apps

### "Transfer timeout"
- Check the scooter is not already connected to another device
- Try reducing file size
- Move closer to the scooter for better signal

## Protocol Details

The client implements the custom file transfer protocol:

1. **Initialize**: Send filename, size, and CRC32
2. **Transfer**: Send file in 1024-byte chunks with sequence numbers
3. **Complete**: Verify CRC32 and finalize transfer

All communication uses BLE GATT characteristics with custom UUIDs based on the service UUID 0xF000.

## Testing

Test with a small file first:
```bash
echo "Hello Scooter" > test.txt
python3 scooter_file_transfer.py test.txt -v
```

## Requirements

- macOS 10.15 or later
- Python 3.8+
- Bluetooth 4.0+ adapter
- `bleak` library (BLE communication)