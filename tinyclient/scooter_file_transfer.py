#!/usr/bin/env python3
"""
Bluetooth File Transfer Client for Electric Scooter
Transfers files from macOS to the scooter via BLE
"""

import asyncio
import argparse
import struct
import os
import sys
import time
import zlib
import base64
from pathlib import Path
from typing import Optional, List
from bleak import BleakClient, BleakScanner
from bleak.backends.characteristic import BleakGATTCharacteristic

# BLE UUIDs - Base UUID with service and characteristic IDs
# Base UUID: 9a599c29-6e67-5d0d-aab9-ad9126b66f91
# Format: 9a59{:04x}-6e67-5d0d-aab9-ad9126b66f91
BASE_UUID = "9a59{:04x}-6e67-5d0d-aab9-ad9126b66f91"

# Service and Characteristic UUIDs (from ble_chars_def.h)
FILE_TRANSFER_SERVICE = BASE_UUID.format(0x0300)  # BLE_SCOOTER_SERVICE_FILE_TRANSFER
FILE_TRANSFER_INIT = BASE_UUID.format(0x0301)     # BLE_SCOOTER_SERVICE_FILE_TRANSFER_INIT
FILE_TRANSFER_CHUNK = BASE_UUID.format(0x0302)    # BLE_SCOOTER_SERVICE_FILE_TRANSFER_CHUNK
FILE_TRANSFER_ACK = BASE_UUID.format(0x0303)      # BLE_SCOOTER_SERVICE_FILE_TRANSFER_ACK
FILE_TRANSFER_COMPLETE = BASE_UUID.format(0x0304) # BLE_SCOOTER_SERVICE_FILE_TRANSFER_COMPLETE
FILE_TRANSFER_ABORT = BASE_UUID.format(0x0305)    # BLE_SCOOTER_SERVICE_FILE_TRANSFER_ABORT
FILE_TRANSFER_STATUS = BASE_UUID.format(0x0306)   # BLE_SCOOTER_SERVICE_FILE_TRANSFER_STATUS

# Constants
# Base64 encoding increases size by 4/3, so 64-byte buffer / (4/3) = 48 bytes max binary
# 48 bytes - 2 bytes for sequence number = 46 bytes for actual data
CHUNK_SIZE = 46
MAX_FILE_SIZE = 10 * 1024 * 1024  # 10MB
DEVICE_NAME = "unu Scooter"
SCAN_TIMEOUT = 10.0


