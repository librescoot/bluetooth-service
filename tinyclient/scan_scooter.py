#!/usr/bin/env python3
"""
BLE Scanner for Electric Scooter
Finds and displays information about the scooter's BLE services
"""

import asyncio
import argparse
from bleak import BleakClient, BleakScanner

# BLE UUIDs
BASE_UUID = "0000{:04x}-1212-efde-1523-785feabcd123"
FILE_TRANSFER_SERVICE = BASE_UUID.format(0xF000)

KNOWN_SERVICES = {
    BASE_UUID.format(0x0000): "Power Management",
    BASE_UUID.format(0x0040): "Scooter Info",
    BASE_UUID.format(0x0060): "Battery Info",
    BASE_UUID.format(0x0080): "BLE Params",
    BASE_UUID.format(0x00A0): "Requests",
    BASE_UUID.format(0x00C0): "Scooter State",
    BASE_UUID.format(0x00E0): "Battery Status",
    BASE_UUID.format(0xF000): "File Transfer",
}


async def scan_devices():
    """Scan for all BLE devices"""
    print("Scanning for BLE devices...")
    devices = await BleakScanner.discover(timeout=10.0)

    if not devices:
        print("No devices found")
        return

    print(f"\nFound {len(devices)} devices:\n")
    for i, device in enumerate(devices, 1):
        name = device.name or "Unknown"
        rssi = device.rssi if hasattr(device, 'rssi') else "N/A"
        print(f"{i:2}. {name:30} {device.address:20} RSSI: {rssi}")

    return devices


async def inspect_device(address: str):
    """Inspect services and characteristics of a BLE device"""
    print(f"\nConnecting to {address}...")

    try:
        async with BleakClient(address) as client:
            if not client.is_connected:
                print("Failed to connect")
                return

            print("Connected!\n")
            print("=" * 60)
            print("SERVICES AND CHARACTERISTICS")
            print("=" * 60)

            # List all services
            for service in client.services:
                service_name = KNOWN_SERVICES.get(service.uuid, "Unknown Service")
                print(f"\n📁 Service: {service_name}")
                print(f"   UUID: {service.uuid}")

                # List characteristics for each service
                for char in service.characteristics:
                    properties = []
                    if "read" in char.properties:
                        properties.append("READ")
                    if "write" in char.properties:
                        properties.append("WRITE")
                    if "notify" in char.properties:
                        properties.append("NOTIFY")

                    props_str = ", ".join(properties)
                    print(f"   └─ Char: {char.uuid[-4:]} [{props_str}]")

                    # Try to read if readable
                    if "read" in char.properties:
                        try:
                            value = await client.read_gatt_char(char)
                            # Try to decode as string, otherwise show hex
                            try:
                                decoded = value.decode('utf-8').strip('\x00')
                                if decoded and decoded.isprintable():
                                    print(f"      Value: '{decoded}'")
                                else:
                                    print(f"      Value: {value.hex()}")
                            except:
                                print(f"      Value: {value.hex()}")
                        except Exception as e:
                            print(f"      Value: <error reading: {e}>")

            # Check for File Transfer service specifically
            print("\n" + "=" * 60)
            if FILE_TRANSFER_SERVICE in [s.uuid for s in client.services]:
                print("✅ FILE TRANSFER SERVICE FOUND!")
                print("   The scooter supports file transfers")
            else:
                print("❌ File Transfer service not found")
                print("   The scooter may not support file transfers")

            print("=" * 60)

    except Exception as e:
        print(f"Error: {e}")


async def main():
    parser = argparse.ArgumentParser(description='Scan for electric scooter BLE services')
    parser.add_argument('-a', '--address', help='BLE address to inspect directly')

    args = parser.parse_args()

    try:
        if args.address:
            # Inspect specific device
            await inspect_device(args.address)
        else:
            # Scan and let user choose
            devices = await scan_devices()
            if devices:
                print("\n" + "=" * 60)
                try:
                    choice = int(input("Select device number to inspect (0 to exit): "))
                    if 1 <= choice <= len(devices):
                        await inspect_device(devices[choice-1].address)
                except (ValueError, KeyboardInterrupt):
                    print("\nExiting...")

    except KeyboardInterrupt:
        print("\n\nInterrupted by user")

    except Exception as e:
        print(f"\nError: {e}")


if __name__ == "__main__":
    asyncio.run(main())