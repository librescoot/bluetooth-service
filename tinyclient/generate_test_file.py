#!/usr/bin/env python3
"""
Generate test files for file transfer testing
"""

import os
import sys
import random
import string
import argparse


def generate_test_file(filename: str, size: int, pattern: str = "random"):
    """Generate a test file with specified size and pattern"""

    # Convert size string to bytes
    multipliers = {'K': 1024, 'M': 1024*1024, 'B': 1}

    if isinstance(size, str):
        unit = size[-1].upper()
        if unit in multipliers:
            size = int(size[:-1]) * multipliers[unit]
        else:
            size = int(size)

    print(f"Generating {filename} ({size:,} bytes) with pattern: {pattern}")

    with open(filename, 'wb') as f:
        remaining = size
        chunk_size = 4096

        while remaining > 0:
            current_chunk = min(chunk_size, remaining)

            if pattern == "random":
                # Random binary data
                data = os.urandom(current_chunk)
            elif pattern == "zeros":
                # All zeros
                data = b'\x00' * current_chunk
            elif pattern == "ones":
                # All ones
                data = b'\xFF' * current_chunk
            elif pattern == "text":
                # Readable text
                text = ''.join(random.choices(string.ascii_letters + string.digits + ' \n', k=current_chunk))
                data = text.encode('utf-8')[:current_chunk]
            elif pattern == "counter":
                # Sequential counter pattern
                data = bytes((i % 256) for i in range(current_chunk))
            else:
                print(f"Unknown pattern: {pattern}")
                sys.exit(1)

            f.write(data)
            remaining -= current_chunk

    # Show file info
    actual_size = os.path.getsize(filename)
    print(f"✅ Created: {filename}")
    print(f"   Size: {actual_size:,} bytes")

    # Calculate and show CRC32
    import zlib
    with open(filename, 'rb') as f:
        crc32 = zlib.crc32(f.read()) & 0xFFFFFFFF
    print(f"   CRC32: 0x{crc32:08X}")


def main():
    parser = argparse.ArgumentParser(description='Generate test files for transfer testing')
    parser.add_argument('filename', help='Output filename')
    parser.add_argument('size', help='File size (e.g., 1024, 1K, 5M)')
    parser.add_argument('-p', '--pattern',
                       choices=['random', 'zeros', 'ones', 'text', 'counter'],
                       default='random',
                       help='Data pattern to use (default: random)')

    args = parser.parse_args()

    try:
        generate_test_file(args.filename, args.size, args.pattern)
    except Exception as e:
        print(f"Error: {e}")
        sys.exit(1)


if __name__ == "__main__":
    main()