class FileTransferClient:
    def __init__(self, verbose: bool = False):
        self.verbose = verbose
        self.client: Optional[BleakClient] = None
        self.ack_received = asyncio.Event()
        self.ack_data = None
        self.transfer_id = None

    def log(self, message: str):
        if self.verbose:
            print(f"[{time.strftime('%H:%M:%S')}] {message}")

    async def find_scooter(self) -> Optional[str]:
        """Scan for the scooter device"""
        print("Scanning for scooter...")

        devices = await BleakScanner.discover(timeout=SCAN_TIMEOUT)

        for device in devices:
            self.log(f"Found device: {device.name} ({device.address})")
            if device.name and DEVICE_NAME in device.name:
                print(f"Found scooter: {device.name} at {device.address}")
                return device.address

        # If no device found with name, show all devices and let user choose
        if devices:
            print("\nNo scooter found by name. Available devices:")
            for i, device in enumerate(devices):
                name = device.name or "Unknown"
                print(f"{i+1}. {name} ({device.address})")

            try:
                choice = int(input("\nSelect device number (or 0 to cancel): "))
                if 1 <= choice <= len(devices):
                    return devices[choice-1].address
            except (ValueError, KeyboardInterrupt):
                pass

        return None

    def notification_handler(self, sender: BleakGATTCharacteristic, data: bytearray):
        """Handle notifications from the scooter"""
        self.log(f"Notification from {sender.uuid}: {data.hex()}")
        self.ack_data = data
        self.ack_received.set()

    async def connect(self, address: str) -> bool:
        """Connect to the scooter"""
        print(f"Connecting to {address}...")

        try:
            self.client = BleakClient(address)
            await self.client.connect()

            if not self.client.is_connected:
                print("Failed to connect")
                return False

            print("Connected!")

            # Subscribe to notifications
            await self.client.start_notify(FILE_TRANSFER_ACK, self.notification_handler)
            await self.client.start_notify(FILE_TRANSFER_STATUS, self.notification_handler)

            return True

        except Exception as e:
            print(f"Connection error: {e}")
            return False

    async def send_file(self, file_path: str) -> bool:
        """Send a file to the scooter"""
        path = Path(file_path)

        if not path.exists():
            print(f"File not found: {file_path}")
            return False

        file_size = path.stat().st_size

        if file_size > MAX_FILE_SIZE:
            print(f"File too large: {file_size} bytes (max {MAX_FILE_SIZE})")
            return False

        print(f"Sending file: {path.name} ({file_size} bytes)")

        # Read file data
        with open(path, 'rb') as f:
            file_data = f.read()

        # Calculate CRC32
        crc32 = zlib.crc32(file_data) & 0xFFFFFFFF

        # Step 1: Initialize transfer
        print("Initializing transfer...")

        # Build init message: size(4) + crc32(4) + filename
        # Encode as base64 to ensure UTF-8 safety through CBOR
        binary_data = struct.pack('<II', file_size, crc32) + path.name.encode('utf-8')
        init_data = base64.b64encode(binary_data)

        self.ack_received.clear()
        await self.client.write_gatt_char(FILE_TRANSFER_INIT, init_data)

        # Wait for ACK with transfer ID
        try:
            await asyncio.wait_for(self.ack_received.wait(), timeout=5.0)
            if self.ack_data:
                # Parse transfer ID from response
                self.transfer_id = self.ack_data.decode('utf-8', errors='ignore')
                print(f"Transfer initialized: {self.transfer_id}")
        except asyncio.TimeoutError:
            print("Timeout waiting for init ACK")
            return False

        # Step 2: Send chunks
        total_chunks = (file_size + CHUNK_SIZE - 1) // CHUNK_SIZE
        print(f"Sending {total_chunks} chunks...")

        for i in range(0, file_size, CHUNK_SIZE):
            chunk_num = i // CHUNK_SIZE
            chunk_end = min(i + CHUNK_SIZE, file_size)
            chunk_data = file_data[i:chunk_end]

            # Build chunk message: sequence(2) + data
            # Base64 encode to avoid null byte issues in CBOR string encoding
            binary_chunk = struct.pack('<H', chunk_num) + chunk_data
            chunk_msg = base64.b64encode(binary_chunk)

            # Send chunk
            self.ack_received.clear()
            await self.client.write_gatt_char(FILE_TRANSFER_CHUNK, chunk_msg)

            # Wait for ACK
            try:
                await asyncio.wait_for(self.ack_received.wait(), timeout=30.0)
            except asyncio.TimeoutError:
                print(f"Timeout waiting for chunk {chunk_num} ACK")
                return False

            # Progress bar
            progress = (chunk_num + 1) * 100 // total_chunks
            bar_length = 40
            filled = int(bar_length * (chunk_num + 1) / total_chunks)
            bar = '█' * filled + '░' * (bar_length - filled)
            print(f'\r[{bar}] {progress}% ({chunk_num + 1}/{total_chunks})', end='', flush=True)

        print()  # New line after progress bar

        # Step 3: Complete transfer
        print("Completing transfer...")
        # Just send CRC32 (4 bytes), base64 encoded to fit in 16-byte buffer
        complete_data = base64.b64encode(struct.pack('<I', crc32))

        self.ack_received.clear()
        await self.client.write_gatt_char(FILE_TRANSFER_COMPLETE, complete_data)

        # Wait for completion ACK
        try:
            await asyncio.wait_for(self.ack_received.wait(), timeout=5.0)
            print("Transfer completed successfully!")
            return True
        except asyncio.TimeoutError:
            print("Timeout waiting for completion ACK")
            return False

    async def abort_transfer(self, reason: str = "User cancelled"):
        """Abort the current transfer"""
        if self.transfer_id:
            abort_data = f"{self.transfer_id}:{reason}".encode('utf-8')
            await self.client.write_gatt_char(FILE_TRANSFER_ABORT, abort_data)
            print(f"Transfer aborted: {reason}")

    async def disconnect(self):
        """Disconnect from the scooter"""
        if self.client and self.client.is_connected:
            try:
                await self.client.stop_notify(FILE_TRANSFER_ACK)
                await self.client.stop_notify(FILE_TRANSFER_STATUS)
            except:
                pass
            await self.client.disconnect()
            print("Disconnected")


async def main():
    parser = argparse.ArgumentParser(description='Send files to electric scooter via Bluetooth')
    parser.add_argument('file', help='Path to file to send')
    parser.add_argument('-a', '--address', help='BLE address of scooter (skip scanning)')
    parser.add_argument('-v', '--verbose', action='store_true', help='Verbose output')

    args = parser.parse_args()

    # Check file exists
    if not os.path.exists(args.file):
        print(f"Error: File not found: {args.file}")
        sys.exit(1)

    client = FileTransferClient(verbose=args.verbose)

    try:
        # Find or use provided address
        address = args.address
        if not address:
            address = await client.find_scooter()
            if not address:
                print("No scooter found")
                sys.exit(1)

        # Connect
        if not await client.connect(address):
            sys.exit(1)

        # Send file
        success = await client.send_file(args.file)

        if not success:
            # Try to abort if transfer failed
            await client.abort_transfer("Transfer failed")
            sys.exit(1)

    except KeyboardInterrupt:
        print("\n\nInterrupted by user")
        await client.abort_transfer("User interrupted")

    except Exception as e:
        print(f"\nError: {e}")
        if args.verbose:
            import traceback
            traceback.print_exc()

    finally:
        await client.disconnect()


if __name__ == "__main__":
    asyncio.run(main())