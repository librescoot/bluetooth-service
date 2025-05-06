package main

import (
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/librescoot/bluetooth-service/pkg/redis"
	"github.com/librescoot/bluetooth-service/pkg/service"
	"github.com/librescoot/bluetooth-service/pkg/usock"
)

// Configuration flags
var (
	serialDevice = flag.String("serial", "/dev/ttymxc1", "Serial device path")
	baudRate     = flag.Int("baud", 115200, "Serial baud rate")
	redisAddr    = flag.String("redis-addr", "localhost:6379", "Redis server address")
	redisPass    = flag.String("redis-pass", "", "Redis password")
	redisDB      = flag.Int("redis-db", 0, "Redis database number")
)

// Redis keys
const (
	KeyBatterySlot1      = "battery:0"
	KeyBatterySlot2      = "battery:1"
	KeyVehicle           = "vehicle"
	KeyPowerManager      = "power-manager"
	KeyMileage           = "engine-ecu"
	KeyFirmwareVersion   = "system"
	KeyBLEPairingPin     = "ble"
	KeyBLEStatus         = "ble"
	KeyBLECommand        = "ble"
)

// Battery state constants
const (
	BatteryStateUnknown = 0
	BatteryStateAsleep  = 1
	BatteryStateIdle    = 2
	BatteryStateActive  = 3
)

func main() {
	flag.Parse()

	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)
	log.Printf("Starting MDB Bluetooth Service")
	log.Printf("Serial device: %s", *serialDevice)
	log.Printf("Baud rate: %d", *baudRate)
	log.Printf("Redis address: %s", *redisAddr)

	redisClient, err := redis.New(*redisAddr, *redisPass, *redisDB)
	if err != nil {
		log.Fatalf("Failed to connect to Redis: %v", err)
	}
	defer redisClient.Close()
	log.Printf("Connected to Redis")

	svc := service.New(redisClient)

	usockHandler := func(payload *usock.Payload) {
		svc.HandleUSockMessage(payload.ID, payload)
	}
	sock, err := usock.New(*serialDevice, *baudRate, usockHandler)
	if err != nil {
		log.Fatalf("Failed to connect to nRF52 via USOCK: %v", err)
	}
	svc.SetUSock(sock)
	defer sock.Close()
	log.Printf("Connected to nRF52 via USOCK")

	// Start the command watcher goroutine
	go svc.WatchRedisCommands()

	// Subscribe to Redis Pub/Sub channels for state updates
	svc.SubscribeToRedisChannels()

	log.Printf("Subscribed to Redis channels")

	log.Printf("Initializing communication with nRF52...")
	if err := svc.InitializeNRF52(); err != nil {
		// Log the error but continue, initialization might partially succeed
		log.Printf("Error during nRF52 initialization sequence: %v", err)
	} else {
		log.Printf("nRF52 initialization sequence sent successfully.")
	}

	// Wait a bit for nRF52 to process initialization commands before sending state updates
	log.Printf("Waiting briefly before sending initial state updates...")
	time.Sleep(200 * time.Millisecond)

	log.Printf("Sending initial state updates...")

	// Update vehicle state
	if err := svc.UpdateVehicleState(); err != nil {
		log.Printf("Warning during initial update: %v", err)
	}
	// Update seatbox lock state
	if err := svc.UpdateSeatboxLock(); err != nil {
		log.Printf("Warning during initial update: %v", err)
	}
	// Update handlebar lock state
	if err := svc.UpdateHandlebarLock(); err != nil {
		log.Printf("Warning during initial update: %v", err)
	}
	// Update mileage
	if err := svc.UpdateMileage(); err != nil {
		log.Printf("Warning during initial update: %v", err)
	}
	// Update firmware version
	if err := svc.UpdateFirmwareVersion(); err != nil {
		log.Printf("Warning during initial update: %v", err)
	}
	// Update battery states for both slots
	for slot := 1; slot <= 2; slot++ {
		if err := svc.UpdateBatteryPresentStatus(slot); err != nil {
			log.Printf("Warning during initial update (Slot %d): %v", slot, err)
		}
		if err := svc.UpdateBatteryActiveStatus(slot); err != nil {
			log.Printf("Warning during initial update (Slot %d): %v", slot, err)
		}
		if err := svc.UpdateBatteryCycleCount(slot); err != nil {
			log.Printf("Warning during initial update (Slot %d): %v", slot, err)
		}
		if err := svc.UpdateBatteryRemainingCharge(slot); err != nil {
			log.Printf("Warning during initial update (Slot %d): %v", slot, err)
		}
	}
	// Update power management state
	if err := svc.UpdatePowerManagementState(); err != nil {
		log.Printf("Warning during initial update: %v", err)
	}
	log.Printf("Initial state updates sent.")

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	<-sigCh
	svc.Stop()
	log.Printf("Shutting down...")
}